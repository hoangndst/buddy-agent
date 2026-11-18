package agent

import (
	"context"
	"testing"
	"time"

	"github.com/hoangndst/buddy-agent/internal/memory"
	"github.com/hoangndst/buddy-agent/internal/tools"
	"github.com/sashabaranov/go-openai"
)

type mockChatClient struct {
	responses []openai.ChatCompletionResponse
	calls     int
}

func (m *mockChatClient) CreateChatCompletion(_ context.Context, _ openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	if m.calls >= len(m.responses) {
		return openai.ChatCompletionResponse{}, nil
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
		responses: []openai.ChatCompletionResponse{
			// Step 1: Model invokes tool
			{
				Choices: []openai.ChatCompletionChoice{
					{
						Message: openai.ChatCompletionMessage{
							Role:    openai.ChatMessageRoleAssistant,
							Content: "Let me check the environment.",
							ToolCalls: []openai.ToolCall{
								{
									ID:   "call_123",
									Type: openai.ToolTypeFunction,
									Function: openai.FunctionCall{
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
							Role:    openai.ChatMessageRoleAssistant,
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
