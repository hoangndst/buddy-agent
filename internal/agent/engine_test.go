package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hoangndst/buddy-agent/internal/agent/provider"
	"github.com/hoangndst/buddy-agent/internal/history"
	"github.com/hoangndst/buddy-agent/internal/memory"
	"github.com/hoangndst/buddy-agent/internal/persistence"
	"github.com/hoangndst/buddy-agent/internal/tools"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

type mockChatClient struct {
	responses []*openai.ChatCompletion
	err       error
	calls     int
}

func (m *mockChatClient) CreateChatCompletion(_ context.Context, _ openai.ChatCompletionNewParams, _ ...option.RequestOption) (*openai.ChatCompletion, error) {
	if m.err != nil {
		return nil, m.err
	}
	if m.calls >= len(m.responses) {
		return &openai.ChatCompletion{}, nil
	}
	resp := m.responses[m.calls]
	m.calls++
	return resp, nil
}

func (m *mockChatClient) Protocol() string {
	return "chat"
}

func (m *mockChatClient) Generate(ctx context.Context, req provider.GenerateRequest) (*provider.GenerateResponse, error) {
	chatResp, err := m.CreateChatCompletion(ctx, openai.ChatCompletionNewParams{
		Model:    req.Model,
		Messages: req.Messages,
	})
	if err != nil {
		return nil, err
	}
	if len(chatResp.Choices) == 0 {
		return &provider.GenerateResponse{Protocol: "chat"}, nil
	}
	choice := chatResp.Choices[0]
	toolCalls := make([]provider.ToolCall, 0, len(choice.Message.ToolCalls))
	for _, tc := range choice.Message.ToolCalls {
		toolCalls = append(toolCalls, provider.ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	return &provider.GenerateResponse{
		Content:   choice.Message.Content,
		ToolCalls: toolCalls,
		Usage: provider.TokenUsage{
			PromptTokens:     chatResp.Usage.PromptTokens,
			CompletionTokens: chatResp.Usage.CompletionTokens,
			TotalTokens:      chatResp.Usage.TotalTokens,
		},
		Protocol: "chat",
	}, nil
}

func TestEngineExecutionFlow(t *testing.T) {
	tempDir := t.TempDir()
	reg := tools.NewRegistry(1000)
	reg.Register(tools.NewBashTool(tempDir, 5*time.Second))

	memMgr := memory.NewManager(tempDir)

	mockClient := &mockChatClient{
		responses: []*openai.ChatCompletion{
			// Step 1: Model invokes tool
			{
				Choices: []openai.ChatCompletionChoice{
					{
						Message: openai.ChatCompletionMessage{
							Role:    "assistant",
							Content: "Let me check the environment.",
							ToolCalls: []openai.ChatCompletionMessageToolCall{
								{
									ID: "call_123",
									Function: openai.ChatCompletionMessageToolCallFunction{
										Name:      "bash",
										Arguments: `{"command": "echo test_output"}`,
									},
								},
							},
						},
					},
				},
			},
			// Step 2: Model finishes task
			{
				Choices: []openai.ChatCompletionChoice{
					{
						Message: openai.ChatCompletionMessage{
							Role:    "assistant",
							Content: "Task complete: found test_output.",
						},
					},
				},
			},
		},
	}

	var events []Event
	engine := NewEngine(mockClient, "test-model", 5, reg, memMgr, func(e Event) {
		events = append(events, e)
	})

	result, err := engine.Run(context.Background(), "Run the test check")
	if err != nil {
		t.Fatalf("engine failed: %v", err)
	}

	if result != "Task complete: found test_output." {
		t.Errorf("unexpected result: %s", result)
	}

	// Verify events sequence
	hasTextComplete := false
	hasToolCall := false
	hasToolResult := false
	hasFinished := false

	for _, ev := range events {
		switch ev.Type {
		case EventTextComplete:
			hasTextComplete = true
		case EventToolCall:
			hasToolCall = true
		case EventToolResult:
			hasToolResult = true
		case EventFinished:
			hasFinished = true
		}
	}

	if !hasTextComplete || !hasToolCall || !hasToolResult || !hasFinished {
		t.Errorf("missing expected events in trace: %+v", events)
	}
}

func TestEngineToolInterceptorExecutesModifiedArguments(t *testing.T) {
	workspace := t.TempDir()
	store, err := persistence.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	registry := tools.NewRegistry(1000)
	registry.Register(tools.NewWriteFileTool(workspace))
	client := &mockChatClient{responses: []*openai.ChatCompletion{
		{Choices: []openai.ChatCompletionChoice{{Message: openai.ChatCompletionMessage{Role: "assistant", ToolCalls: []openai.ChatCompletionMessageToolCall{{
			ID: "write", Function: openai.ChatCompletionMessageToolCallFunction{Name: "write_file", Arguments: `{"path":"original.txt","content":"original"}`},
		}}}}}},
		{Choices: []openai.ChatCompletionChoice{{Message: openai.ChatCompletionMessage{Role: "assistant", Content: "done"}}}},
	}}
	var events []Event
	engine := NewEngine(client, "test-model", 3, registry, memory.NewManager(workspace), func(event Event) { events = append(events, event) }, WithStore(store), WithSessionIDOverride("approval-interceptor-test"), WithToolInterceptor(func(_ context.Context, tool string, args json.RawMessage) (bool, json.RawMessage, error) {
		if tool != "write_file" || !strings.Contains(string(args), "original.txt") {
			t.Fatalf("interceptor args = %s for %s", args, tool)
		}
		return true, json.RawMessage(`{"path":"approved.txt","content":"approved"}`), nil
	}))
	if _, err := engine.Run(context.Background(), "write a file"); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(workspace, "approved.txt"))
	if err != nil || string(content) != "approved" {
		t.Fatalf("modified arguments were not executed: content=%q err=%v", content, err)
	}
	turns, err := store.GetAllTurns(context.Background(), "approval-interceptor-test")
	if err != nil {
		t.Fatal(err)
	}
	foundArgs := false
	for _, turn := range turns {
		for _, call := range turn.ToolCalls {
			foundArgs = foundArgs || strings.Contains(call.Args, "approved.txt")
		}
	}
	if !foundArgs {
		t.Fatalf("approved tool arguments were not persisted: %+v", turns)
	}
	for _, event := range events {
		if event.Type == EventToolCall && !strings.Contains(event.ToolArgs, "approved.txt") {
			t.Fatalf("tool event did not record approved arguments: %s", event.ToolArgs)
		}
	}
}

func TestEngineToolDenialSkipsDispatchAndContinues(t *testing.T) {
	workspace := t.TempDir()
	registry := tools.NewRegistry(1000)
	registry.Register(tools.NewWriteFileTool(workspace))
	client := &mockChatClient{responses: []*openai.ChatCompletion{
		{Choices: []openai.ChatCompletionChoice{{Message: openai.ChatCompletionMessage{Role: "assistant", ToolCalls: []openai.ChatCompletionMessageToolCall{{
			ID: "write", Function: openai.ChatCompletionMessageToolCallFunction{Name: "write_file", Arguments: `{"path":"denied.txt","content":"no"}`},
		}}}}}},
		{Choices: []openai.ChatCompletionChoice{{Message: openai.ChatCompletionMessage{Role: "assistant", Content: "I will not write it."}}}},
	}}
	var events []Event
	engine := NewEngine(client, "test-model", 3, registry, memory.NewManager(workspace), func(event Event) { events = append(events, event) }, WithToolInterceptor(func(context.Context, string, json.RawMessage) (bool, json.RawMessage, error) {
		return false, nil, &ToolDeniedError{Reason: "policy"}
	}))
	result, err := engine.Run(context.Background(), "write a file")
	if err != nil || result != "I will not write it." {
		t.Fatalf("turn did not recover from denial: result=%q err=%v", result, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "denied.txt")); !os.IsNotExist(err) {
		t.Fatalf("denied tool was executed: stat error=%v", err)
	}
	foundDenial := false
	for _, event := range events {
		if event.Type == EventToolResult && strings.Contains(event.ToolResult, "denied by user: policy") {
			foundDenial = true
		}
	}
	if !foundDenial {
		t.Fatalf("tool denial was not returned to the model: %+v", events)
	}
}

func TestConversationRuntimeRunsSequentialTurnsAgainstSharedHistory(t *testing.T) {
	store, err := persistence.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("create conversation store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	firstPrompt := "inspect the project"
	firstTurnID := "conversation-turn-1"
	firstFingerprint := persistence.SubmissionFingerprint(firstPrompt, persistence.SubmissionQueue)
	start, err := store.CreateConversation(context.Background(), &persistence.ConversationStartRequest{
		Session: persistence.SessionRecord{
			ID:         "conversation-runtime-test",
			WorkingDir: t.TempDir(),
			Model:      "test-model",
		},
		StartID:            "start-runtime-test",
		PayloadFingerprint: firstFingerprint,
		FirstSubmission: persistence.SubmissionRecord{
			SubmissionID:       "submission-1",
			Prompt:             firstPrompt,
			Mode:               persistence.SubmissionQueue,
			PayloadFingerprint: firstFingerprint,
			TurnID:             &firstTurnID,
		},
	})
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}

	provider := &mockChatClient{responses: []*openai.ChatCompletion{
		{Choices: []openai.ChatCompletionChoice{{Message: openai.ChatCompletionMessage{Role: "assistant", Content: "The project is a Go service."}}}},
		{Choices: []openai.ChatCompletionChoice{{Message: openai.ChatCompletionMessage{Role: "assistant", Content: "It uses SQLite for persistence."}}}},
	}}
	engine := NewEngine(provider, "test-model", 5, tools.NewRegistry(1000), nil, nil, WithStore(store))
	conversationHistory, err := engine.NewHistorySession(firstPrompt)
	if err != nil {
		t.Fatalf("create history: %v", err)
	}
	runtime, err := NewConversationRuntime(context.Background(), engine, store, start.ConversationID, start.SubmissionID, conversationHistory)
	if err != nil {
		t.Fatalf("create conversation runtime: %v", err)
	}
	if _, err := runtime.RunTurn(context.Background(), start.SubmissionID, firstTurnID); err != nil {
		t.Fatalf("run first conversation turn: %v", err)
	}

	secondPrompt := "which persistence layer does it use?"
	second, err := store.AdmitSubmission(context.Background(), &persistence.SubmissionRecord{
		ConversationID: start.ConversationID,
		SubmissionID:   "submission-2",
		Prompt:         secondPrompt,
		Mode:           persistence.SubmissionQueue,
	})
	if err != nil {
		t.Fatalf("admit second prompt: %v", err)
	}
	secondTurnID := "conversation-turn-2"
	if _, err := runtime.RunTurn(context.Background(), second.SubmissionID, secondTurnID); err != nil {
		t.Fatalf("run second conversation turn: %v", err)
	}

	if provider.calls != 2 {
		t.Fatalf("expected one model request per user turn, got %d", provider.calls)
	}
	messages := runtime.History().CompileMessages()
	if len(messages) != 4 {
		t.Fatalf("expected root prompt, first answer, follow-up, and second answer; got %d messages", len(messages))
	}
	if messages[2].OfUser == nil || messages[2].OfUser.Content.OfString.Value != secondPrompt {
		t.Fatalf("second turn did not retain the follow-up in shared history: %+v", messages[2])
	}
	steps, err := store.GetAllTurns(context.Background(), start.ConversationID)
	if err != nil {
		t.Fatalf("load persisted ReAct steps: %v", err)
	}
	if len(steps) != 2 || steps[0].ConversationTurnID != firstTurnID || steps[1].ConversationTurnID != secondTurnID {
		t.Fatalf("expected persisted steps linked to both conversation turns: %+v", steps)
	}
	trackerJSON, err := json.Marshal(history.StateTrackerData{})
	if err != nil {
		t.Fatalf("encode snapshot tracker: %v", err)
	}
	if err := store.SaveSnapshot(context.Background(), &persistence.SnapshotRecord{
		SessionID: start.ConversationID, StepNumber: 1, DistilledNarrative: "The project is a Go service.", StateTrackerJSON: string(trackerJSON),
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("save snapshot before follow-up turn: %v", err)
	}
	restored, err := RestoreConversationRuntime(context.Background(), engine, store, start.ConversationID)
	if err != nil {
		t.Fatalf("restore conversation runtime: %v", err)
	}
	restoredMessages := restored.History().CompileMessages()
	followupRestored := false
	for _, message := range restoredMessages {
		if message.OfUser != nil && message.OfUser.Content.OfString.Value == secondPrompt {
			followupRestored = true
		}
	}
	if !followupRestored {
		t.Fatalf("restored history lost the follow-up prompt or turn order: %+v", restoredMessages)
	}
}

func TestEngineErrorAndBudgetExhaustion(t *testing.T) {
	tempDir := t.TempDir()
	reg := tools.NewRegistry(1000)
	memMgr := memory.NewManager(tempDir)

	// 1. Empty task error
	engine := NewEngine(&mockChatClient{}, "test-model", 5, reg, memMgr, nil)
	_, err := engine.Run(context.Background(), "")
	if err == nil {
		t.Fatal("expected error on empty task")
	}

	// 2. Client error propagation
	failClient := &mockChatClient{err: errors.New("network timeout")}
	failEngine := NewEngine(failClient, "test-model", 2, reg, memMgr, nil)
	_, err = failEngine.Run(context.Background(), "do something")
	if err == nil || !errors.Is(err, failClient.err) && err.Error() == "" {
		t.Fatalf("expected error from client, got %v", err)
	}

	// 3. Step budget exhaustion
	loopClient := &mockChatClient{
		responses: []*openai.ChatCompletion{
			{
				Choices: []openai.ChatCompletionChoice{
					{
						Message: openai.ChatCompletionMessage{
							Role:    "assistant",
							Content: "Still working...",
							ToolCalls: []openai.ChatCompletionMessageToolCall{
								{
									ID: "call_loop",
									Function: openai.ChatCompletionMessageToolCallFunction{
										Name:      "nonexistent_tool",
										Arguments: `{}`,
									},
								},
							},
						},
					},
				},
			},
			{
				Choices: []openai.ChatCompletionChoice{
					{
						Message: openai.ChatCompletionMessage{
							Role:    "assistant",
							Content: "Still working again...",
							ToolCalls: []openai.ChatCompletionMessageToolCall{
								{
									ID: "call_loop_2",
									Function: openai.ChatCompletionMessageToolCallFunction{
										Name:      "nonexistent_tool",
										Arguments: `{}`,
									},
								},
							},
						},
					},
				},
			},
		},
	}
	loopEngine := NewEngine(loopClient, "test-model", 2, reg, memMgr, nil)
	_, err = loopEngine.Run(context.Background(), "infinite loop task")
	if err == nil {
		t.Fatal("expected budget exhaustion error")
	}
}

func TestEngineContextCompactionAndExhaustion(t *testing.T) {
	tempDir := t.TempDir()
	reg := tools.NewRegistry(1000)
	reg.Register(tools.NewWriteFileTool(tempDir))
	memMgr := memory.NewManager(tempDir)

	// 1. Test safe halt on context exhaustion
	exhaustClient := &mockChatClient{
		responses: []*openai.ChatCompletion{
			{
				Choices: []openai.ChatCompletionChoice{
					{
						Message: openai.ChatCompletionMessage{
							Role:    "assistant",
							Content: "Running heavy task",
							ToolCalls: []openai.ChatCompletionMessageToolCall{
								{
									ID: "call_exhaust",
									Function: openai.ChatCompletionMessageToolCallFunction{
										Name:      "write_file",
										Arguments: `{"path": "test.txt", "content": "hello"}`,
									},
								},
							},
						},
					},
				},
				Usage: openai.CompletionUsage{PromptTokens: 20},
			},
		},
	}

	var events []Event
	exhaustEngine := NewEngine(
		exhaustClient,
		"test-model",
		5,
		reg,
		memMgr,
		func(e Event) { events = append(events, e) },
		WithMaxContextTokens(15), // Ceiling lower than prompt tokens (20) triggers safe halt
		WithWorkingTailSize(2),
	)

	_, err := exhaustEngine.Run(context.Background(), "Trigger context exhaustion")
	if err == nil {
		t.Fatal("expected context exhaustion error")
	}

	hasExhaustEvent := false
	for _, ev := range events {
		if ev.Type == EventContextExhausted {
			hasExhaustEvent = true
			break
		}
	}
	if !hasExhaustEvent {
		t.Errorf("expected EventContextExhausted in events, got: %+v", events)
	}
}

func TestEngineInitialContextExhaustion(t *testing.T) {
	tempDir := t.TempDir()
	reg := tools.NewRegistry(1000)
	memMgr := memory.NewManager(tempDir)

	mockClient := &mockChatClient{}
	var events []Event

	// Set MaxContextTokens so small (5 tokens) that initial task immediately exceeds it
	engine := NewEngine(
		mockClient,
		"test-model",
		5,
		reg,
		memMgr,
		func(e Event) { events = append(events, e) },
		WithMaxContextTokens(5),
	)

	_, err := engine.Run(context.Background(), "This is an oversized initial task that exceeds five tokens")
	if !errors.Is(err, history.ErrContextExhausted) {
		t.Fatalf("expected ErrContextExhausted on initial prompt, got: %v", err)
	}

	// Model client must NEVER have been called
	if mockClient.calls > 0 {
		t.Errorf("expected 0 LLM calls for oversized initial prompt, got %d", mockClient.calls)
	}

	hasExhaustEvent := false
	for _, ev := range events {
		if ev.Type == EventContextExhausted {
			hasExhaustEvent = true
			break
		}
	}
	if !hasExhaustEvent {
		t.Errorf("expected EventContextExhausted event, got: %+v", events)
	}
}

type flexibleMockClient struct {
	calls   int
	handler func(call int, params openai.ChatCompletionNewParams, opts []option.RequestOption) (*openai.ChatCompletion, error)
}

func (m *flexibleMockClient) CreateChatCompletion(_ context.Context, params openai.ChatCompletionNewParams, opts ...option.RequestOption) (*openai.ChatCompletion, error) {
	resp, err := m.handler(m.calls, params, opts)
	m.calls++
	return resp, err
}

func (m *flexibleMockClient) Protocol() string {
	return "chat"
}

func (m *flexibleMockClient) Generate(ctx context.Context, req provider.GenerateRequest) (*provider.GenerateResponse, error) {
	var opts []option.RequestOption
	if req.IsLocal && req.LocalNumCtx > 0 {
		opts = append(opts, option.WithJSONSet("options.num_ctx", req.LocalNumCtx))
	}
	chatResp, err := m.CreateChatCompletion(ctx, openai.ChatCompletionNewParams{
		Model:    req.Model,
		Messages: req.Messages,
	}, opts...)
	if err != nil {
		return nil, err
	}
	if len(chatResp.Choices) == 0 {
		return &provider.GenerateResponse{Protocol: "chat"}, nil
	}
	choice := chatResp.Choices[0]
	toolCalls := make([]provider.ToolCall, 0, len(choice.Message.ToolCalls))
	for _, tc := range choice.Message.ToolCalls {
		toolCalls = append(toolCalls, provider.ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	return &provider.GenerateResponse{
		Content:   choice.Message.Content,
		ToolCalls: toolCalls,
		Usage: provider.TokenUsage{
			PromptTokens:     chatResp.Usage.PromptTokens,
			CompletionTokens: chatResp.Usage.CompletionTokens,
			TotalTokens:      chatResp.Usage.TotalTokens,
		},
		Protocol: "chat",
	}, nil
}

func TestEngineReactiveContextRecovery(t *testing.T) {
	tempDir := t.TempDir()
	reg := tools.NewRegistry(1000)
	memMgr := memory.NewManager(tempDir)

	mock := &flexibleMockClient{
		handler: func(call int, _ openai.ChatCompletionNewParams, _ []option.RequestOption) (*openai.ChatCompletion, error) {
			if call == 0 {
				// First call fails with context overflow
				return nil, &openai.Error{
					Code:    "context_length_exceeded",
					Message: "maximum context length is 16384 tokens. However, your messages resulted in 18000 tokens.",
				}
			}
			// Second call (retry) succeeds
			return &openai.ChatCompletion{
				Choices: []openai.ChatCompletionChoice{
					{
						Message: openai.ChatCompletionMessage{
							Role:    "assistant",
							Content: "Self-healed and completed successfully.",
						},
					},
				},
				Usage: openai.CompletionUsage{PromptTokens: 12000},
			}, nil
		},
	}

	var events []Event
	engine := NewEngine(
		mock,
		"qwen2.5-coder:7b",
		5,
		reg,
		memMgr,
		func(e Event) { events = append(events, e) },
		WithMaxContextTokens(32000),
		WithWorkingTailSize(2),
	)

	result, err := engine.Run(context.Background(), "Perform recovery test")
	if err != nil {
		t.Fatalf("expected successful execution after reactive retry, got: %v", err)
	}

	if result != "Self-healed and completed successfully." {
		t.Errorf("unexpected result: %s", result)
	}

	hasRetryingEvent := false
	for _, ev := range events {
		if ev.Type == EventContextRetrying {
			hasRetryingEvent = true
			break
		}
	}
	if !hasRetryingEvent {
		t.Errorf("expected EventContextRetrying to be emitted, got events: %+v", events)
	}
}

func TestEngineReactiveContextExhausted(t *testing.T) {
	tempDir := t.TempDir()
	reg := tools.NewRegistry(1000)
	memMgr := memory.NewManager(tempDir)

	mock := &flexibleMockClient{
		handler: func(_ int, _ openai.ChatCompletionNewParams, _ []option.RequestOption) (*openai.ChatCompletion, error) {
			return nil, &openai.Error{
				Code:    "context_length_exceeded",
				Message: "maximum context length is 16384 tokens.",
			}
		},
	}

	var events []Event
	engine := NewEngine(
		mock,
		"qwen2.5-coder:7b",
		5,
		reg,
		memMgr,
		func(e Event) { events = append(events, e) },
		WithMaxContextTokens(32000),
		WithWorkingTailSize(2),
	)

	_, err := engine.Run(context.Background(), "Fail after retries")
	if err == nil {
		t.Fatal("expected error after retry exhaustion, got nil")
	}

	// Should have made 1 initial call + 2 retries = 3 calls
	if mock.calls != 3 {
		t.Errorf("expected 3 total calls (1 initial + 2 retries), got %d", mock.calls)
	}

	retryEvents := 0
	for _, ev := range events {
		if ev.Type == EventContextRetrying {
			retryEvents++
		}
	}
	if retryEvents != 2 {
		t.Errorf("expected 2 EventContextRetrying events, got %d", retryEvents)
	}
}

func TestEngineLocalNumCtxInjection(t *testing.T) {
	tempDir := t.TempDir()
	reg := tools.NewRegistry(1000)
	memMgr := memory.NewManager(tempDir)

	var capturedOpts []option.RequestOption
	mock := &flexibleMockClient{
		handler: func(_ int, _ openai.ChatCompletionNewParams, opts []option.RequestOption) (*openai.ChatCompletion, error) {
			capturedOpts = opts
			return &openai.ChatCompletion{
				Choices: []openai.ChatCompletionChoice{
					{
						Message: openai.ChatCompletionMessage{
							Role:    "assistant",
							Content: "Local run complete.",
						},
					},
				},
			}, nil
		},
	}

	engine := NewEngine(
		mock,
		"qwen2.5-coder:7b",
		5,
		reg,
		memMgr,
		nil,
		WithMaxContextTokens(16384),
		WithIsLocal(true),
	)

	_, err := engine.Run(context.Background(), "Check local num_ctx injection")
	if err != nil {
		t.Fatalf("unexpected engine error: %v", err)
	}

	if len(capturedOpts) == 0 {
		t.Fatal("expected RequestOptions to be passed when isLocal=true")
	}
}

type mockSimpleTool struct {
	name string
	exec func(ctx context.Context, args json.RawMessage) (string, error)
}

func (m *mockSimpleTool) Name() string        { return m.name }
func (m *mockSimpleTool) Description() string { return "mock tool" }
func (m *mockSimpleTool) Parameters() any     { return map[string]any{"type": "object"} }
func (m *mockSimpleTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	return m.exec(ctx, args)
}

func TestEngineCumulativeToolOutputTruncation(t *testing.T) {
	tempDir := t.TempDir()
	reg := tools.NewRegistry(100000)
	// Register a mock tool that outputs large data with Title Case "Exit Code: 0"
	reg.Register(&mockSimpleTool{
		name: "noisy_cmd",
		exec: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "Exit Code: 0\n" + strings.Repeat("log output line\n", 5000), nil
		},
	})
	memMgr := memory.NewManager(tempDir)

	mock := &mockChatClient{
		responses: []*openai.ChatCompletion{
			{
				Choices: []openai.ChatCompletionChoice{
					{
						Message: openai.ChatCompletionMessage{
							Role: "assistant",
							ToolCalls: []openai.ChatCompletionMessageToolCall{
								{
									ID: "call_noisy",
									Function: openai.ChatCompletionMessageToolCallFunction{
										Name:      "noisy_cmd",
										Arguments: `{}`,
									},
								},
							},
						},
					},
				},
			},
			{
				Choices: []openai.ChatCompletionChoice{
					{
						Message: openai.ChatCompletionMessage{
							Role:    "assistant",
							Content: "Done after truncation.",
						},
					},
				},
			},
		},
	}

	var events []Event
	engine := NewEngine(
		mock,
		"test-model",
		5,
		reg,
		memMgr,
		func(e Event) { events = append(events, e) },
		WithMaxContextTokens(8192),
	)

	_, err := engine.Run(context.Background(), "Run noisy command")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var truncatedResult string
	for _, ev := range events {
		if ev.Type == EventToolResult {
			truncatedResult = ev.ToolResult
			break
		}
	}

	expectedPrefix := "[Output truncated: cumulative turn output limit reached] | Exit Code: 0"
	if truncatedResult != expectedPrefix {
		t.Errorf("expected truncated tool result %q, got %q", expectedPrefix, truncatedResult)
	}
}

type mockProviderWithReasoning struct {
	response *provider.GenerateResponse
}

func (m *mockProviderWithReasoning) Protocol() string { return "responses" }
func (m *mockProviderWithReasoning) Generate(_ context.Context, _ provider.GenerateRequest) (*provider.GenerateResponse, error) {
	return m.response, nil
}

func TestEngineReasoningEventEmission(t *testing.T) {
	tempDir := t.TempDir()
	reg := tools.NewRegistry(1000)
	memMgr := memory.NewManager(tempDir)

	mockProv := &mockProviderWithReasoning{
		response: &provider.GenerateResponse{
			Content:          "Final answer",
			ReasoningContent: "Thinking about the solution...",
			Protocol:         "responses",
		},
	}

	var events []Event
	engine := NewEngine(mockProv, "test-model", 5, reg, memMgr, func(e Event) {
		events = append(events, e)
	})

	res, err := engine.Run(context.Background(), "solve this")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != "Final answer" {
		t.Errorf("expected 'Final answer', got %s", res)
	}

	hasReasoning := false
	hasTextComplete := false
	for _, ev := range events {
		if ev.Type == EventReasoning && ev.Content == "Thinking about the solution..." {
			hasReasoning = true
		}
		if ev.Type == EventTextComplete && ev.Content == "Final answer" {
			hasTextComplete = true
		}
	}
	if !hasReasoning {
		t.Errorf("expected EventReasoning event, got events: %+v", events)
	}
	if !hasTextComplete {
		t.Errorf("expected EventTextComplete event, got events: %+v", events)
	}
}

func TestParseToolExitCode(t *testing.T) {
	tests := []struct {
		name     string
		output   string
		expected int
	}{
		{
			name:     "Subprocess runner Exit Code 0",
			output:   "Exit Code: 0\nstdout output",
			expected: 0,
		},
		{
			name:     "Subprocess runner Exit Code 42",
			output:   "Exit Code: 42\nsome command failure",
			expected: 42,
		},
		{
			name:     "Subprocess runner timeout with Exit Code 124",
			output:   "Exit Code: 124 (timeout: 10s)\ncommand timed out",
			expected: 124,
		},
		{
			name:     "Lowercase exit code header",
			output:   "exit code: 1\nerror",
			expected: 1,
		},
		{
			name:     "Standard Go exec exit status",
			output:   "Command failed: exit status 2",
			expected: 2,
		},
		{
			name:     "Tool execution error prefix",
			output:   "Tool error: invalid parameter format",
			expected: -1,
		},
		{
			name:     "Error prefix",
			output:   "Error: cannot open file",
			expected: -1,
		},
		{
			name:     "Normal clean tool output without exit code",
			output:   "file contents here\nline 2",
			expected: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseToolExitCode(tt.output)
			if got != tt.expected {
				t.Errorf("parseToolExitCode(%q) = %d, want %d", tt.output, got, tt.expected)
			}
		})
	}
}

func TestEngine_PersistenceAndResume(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := persistence.NewSQLiteStore(fmt.Sprintf("file:memdb_engine_%d?mode=memory&cache=shared&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	defer func() { _ = store.Close() }()

	memMgr := memory.NewManager(tmpDir)
	reg := tools.NewRegistry(1000)

	// Session 1: executes step 1 with tool call, then fails/stops at step 2
	mockClient := &mockChatClient{
		responses: []*openai.ChatCompletion{
			{
				Choices: []openai.ChatCompletionChoice{
					{
						Message: openai.ChatCompletionMessage{
							Role: "assistant",
							ToolCalls: []openai.ChatCompletionMessageToolCall{
								{
									ID:   "call_step1",
									Type: "function",
									Function: openai.ChatCompletionMessageToolCallFunction{
										Name:      "save_memory",
										Arguments: `{"topic": "learning", "content": "note"}`,
									},
								},
							},
						},
					},
				},
				Usage: openai.CompletionUsage{PromptTokens: 50, CompletionTokens: 20, TotalTokens: 70},
			},
			{
				Choices: []openai.ChatCompletionChoice{
					{
						Message: openai.ChatCompletionMessage{
							Role:    "assistant",
							Content: "Task finished after resume",
						},
					},
				},
				Usage: openai.CompletionUsage{PromptTokens: 70, CompletionTokens: 10, TotalTokens: 80},
			},
		},
	}
	reg.Register(tools.NewSaveMemoryTool(memMgr))

	eng := NewEngine(
		mockClient,
		"gpt-4o",
		1, // limit to 1 step to simulate interruption/budget exhaustion
		reg,
		memMgr,
		nil,
		WithStore(store),
		WithWorkspaceDir(tmpDir),
	)

	ctx := context.Background()
	_, err = eng.Run(ctx, "Test persistence task")
	if err == nil {
		t.Fatal("expected engine to exhaust max steps (1), got nil error")
	}

	sessions, err := store.ListSessions(ctx, tmpDir, 1)
	if err != nil || len(sessions) == 0 {
		t.Fatalf("expected 1 session in store, got %v (err: %v)", sessions, err)
	}
	sessRecord := sessions[0]
	if sessRecord.TotalSteps != 1 {
		t.Errorf("expected 1 completed step, got %d", sessRecord.TotalSteps)
	}

	// Now resume the session with additional budget
	resumeEng := NewEngine(
		mockClient,
		"gpt-4o",
		5,
		reg,
		memMgr,
		nil,
		WithStore(store),
		WithWorkspaceDir(tmpDir),
	)

	result, err := resumeEng.Resume(ctx, sessRecord.ID, 5)
	if err != nil {
		t.Fatalf("Resume failed: %v", err)
	}
	if result != "Task finished after resume" {
		t.Errorf("expected resumed task result 'Task finished after resume', got %q", result)
	}

	resumedSess, err := store.GetSession(ctx, sessRecord.ID)
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}
	if resumedSess.Status != persistence.StatusCompleted {
		t.Errorf("expected status completed, got %s", resumedSess.Status)
	}
	if resumedSess.TotalSteps != 2 {
		t.Errorf("expected total steps 2, got %d", resumedSess.TotalSteps)
	}
}

func TestEngine_SignalInterruptionGracefulPersist(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := persistence.NewSQLiteStore(fmt.Sprintf("file:memdb_sigint_%d?mode=memory&cache=shared&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	defer func() { _ = store.Close() }()

	memMgr := memory.NewManager(tmpDir)
	reg := tools.NewRegistry(1000)

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel context

	eng := NewEngine(
		&mockChatClient{},
		"gpt-4o",
		5,
		reg,
		memMgr,
		nil,
		WithStore(store),
		WithWorkspaceDir(tmpDir),
	)

	_, err = eng.Run(canceledCtx, "Task with canceled context")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}

	sessions, err := store.ListSessions(context.Background(), tmpDir, 1)
	if err != nil || len(sessions) == 0 {
		t.Fatalf("expected 1 session recorded, got %v", sessions)
	}
	if sessions[0].Status != persistence.StatusInterrupted {
		t.Errorf("expected status interrupted, got %s", sessions[0].Status)
	}
}

func TestEngine_ResetCumulativeUsage(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := persistence.NewSQLiteStore(fmt.Sprintf("file:memdb_usage_%d?mode=memory&cache=shared&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	defer func() { _ = store.Close() }()

	memMgr := memory.NewManager(tmpDir)
	reg := tools.NewRegistry(1000)

	mockClient := &mockChatClient{
		responses: []*openai.ChatCompletion{
			{
				Choices: []openai.ChatCompletionChoice{
					{Message: openai.ChatCompletionMessage{Role: "assistant", Content: "Run 1 done"}},
				},
				Usage: openai.CompletionUsage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
			},
			{
				Choices: []openai.ChatCompletionChoice{
					{Message: openai.ChatCompletionMessage{Role: "assistant", Content: "Run 2 done"}},
				},
				Usage: openai.CompletionUsage{PromptTokens: 40, CompletionTokens: 20, TotalTokens: 60},
			},
		},
	}

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

	ctx := context.Background()
	_, err = eng.Run(ctx, "Task 1")
	if err != nil {
		t.Fatalf("Run 1 failed: %v", err)
	}

	// Run 2 must reset cumulative usage
	_, err = eng.Run(ctx, "Task 2")
	if err != nil {
		t.Fatalf("Run 2 failed: %v", err)
	}

	sessions, err := store.ListSessions(ctx, tmpDir, 10)
	if err != nil || len(sessions) != 2 {
		t.Fatalf("expected 2 sessions, got %d", len(sessions))
	}

	// sessions[0] is the most recent (Task 2)
	task2Sess := sessions[0]
	if task2Sess.TokenUsage.TotalTokens != 60 {
		t.Errorf("expected Task 2 to have 60 total tokens, got %d", task2Sess.TokenUsage.TotalTokens)
	}
}

func TestEngine_ResumeWithTurnGaps(t *testing.T) {
	tmpDir := t.TempDir()
	store, err := persistence.NewSQLiteStore(fmt.Sprintf("file:memdb_gap_%d?mode=memory&cache=shared&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	defer func() { _ = store.Close() }()

	ctx := context.Background()
	now := time.Now().UTC()
	sess := &persistence.SessionRecord{
		ID:         "sess_gap_test",
		CreatedAt:  now,
		UpdatedAt:  now,
		Status:     persistence.StatusInterrupted,
		Task:       "Task with step gaps",
		Model:      "gpt-4o",
		WorkingDir: tmpDir,
		MaxSteps:   10,
	}
	_ = store.CreateSession(ctx, sess)

	// Persist step 1 and step 3 (simulating a gap where step 2 failed to write)
	_ = store.CommitTurn(ctx, &persistence.TurnRecord{
		SessionID:  sess.ID,
		StepNumber: 1,
		Role:       "assistant",
		Content:    "Step 1",
		CreatedAt:  now,
	}, 1, persistence.TokenUsage{TotalTokens: 10})
	_ = store.CommitTurn(ctx, &persistence.TurnRecord{
		SessionID:  sess.ID,
		StepNumber: 3,
		Role:       "assistant",
		Content:    "Step 3",
		CreatedAt:  now,
	}, 3, persistence.TokenUsage{TotalTokens: 30})

	memMgr := memory.NewManager(tmpDir)
	reg := tools.NewRegistry(1000)
	mockClient := &mockChatClient{
		responses: []*openai.ChatCompletion{
			{
				Choices: []openai.ChatCompletionChoice{
					{Message: openai.ChatCompletionMessage{Role: "assistant", Content: "Finished at step 4"}},
				},
				Usage: openai.CompletionUsage{PromptTokens: 20, CompletionTokens: 10, TotalTokens: 30},
			},
		},
	}

	eng := NewEngine(
		mockClient,
		"gpt-4o",
		10,
		reg,
		memMgr,
		nil,
		WithStore(store),
		WithWorkspaceDir(tmpDir),
	)

	_, err = eng.Resume(ctx, sess.ID, 0)
	if err != nil {
		t.Fatalf("Resume failed: %v", err)
	}

	// Verify that step 4 was committed (not step 3 conflicting)
	turns, err := store.GetAllTurns(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetAllTurns failed: %v", err)
	}
	if len(turns) != 3 {
		t.Fatalf("expected 3 turns, got %d", len(turns))
	}
	if turns[2].StepNumber != 4 {
		t.Errorf("expected last turn to be step 4, got %d", turns[2].StepNumber)
	}
}

func TestEngine_SessionIDOverride(t *testing.T) {
	mock := &mockChatClient{
		responses: []*openai.ChatCompletion{
			{
				Choices: []openai.ChatCompletionChoice{
					{
						Message: openai.ChatCompletionMessage{
							Role:    "assistant",
							Content: "override done",
						},
					},
				},
			},
		},
	}

	eng := NewEngine(
		mock,
		"test-model",
		1,
		tools.NewRegistry(1000),
		nil,
		nil,
		WithSystemPromptOverride("override prompt"),
		WithSessionIDOverride("sess_override_1234"),
	)
	eng.compClient = mock

	res, err := eng.Run(context.Background(), "test task")
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if res != "override done" {
		t.Errorf("expected 'override done', got %q", res)
	}
	lastRes := eng.LastExecutionResult()
	if lastRes == nil {
		t.Fatal("expected LastExecutionResult to be non-nil")
	}
	if lastRes.SessionID != "sess_override_1234" {
		t.Errorf("expected session ID 'sess_override_1234', got %q", lastRes.SessionID)
	}
}
