package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hoangndst/buddy-agent/internal/history"
	"github.com/hoangndst/buddy-agent/internal/memory"
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
	reg.Register(&dummyTestTool{name: "list_directory"})
	reg.Register(&dummyTestTool{name: "search_files"})
	reg.Register(&dummyTestTool{name: "write_file"})
	reg.Register(&dummyTestTool{name: "edit_file"})
	reg.Register(&dummyTestTool{name: "bash"})
	reg.Register(&dummyTestTool{name: "save_memory"})
	reg.Register(&dummyTestTool{name: "create_skill"})
	reg.Register(&dummyTestTool{name: "invoke_subagent"})
	reg.Register(&dummyTestTool{name: "mcp_list_files"})
	reg.Register(&dummyTestTool{name: "mcp_postgres-db_list_tables"})
	reg.Register(&dummyTestTool{name: "mcp_create_issue"})
	reg.Register(&dummyTestTool{name: "mcp_write_data"})
	reg.Register(&dummyTestTool{name: "mcp_unknown_action"})

	subTool := NewInvokeSubagentTool(SubagentToolConfig{
		Depth:          0,
		ParentRegistry: reg,
	})

	// 1. Test researcher registry
	resReg := subTool.buildChildRegistry("researcher")
	if _, ok := resReg.Get("read_file"); !ok {
		t.Errorf("expected researcher to have read_file")
	}
	if _, ok := resReg.Get("list_directory"); !ok {
		t.Errorf("expected researcher to have list_directory")
	}
	if _, ok := resReg.Get("search_files"); !ok {
		t.Errorf("expected researcher to have search_files")
	}
	if _, ok := resReg.Get("mcp_list_files"); !ok {
		t.Errorf("expected researcher to have mcp_list_files")
	}
	if _, ok := resReg.Get("mcp_postgres-db_list_tables"); !ok {
		t.Errorf("expected researcher to have mcp_postgres-db_list_tables")
	}
	for _, forbidden := range []string{"bash", "write_file", "edit_file", "invoke_subagent", "save_memory", "create_skill", "mcp_create_issue", "mcp_write_data", "mcp_unknown_action"} {
		if _, ok := resReg.Get(forbidden); ok {
			t.Errorf("researcher role MUST NOT have %q", forbidden)
		}
	}

	// 2. Test worker registry
	workerReg := subTool.buildChildRegistry("worker")
	for _, allowed := range []string{"read_file", "list_directory", "search_files", "write_file", "edit_file", "bash", "mcp_list_files", "mcp_create_issue", "mcp_write_data", "mcp_unknown_action"} {
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

func TestEngine_RejectChildSessionResume(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := persistence.NewSQLiteStore(fmt.Sprintf("file:memdb_reject_child_%d?mode=memory&cache=shared&_pragma=busy_timeout(5000)", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer func() { _ = store.Close() }()

	ctx := context.Background()
	parentID := "parent-sess-1"
	childID := "child-sess-1"

	// Create root session
	rootRec := &persistence.SessionRecord{
		ID:         parentID,
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
		Status:     persistence.StatusInterrupted,
		Task:       "Root task",
		Model:      "gpt-4o",
		WorkingDir: tmpDir,
		MaxSteps:   5,
		Role:       "root",
		Depth:      0,
	}
	if err := store.CreateSession(ctx, rootRec); err != nil {
		t.Fatalf("failed to create root session: %v", err)
	}

	// Create child session
	childRec := &persistence.SessionRecord{
		ID:              childID,
		CreatedAt:       time.Now().UTC(),
		UpdatedAt:       time.Now().UTC(),
		Status:          persistence.StatusInterrupted,
		Task:            "Child sub-task",
		Model:           "gpt-4o",
		WorkingDir:      tmpDir,
		MaxSteps:        5,
		ParentSessionID: &parentID,
		Role:            "researcher",
		Depth:           1,
	}
	if err := store.CreateSession(ctx, childRec); err != nil {
		t.Fatalf("failed to create child session: %v", err)
	}

	memMgr := memory.NewManager(tmpDir)
	eng := NewEngine(
		&mockChatClient{},
		"gpt-4o",
		5,
		tools.NewRegistry(1000),
		memMgr,
		nil,
		WithStore(store),
		WithWorkspaceDir(tmpDir),
	)

	_, err = eng.Resume(ctx, childID, 0)
	if err == nil {
		t.Fatal("expected error resuming child session directly, got nil")
	}
	if !strings.Contains(err.Error(), "cannot directly resume child subagent session") {
		t.Errorf("expected error to mention child subagent session restriction, got: %v", err)
	}
}

func TestEngine_ResumeRestoresDelegationQuota(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := persistence.NewSQLiteStore(fmt.Sprintf("file:memdb_quota_resume_%d?mode=memory&cache=shared&_pragma=busy_timeout(5000)", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	defer func() { _ = store.Close() }()

	ctx := context.Background()
	parentID := "parent-quota-sess"

	// Create root session
	rootRec := &persistence.SessionRecord{
		ID:         parentID,
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
		Status:     persistence.StatusInterrupted,
		Task:       "Parent task with 5 subagents",
		Model:      "gpt-4o",
		WorkingDir: tmpDir,
		MaxSteps:   10,
		Role:       "root",
		Depth:      0,
	}
	if err := store.CreateSession(ctx, rootRec); err != nil {
		t.Fatalf("failed to create root session: %v", err)
	}

	// Insert 1 turn in root session so resume can reconstruct trailing turns
	turnRec := &persistence.TurnRecord{
		SessionID:  parentID,
		StepNumber: 1,
		Role:       "assistant",
		Content:    "Ran initial step",
		CreatedAt:  time.Now().UTC(),
	}
	if err := store.CommitTurn(ctx, turnRec, 1, persistence.TokenUsage{PromptTokens: 10, TotalTokens: 10}); err != nil {
		t.Fatalf("failed to commit turn: %v", err)
	}

	// Insert 5 child sessions for this parent
	for i := 1; i <= 5; i++ {
		cID := fmt.Sprintf("child-%d", i)
		cRec := &persistence.SessionRecord{
			ID:              cID,
			CreatedAt:       time.Now().UTC().Add(time.Duration(i) * time.Second),
			UpdatedAt:       time.Now().UTC().Add(time.Duration(i) * time.Second),
			Status:          persistence.StatusCompleted,
			Task:            fmt.Sprintf("Child task %d", i),
			Model:           "gpt-4o",
			WorkingDir:      tmpDir,
			MaxSteps:        5,
			ParentSessionID: &parentID,
			Role:            "researcher",
			Depth:           1,
		}
		if err := store.CreateSession(ctx, cRec); err != nil {
			t.Fatalf("failed to create child session %d: %v", i, err)
		}
	}

	reg := tools.NewRegistry(1000)
	subTool := NewInvokeSubagentTool(SubagentToolConfig{
		Depth: 0,
		Store: store,
	})
	reg.Register(subTool)

	mockClient := &mockChatClient{
		responses: []*openai.ChatCompletion{
			{
				Choices: []openai.ChatCompletionChoice{
					{
						Message: openai.ChatCompletionMessage{
							Role:    "assistant",
							Content: "Resume completed directly",
						},
					},
				},
				Usage: openai.CompletionUsage{TotalTokens: 20},
			},
		},
	}

	memMgr := memory.NewManager(tmpDir)
	eng := NewEngine(
		mockClient,
		"gpt-4o",
		5,
		reg,
		memMgr,
		nil,
		WithStore(store),
		WithWorkspaceDir(tmpDir),
	)

	_, err = eng.Resume(ctx, parentID, 0)
	if err != nil {
		t.Fatalf("unexpected error resuming session: %v", err)
	}

	// Verify call count was restored to 5 from the 5 persisted child sessions
	if subTool.CallCount() != 5 {
		t.Errorf("expected subTool.CallCount() == 5, got %d", subTool.CallCount())
	}

	// Attempting a 6th subagent execution must now fail with quota exhaustion
	res, err := subTool.Execute(ctx, json.RawMessage(`{"task": "sub-task 6"}`))
	if err != nil {
		t.Fatalf("unexpected tool error: %v", err)
	}
	if !strings.Contains(strings.ToLower(res), "maximum subagent delegation quota reached (5/5)") {
		t.Errorf("expected quota exhaustion message, got: %s", res)
	}
}

func TestSubagentTool_ChildUsageTracking(t *testing.T) {
	subTool := NewInvokeSubagentTool(SubagentToolConfig{
		Depth: 0,
	})

	if subTool.ChildUsage().TotalTokens != 0 {
		t.Errorf("expected 0 initial child tokens, got %d", subTool.ChildUsage().TotalTokens)
	}

	subTool.mu.Lock()
	subTool.childUsage.PromptTokens += 30
	subTool.childUsage.CompletionTokens += 20
	subTool.childUsage.TotalTokens += 50
	subTool.mu.Unlock()

	usage := subTool.ChildUsage()
	if usage.PromptTokens != 30 || usage.CompletionTokens != 20 || usage.TotalTokens != 50 {
		t.Errorf("unexpected child usage: %+v", usage)
	}

	subTool.Reset()
	if subTool.ChildUsage().TotalTokens != 0 {
		t.Errorf("expected 0 child tokens after reset, got %d", subTool.ChildUsage().TotalTokens)
	}
}

func TestSubagentTool_InheritedInvariantsInSystemPrompt(t *testing.T) {
	subTool := NewInvokeSubagentTool(SubagentToolConfig{Depth: 0})
	invariants := []string{"Do not touch main.go", "All tests must pass"}
	prompt := subTool.buildSubagentSystemPrompt("researcher", "Investigate code", invariants)

	for _, inv := range invariants {
		if !strings.Contains(prompt, inv) {
			t.Errorf("expected prompt to contain invariant %q, got:\n%s", inv, prompt)
		}
	}
	if !strings.Contains(prompt, "Inherited Invariants (Strict Constraints)") {
		t.Errorf("expected header for invariants, got:\n%s", prompt)
	}
}

func TestStateTracker_HasDynamicUpdatesWithInvariants(t *testing.T) {
	tracker := history.NewStateTracker("Test task", nil)
	if tracker.HasDynamicUpdates() {
		t.Fatal("expected empty tracker to have no dynamic updates")
	}

	tracker.AddInvariant("Always preserve backwards compatibility")
	if !tracker.HasDynamicUpdates() {
		t.Fatal("expected tracker with invariants to have dynamic updates")
	}
}

func TestSubagentTool_LocalProviderPropagation(t *testing.T) {
	subTool := NewInvokeSubagentTool(SubagentToolConfig{
		Depth:            0,
		IsLocal:          true,
		MaxContextTokens: 32000,
	})

	if !subTool.isLocal {
		t.Errorf("expected subTool.isLocal == true")
	}
	if subTool.maxContextTokens != 32000 {
		t.Errorf("expected subTool.maxContextTokens == 32000, got %d", subTool.maxContextTokens)
	}
}
