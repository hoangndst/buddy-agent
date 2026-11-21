package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hoangndst/buddy-agent/internal/agent/provider"
	"github.com/hoangndst/buddy-agent/internal/history"
	"github.com/hoangndst/buddy-agent/internal/memory"
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

func (m *mockSimpleTool) Name() string { return m.name }
func (m *mockSimpleTool) Description() string { return "mock tool" }
func (m *mockSimpleTool) Parameters() any { return map[string]any{"type": "object"} }
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
