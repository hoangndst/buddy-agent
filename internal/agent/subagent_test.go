package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hoangndst/buddy-agent/internal/history"
	"github.com/hoangndst/buddy-agent/internal/persistence"
	"github.com/hoangndst/buddy-agent/internal/tools"
	"github.com/openai/openai-go"
)

type dummyTestTool struct {
	name string
}

func (d *dummyTestTool) Name() string        { return d.name }
func (d *dummyTestTool) Description() string { return d.name }
func (d *dummyTestTool) Parameters() any     { return nil }
func (d *dummyTestTool) Execute(_ context.Context, _ json.RawMessage) (string, error) {
	return "ok", nil
}

func TestSubagentTool_RoleGating(t *testing.T) {
	reg := tools.NewRegistry(1000)
	reg.Register(&dummyTestTool{name: "read_file"})
	reg.Register(&dummyTestTool{name: "write_file"})
	reg.Register(&dummyTestTool{name: "edit_file"})
	reg.Register(&dummyTestTool{name: "bash"})
	reg.Register(&dummyTestTool{name: "save_memory"})
	reg.Register(&dummyTestTool{name: "create_skill"})
	reg.Register(&dummyTestTool{name: "invoke_subagent"})
	reg.Register(&dummyTestTool{name: "mcp_list_files"})

	subTool := NewInvokeSubagentTool(SubagentToolConfig{
		Depth:          0,
		ParentRegistry: reg,
	})

	// 1. Test researcher registry
	resReg := subTool.buildChildRegistry("researcher")
	if _, ok := resReg.Get("read_file"); !ok {
		t.Errorf("expected researcher to have read_file")
	}
	if _, ok := resReg.Get("mcp_list_files"); !ok {
		t.Errorf("expected researcher to have mcp_list_files")
	}
	for _, forbidden := range []string{"bash", "write_file", "edit_file", "invoke_subagent", "save_memory", "create_skill"} {
		if _, ok := resReg.Get(forbidden); ok {
			t.Errorf("researcher role MUST NOT have %q", forbidden)
		}
	}

	// 2. Test worker registry
	workerReg := subTool.buildChildRegistry("worker")
	for _, allowed := range []string{"read_file", "write_file", "edit_file", "bash", "mcp_list_files"} {
		if _, ok := workerReg.Get(allowed); !ok {
			t.Errorf("worker expected to have %q", allowed)
		}
	}
	for _, forbidden := range []string{"invoke_subagent", "save_memory", "create_skill"} {
		if _, ok := workerReg.Get(forbidden); ok {
			t.Errorf("worker role MUST NOT have %q", forbidden)
		}
	}
}

func TestSubagentTool_DepthCeiling(t *testing.T) {
	// An engine running at depth=1 must be barred from delegating
	subTool := NewInvokeSubagentTool(SubagentToolConfig{
		Depth: 1,
	})

	args, _ := json.Marshal(InvokeSubagentArgs{Task: "Explore deeper"})
	_, err := subTool.Execute(context.Background(), args)
	if err == nil {
		t.Fatalf("expected error delegating from depth=1, got nil")
	}
	if !strings.Contains(err.Error(), "depth=1 ceiling enforced") {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestSubagentTool_HorizontalThrottling(t *testing.T) {
	subTool := NewInvokeSubagentTool(SubagentToolConfig{
		Depth:    0,
		MaxCalls: 2,
	})

	// Manually simulate reaching maxCalls
	subTool.callCount = 2
	args, _ := json.Marshal(InvokeSubagentArgs{Task: "Another subtask"})
	res, err := subTool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("expected structured status string, got err: %v", err)
	}
	if !strings.Contains(res, "maximum subagent delegation quota reached") {
		t.Errorf("expected quota reached error message, got: %s", res)
	}
}

func TestSubagentTool_ExecutionAndRollup(t *testing.T) {
	tempDir := t.TempDir()
	store, err := persistence.NewSQLiteStore(fmt.Sprintf("file:%s/test.db?mode=memory&cache=shared", tempDir))
	if err != nil {
		t.Fatalf("failed to create memory store: %v", err)
	}
	defer func() { _ = store.Close() }()

	ctx := context.Background()
	parentSessID := "sess_parent_exec_test"
	_ = store.CreateSession(ctx, &persistence.SessionRecord{
		ID:         parentSessID,
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
		Status:     persistence.StatusRunning,
		Task:       "Parent task",
		Model:      "gpt-4o",
		WorkingDir: tempDir,
		Role:       "root",
		Depth:      0,
	})

	mockProv := &mockChatClient{
		responses: []*openai.ChatCompletion{
			{
				Choices: []openai.ChatCompletionChoice{
					{
						Message: openai.ChatCompletionMessage{
							Role:    "assistant",
							Content: "Subagent research complete: all clear.",
						},
					},
				},
				Usage: openai.CompletionUsage{
					PromptTokens:     120,
					CompletionTokens: 30,
					TotalTokens:      150,
				},
			},
		},
	}

	parentTracker := history.NewStateTracker("Parent Goal", []string{"pure Go zero CGO"})
	parentReg := tools.NewRegistry(1000)
	parentReg.Register(&dummyTestTool{name: "read_file"})

	subTool := NewInvokeSubagentTool(SubagentToolConfig{
		Depth:           0,
		ParentSessionID: parentSessID,
		ParentTracker:   parentTracker,
		ParentRegistry:  parentReg,
		Provider:        mockProv,
		Model:           "gpt-4o",
		WorkspaceDir:    tempDir,
		Store:           store,
	})

	args, _ := json.Marshal(InvokeSubagentArgs{
		Task: "Inspect code files",
		Role: "researcher",
	})

	result, err := subTool.Execute(ctx, args)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if !strings.Contains(result, "Status: success") {
		t.Errorf("expected success status, got: %s", result)
	}
	if !strings.Contains(result, "Subagent research complete") {
		t.Errorf("expected summary text in result, got: %s", result)
	}

	// Verify child session in SQLite
	children, err := store.GetChildSessions(ctx, parentSessID)
	if err != nil {
		t.Fatalf("GetChildSessions failed: %v", err)
	}
	if len(children) != 1 {
		t.Fatalf("expected 1 child session in SQLite, got %d", len(children))
	}
	child := children[0]
	if child.Role != "researcher" || child.Depth != 1 {
		t.Errorf("expected child role 'researcher' and depth 1, got %+v", child)
	}

	// Verify token usage rolled up to parent session record
	parentRecord, err := store.GetSession(ctx, parentSessID)
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}
	if parentRecord.TokenUsage.TotalTokens != 150 {
		t.Errorf("expected 150 total tokens rolled up to parent, got %d", parentRecord.TokenUsage.TotalTokens)
	}
}

