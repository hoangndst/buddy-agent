package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hoangndst/buddy-agent/internal/memory"
	"github.com/hoangndst/buddy-agent/internal/tools"
	"github.com/openai/openai-go"
)

type mockChatClient struct {
	responses []*openai.ChatCompletion
	err       error
	calls     int
}

func (m *mockChatClient) CreateChatCompletion(_ context.Context, _ openai.ChatCompletionNewParams) (*openai.ChatCompletion, error) {
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
	hasThought := false
	hasToolCall := false
	hasToolResult := false
	hasFinished := false

	for _, ev := range events {
		switch ev.Type {
		case EventThought:
			hasThought = true
		case EventToolCall:
			hasToolCall = true
		case EventToolResult:
			hasToolResult = true
		case EventFinished:
			hasFinished = true
		}
	}

	if !hasThought || !hasToolCall || !hasToolResult || !hasFinished {
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
