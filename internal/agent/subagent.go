package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hoangndst/buddy-agent/internal/agent/provider"
	"github.com/hoangndst/buddy-agent/internal/history"
	"github.com/hoangndst/buddy-agent/internal/persistence"
	"github.com/hoangndst/buddy-agent/internal/tools"
)

// DefaultMaxSubagentCalls defines the maximum number of subagent invocations per root session.
const DefaultMaxSubagentCalls = 5

// DefaultChildTimeout defines the execution deadline for child subagents.
const DefaultChildTimeout = 120 * time.Second

// InvokeSubagentArgs defines the parameters expected by InvokeSubagentTool.
type InvokeSubagentArgs struct {
	Task     string `json:"task"`
	Role     string `json:"role,omitempty"`
	MaxSteps int    `json:"max_steps,omitempty"`
}

// InvokeSubagentTool enables an agent to delegate an isolated sub-task to a child agent.
type InvokeSubagentTool struct {
	mu              sync.Mutex
	depth           int
	callCount       int
	maxCalls        int
	childUsage      persistence.TokenUsage
	parentSessionID string
	parentTracker   *history.StateTracker
	parentRegistry  *tools.Registry
	provider        provider.ModelProvider
	model           string
	workspaceDir    string
	reasoningEffort string
	compClient      history.CompletionClient
	store           persistence.Store
	eventHandler    EventHandler
	isLocal         bool
	maxContextTokens int
}

// SubagentToolConfig specifies dependencies required to instantiate InvokeSubagentTool.
type SubagentToolConfig struct {
	Depth           int
	ParentSessionID string
	ParentTracker   *history.StateTracker
	ParentRegistry  *tools.Registry
	Provider        provider.ModelProvider
	Model           string
	WorkspaceDir    string
	ReasoningEffort string
	CompClient      history.CompletionClient
	Store           persistence.Store
	EventHandler    EventHandler
	MaxCalls        int
	IsLocal         bool
	MaxContextTokens int
}

// NewInvokeSubagentTool creates a new InvokeSubagentTool.
func NewInvokeSubagentTool(cfg SubagentToolConfig) *InvokeSubagentTool {
	maxCalls := cfg.MaxCalls
	if maxCalls <= 0 {
		maxCalls = DefaultMaxSubagentCalls
	}
	return &InvokeSubagentTool{
		depth:           cfg.Depth,
		maxCalls:        maxCalls,
		parentSessionID: cfg.ParentSessionID,
		parentTracker:   cfg.ParentTracker,
		parentRegistry:  cfg.ParentRegistry,
		provider:        cfg.Provider,
		model:           cfg.Model,
		workspaceDir:    cfg.WorkspaceDir,
		reasoningEffort: cfg.ReasoningEffort,
		compClient:      cfg.CompClient,
		store:           cfg.Store,
		eventHandler:    cfg.EventHandler,
		isLocal:         cfg.IsLocal,
		maxContextTokens: cfg.MaxContextTokens,
	}
}

// Name returns the identifier of the tool.
func (t *InvokeSubagentTool) Name() string {
	return "invoke_subagent"
}

// Description describes the tool's purpose and usage for the LLM.
func (t *InvokeSubagentTool) Description() string {
	return "Delegates a specialized sub-task to an isolated child agent (researcher or worker) with its own context window. Returns a synthesized summary of findings."
}

// Parameters returns the JSON Schema for the tool's arguments.
func (t *InvokeSubagentTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task": map[string]any{
				"type":        "string",
				"description": "The specific sub-task, question, or research assignment for the subagent to complete.",
			},
			"role": map[string]any{
				"type":        "string",
				"enum":        []string{"researcher", "worker"},
				"description": "Role profile: 'researcher' has read-only file/MCP access without bash; 'worker' has full file and bash access.",
				"default":     "researcher",
			},
			"max_steps": map[string]any{
				"type":        "integer",
				"description": "Maximum number of steps for the subagent (default: 10, max: 15).",
				"default":     10,
			},
		},
		"required": []string{"task"},
	}
}

// SetParentSessionID sets or updates the parent session ID.
func (t *InvokeSubagentTool) SetParentSessionID(id string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.parentSessionID = id
}

// SetParentTracker sets or updates the parent state tracker.
func (t *InvokeSubagentTool) SetParentTracker(tracker *history.StateTracker) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.parentTracker = tracker
}

// Reset clears session-scoped delegation count and accumulated child token usage.
func (t *InvokeSubagentTool) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.callCount = 0
	t.childUsage = persistence.TokenUsage{}
}

// SetCallCount sets the current number of subagent invocations (used during session resume).
func (t *InvokeSubagentTool) SetCallCount(count int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.callCount = count
}

// CallCount returns the current count of subagent invocations.
func (t *InvokeSubagentTool) CallCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.callCount
}

// ChildUsage returns the cumulative token usage of all child subagents executed by this tool.
func (t *InvokeSubagentTool) ChildUsage() persistence.TokenUsage {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.childUsage
}