func TestSubagentTool_PanicRecovery(t *testing.T) {
	subTool := NewInvokeSubagentTool(SubagentToolConfig{
		Depth:    0,
		Model:    "gpt-4o",
		Provider: nil, // Calling Run on nil provider will cause a panic
	})

	args, _ := json.Marshal(InvokeSubagentArgs{Task: "Trigger panic"})
	res, err := subTool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("expected panic to be recovered into status string, got err: %v", err)
	}
	if !strings.Contains(res, "Status: failed") || !strings.Contains(res, "subagent panicked") {
		t.Errorf("expected caught panic message, got: %s", res)
	}
}

func TestEngine_ReserveStepSynthesisOnMaxSteps(t *testing.T) {
	// Test that reaching maxSteps triggers the emergency reserve synthesis prompt
	// instead of discarding context and failing with an empty string.
	mockProv := &mockChatClient{
		responses: []*openai.ChatCompletion{
			// Step 1: Model calls a tool
			{
				Choices: []openai.ChatCompletionChoice{
					{
						Message: openai.ChatCompletionMessage{
							Role: "assistant",
							ToolCalls: []openai.ChatCompletionMessageToolCall{
								{
									ID:   "call_1",
									Type: "function",
									Function: openai.ChatCompletionMessageToolCallFunction{
										Name:      "dummy",
										Arguments: `{}`,
									},
								},
							},
						},
					},
				},
			},
			// Step 2: Emergency reserve synthesis without tools
			{
				Choices: []openai.ChatCompletionChoice{
					{
						Message: openai.ChatCompletionMessage{
							Role:    "assistant",
							Content: "Synthesized partial findings: examined 1 file.",
						},
					},
				},
				Usage: openai.CompletionUsage{
					TotalTokens: 50,
				},
			},
		},
	}

	reg := tools.NewRegistry(1000)
	reg.Register(&dummyTestTool{name: "dummy"})

	engine := NewEngine(
		mockProv,
		"gpt-4o",
		1, // MaxSteps = 1: immediately triggers reserve synthesis on step 2
		reg,
		nil,
		nil,
		WithSystemPromptOverride("You are a test subagent."),
		WithDepth(1),
	)

	output, err := engine.Run(context.Background(), "Run one step and synthesize")
	if err != nil {
		t.Fatalf("expected emergency synthesis to return partial output, got err: %v", err)
	}
	if !strings.Contains(output, "[Subagent: budget_exhausted]") {
		t.Errorf("expected budget_exhausted prefix in output, got: %s", output)
	}
	if !strings.Contains(output, "Synthesized partial findings") {
		t.Errorf("expected partial findings in output, got: %s", output)
	}
}