// Execute runs the delegated subagent task synchronously with panic isolation and budget bounds.
func (t *InvokeSubagentTool) Execute(ctx context.Context, argsJSON json.RawMessage) (string, error) {
	t.mu.Lock()
	// 1. Enforce strict depth ceiling: subagents cannot delegate
	if t.depth > 0 {
		t.mu.Unlock()
		return "", errors.New("subagents are not permitted to spawn further subagents (depth=1 ceiling enforced)")
	}

	// 2. Horizontal throttling: cap invocations per session
	if t.callCount >= t.maxCalls {
		t.mu.Unlock()
		return fmt.Sprintf("Status: failed\nError: maximum subagent delegation quota reached (%d/%d). You must synthesize existing findings and complete the task directly.", t.maxCalls, t.maxCalls), nil
	}
	t.callCount++
	parentSessionID := t.parentSessionID
	parentTracker := t.parentTracker
	t.mu.Unlock()

	// 3. Parse and validate arguments
	var args InvokeSubagentArgs
	if err := json.Unmarshal(argsJSON, &args); err != nil {
		return "", fmt.Errorf("invalid arguments for invoke_subagent: %w", err)
	}

	task := strings.TrimSpace(args.Task)
	if task == "" {
		return "", errors.New("task cannot be empty")
	}

	role := strings.ToLower(strings.TrimSpace(args.Role))
	if role != "worker" {
		role = "researcher"
	}

	maxSteps := args.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 10
	} else if maxSteps > 15 {
		maxSteps = 15
	}

	// 4. Build role-gated child tool registry via CloneWithFilter
	childRegistry := t.buildChildRegistry(role)

	// 5. Setup invariant inheritance from parent tracker
	var inheritedInvariants []string
	if parentTracker != nil {
		data := parentTracker.ExportData()
		inheritedInvariants = data.Invariants
	}

	// 6. Build isolated non-interactive system prompt with inherited constraints
	subagentPrompt := t.buildSubagentSystemPrompt(role, task, inheritedInvariants)

	// 7. Scoped event handler for indented terminal output
	wrappedHandler := func(ev Event) {
		if t.eventHandler == nil {
			return
		}
		switch ev.Type {
		case EventToolCall:
			t.eventHandler(Event{
				Type:       ev.Type,
				Step:       ev.Step,
				Timestamp:  ev.Timestamp,
				ToolCallID: ev.ToolCallID,
				ToolName:   ev.ToolName,
				ToolArgs:   ev.ToolArgs,
				Content:    fmt.Sprintf("  [subagent:%s] %s", role, ev.Content),
			})
		case EventToolResult:
			t.eventHandler(Event{
				Type:         ev.Type,
				Step:         ev.Step,
				Timestamp:    ev.Timestamp,
				ToolCallID:   ev.ToolCallID,
				ToolName:     ev.ToolName,
				ToolResult:   ev.ToolResult,
				ToolExitCode: ev.ToolExitCode,
				Content:      fmt.Sprintf("  [subagent:%s] %s", role, ev.Content),
			})
		case EventError:
			t.eventHandler(Event{
				Type:      ev.Type,
				Step:      ev.Step,
				Timestamp: ev.Timestamp,
				Error:     fmt.Errorf("[subagent:%s] %w", role, ev.Error),
			})
		}
	}

	// 8. Derive child context with timeout deadline
	childCtx, cancel := context.WithTimeout(ctx, DefaultChildTimeout)
	defer cancel()

	// 9. Instantiate child engine with depth=1 and local context propagation
	childEngine := NewEngine(
		t.provider,
		t.model,
		maxSteps,
		childRegistry,
		nil, // No MemoryManager for subagents
		wrappedHandler,
		WithDepth(1),
		WithParentSessionID(parentSessionID),
		WithRole(role),
		WithSystemPromptOverride(subagentPrompt),
		WithInheritedInvariants(inheritedInvariants),
		WithStore(t.store),
		WithWorkspaceDir(t.workspaceDir),
		WithCompletionClient(t.compClient),
		WithReasoningEffort(t.reasoningEffort),
		WithIsLocal(t.isLocal),
		WithMaxContextTokens(t.maxContextTokens),
	)

	// 10. Execute child engine inside panic isolation cushion
	var (
		summary  string
		childErr error
	)
	func() {
		defer func() {
			if r := recover(); r != nil {
				childErr = fmt.Errorf("subagent panicked: %v", r)
			}
		}()
		summary, childErr = childEngine.Run(childCtx, task)
	}()

	// 11. State & Metric Rollup (always collect tokens, even on error or timeout)
	execResult := childEngine.LastExecutionResult()
	if execResult != nil {
		t.mu.Lock()
		t.childUsage.PromptTokens += execResult.TokenUsage.PromptTokens
		t.childUsage.CompletionTokens += execResult.TokenUsage.CompletionTokens
		t.childUsage.ReasoningTokens += execResult.TokenUsage.ReasoningTokens
		t.childUsage.TotalTokens += execResult.TokenUsage.TotalTokens
		t.mu.Unlock()

		// A. Token usage billing rollup to parent session record
		if t.store != nil && parentSessionID != "" {
			_ = t.store.IncrementSessionTokens(context.Background(), parentSessionID, execResult.TokenUsage)
		}

		// B. State tracker modified files rollup for successful worker subagents
		if role == "worker" && childErr == nil && parentTracker != nil {
			for path, rec := range execResult.ModifiedFiles {
				parentTracker.RecordFileAccess(path, rec.Action)
			}
		}
	}

	// 12. Handle child cancellation and timeout differentiation
	if ctx.Err() != nil {
		// Parent context was canceled (e.g. user SIGINT)
		return "", ctx.Err()
	}

	if errors.Is(childCtx.Err(), context.DeadlineExceeded) {
		return fmt.Sprintf("Status: budget_exhausted\nError: subagent exceeded execution timeout (%v)\nPartial output: %s", DefaultChildTimeout, summary), nil
	}

	if childErr != nil {
		if strings.Contains(summary, "[Subagent: budget_exhausted]") {
			return fmt.Sprintf("Status: budget_exhausted\n%s", summary), nil
		}
		return fmt.Sprintf("Status: failed\nError: %v\nOutput: %s", childErr, summary), nil
	}
	if strings.Contains(summary, "[Subagent: budget_exhausted]") {
		return fmt.Sprintf("Status: budget_exhausted\n%s", summary), nil
	}

	return fmt.Sprintf("Status: success\nRole: %s\nSummary:\n%s", role, summary), nil
}

// buildChildRegistry constructs a filtered tools.Registry based on the assigned role.
func (t *InvokeSubagentTool) buildChildRegistry(role string) *tools.Registry {
	if t.parentRegistry == nil {
		return tools.NewRegistry(10000)
	}

	return t.parentRegistry.CloneWithFilter(func(tool tools.Tool) bool {
		name := tool.Name()

		// Always bar recursive delegation and administrative persistence mutation
		if name == "invoke_subagent" || name == "save_memory" || name == "create_skill" {
			return false
		}

		// Researcher role is strictly read-only:
		// - Native tools: only read_file is allowed (no bash, write_file, edit_file)
		// - MCP tools: only verified read-only inspection tools are allowed; mutating or unknown MCP tools are blocked
		if role == "researcher" {
			if name == "read_file" {
				return true
			}
			if strings.HasPrefix(name, "mcp_") {
				return isReadOnlyMCPTool(name)
			}
			return false
		}

		return true
	})
}

// isReadOnlyMCPTool validates whether an MCP tool is a safe read-only inspection tool.
func isReadOnlyMCPTool(name string) bool {
	// Standard built-in MCP resource inspection tools
	if name == "mcp_list_resources" || name == "mcp_read_resource" {
		return true
	}
	if !strings.HasPrefix(name, "mcp_") {
		return false
	}

	lower := strings.ToLower(name)

	// Block mutating actions immediately
	mutatingKeywords := []string{
		"create", "write", "edit", "update", "delete", "remove", "drop",
		"insert", "post", "put", "patch", "modify", "exec", "execute",
		"run", "apply", "set", "upload", "push", "kill", "restart",
	}
	for _, kw := range mutatingKeywords {
		if strings.Contains(lower, kw) {
			return false
		}
	}

	// Must match recognized read-only query/inspection verbs
	readOnlyKeywords := []string{
		"read", "list", "get", "search", "fetch", "find", "query",
		"inspect", "describe", "view", "show", "cat", "info", "status",
	}
	for _, kw := range readOnlyKeywords {
		if strings.Contains(lower, kw) {
			return true
		}
	}

	return false
}

// buildSubagentSystemPrompt constructs a concise non-interactive prompt for subagents.
func (t *InvokeSubagentTool) buildSubagentSystemPrompt(role, task string, invariants []string) string {
	var roleRules string
	if role == "researcher" {
		roleRules = "- You have read-only access. You can read files and inspect project structure, but cannot run shell commands or modify files."
	} else {
		roleRules = "- You have full development access to write code, edit files, and execute shell commands to accomplish the sub-task."
	}

	var invSection string
	if len(invariants) > 0 {
		var sb strings.Builder
		sb.WriteString("\nInherited Invariants (Strict Constraints):\n")
		for _, inv := range invariants {
			sb.WriteString(fmt.Sprintf("- %s\n", inv))
		}
		invSection = sb.String()
	}

	return fmt.Sprintf(`You are a specialized subagent operating under the supervision of a primary agent.
Your role: %s
Your assigned task: %s
%s
Operational Directives:
%s
- Focus strictly on completing the assigned task using your available tools.
- Do not use conversational filler, pleasantries, or ask clarifying questions.
- When finished, provide a concise, factual synthesis of your findings, actions taken, and touched files.
- If you encounter blockers or cannot complete the task, state the exact reason clearly.`, role, task, invSection, roleRules)
}
