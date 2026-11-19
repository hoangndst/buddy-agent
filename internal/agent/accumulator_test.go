package agent

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/openai/openai-go"
)

func TestAccumulatorTextOnly(t *testing.T) {
	acc := NewAccumulator()

	chunks := []openai.ChatCompletionChunkChoiceDelta{
		{Content: "Hello "},
		{Content: "world, "},
		{Content: "agent is ready."},
	}

	for _, c := range chunks {
		acc.AddDelta(c)
	}

	if acc.Content() != "Hello world, agent is ready." {
		t.Fatalf("unexpected content: %s", acc.Content())
	}
	if acc.HasToolCalls() {
		t.Fatal("expected no tool calls")
	}

	msg := acc.ToMessage()
	if msg.Role != "assistant" || msg.Content != "Hello world, agent is ready." {
		t.Fatalf("unexpected message: %+v", msg)
	}
	if len(msg.ToolCalls) != 0 {
		t.Fatalf("expected 0 tool calls in message, got %d", len(msg.ToolCalls))
	}
}

func TestAccumulatorSingleToolCallFragmented(t *testing.T) {
	acc := NewAccumulator()

	// Simulating streaming fragments
	acc.AddDelta(openai.ChatCompletionChunkChoiceDelta{
		Content: "Thinking about running bash...",
		ToolCalls: []openai.ChatCompletionChunkChoiceDeltaToolCall{
			{
				Index: 0,
				ID:    "call_bash_1",
				Function: openai.ChatCompletionChunkChoiceDeltaToolCallFunction{
					Name:      "bash",
					Arguments: `{"command":`,
				},
			},
		},
	})

	acc.AddDelta(openai.ChatCompletionChunkChoiceDelta{
		ToolCalls: []openai.ChatCompletionChunkChoiceDeltaToolCall{
			{
				Index: 0,
				Function: openai.ChatCompletionChunkChoiceDeltaToolCallFunction{
					Arguments: ` "echo hello"`,
				},
			},
		},
	})

	acc.AddDelta(openai.ChatCompletionChunkChoiceDelta{
		ToolCalls: []openai.ChatCompletionChunkChoiceDeltaToolCall{
			{
				Index: 0,
				Function: openai.ChatCompletionChunkChoiceDeltaToolCallFunction{
					Arguments: `}`,
				},
			},
		},
	})

	if !acc.HasToolCalls() {
		t.Fatal("expected tool calls")
	}
	if acc.Content() != "Thinking about running bash..." {
		t.Fatalf("unexpected content: %s", acc.Content())
	}

	calls := acc.ToolCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(calls))
	}
	tc := calls[0]
	if tc.ID != "call_bash_1" {
		t.Errorf("expected ID call_bash_1, got %s", tc.ID)
	}
	if tc.Function.Name != "bash" {
		t.Errorf("expected function bash, got %s", tc.Function.Name)
	}
	expectedArgs := `{"command": "echo hello"}`
	if tc.Function.Arguments != expectedArgs {
		t.Errorf("expected arguments %s, got %s", expectedArgs, tc.Function.Arguments)
	}

	msg := acc.ToMessage()
	param := msg.ToParam()
	b, err := json.Marshal(param)
	if err != nil {
		t.Fatalf("failed to marshal ToParam: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("empty marshaled param")
	}
}

func TestAccumulatorInterleavedParallelToolCalls(t *testing.T) {
	acc := NewAccumulator()

	// Initializing index 0 and 1
	acc.AddDelta(openai.ChatCompletionChunkChoiceDelta{
		ToolCalls: []openai.ChatCompletionChunkChoiceDeltaToolCall{
			{
				Index: 0,
				ID:    "call_0",
				Function: openai.ChatCompletionChunkChoiceDeltaToolCallFunction{
					Name:      "read_file",
					Arguments: `{"path":`,
				},
			},
			{
				Index: 1,
				ID:    "call_1",
				Function: openai.ChatCompletionChunkChoiceDeltaToolCallFunction{
					Name:      "write_file",
					Arguments: `{"path":`,
				},
			},
		},
	})

	// Interleaved argument fragments
	acc.AddDelta(openai.ChatCompletionChunkChoiceDelta{
		ToolCalls: []openai.ChatCompletionChunkChoiceDeltaToolCall{
			{
				Index: 0,
				Function: openai.ChatCompletionChunkChoiceDeltaToolCallFunction{
					Arguments: ` "input.txt"}`,
				},
			},
		},
	})

	acc.AddDelta(openai.ChatCompletionChunkChoiceDelta{
		ToolCalls: []openai.ChatCompletionChunkChoiceDeltaToolCall{
			{
				Index: 1,
				Function: openai.ChatCompletionChunkChoiceDeltaToolCallFunction{
					Arguments: ` "output.txt", "content": "done"}`,
				},
			},
		},
	})

	calls := acc.ToolCalls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 tool calls, got %d", len(calls))
	}

	if calls[0].ID != "call_0" || calls[0].Function.Name != "read_file" || calls[0].Function.Arguments != `{"path": "input.txt"}` {
		t.Errorf("tool call 0 mismatch: %+v", calls[0])
	}
	if calls[1].ID != "call_1" || calls[1].Function.Name != "write_file" || calls[1].Function.Arguments != `{"path": "output.txt", "content": "done"}` {
		t.Errorf("tool call 1 mismatch: %+v", calls[1])
	}
}

func TestAccumulatorConcurrency(t *testing.T) {
	acc := NewAccumulator()
	var wg sync.WaitGroup

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			acc.AddDelta(openai.ChatCompletionChunkChoiceDelta{
				Content: "token ",
				ToolCalls: []openai.ChatCompletionChunkChoiceDeltaToolCall{
					{
						Index: int64(idx),
						ID:    "call_concurrent",
						Function: openai.ChatCompletionChunkChoiceDeltaToolCallFunction{
							Name:      "bash",
							Arguments: `{"cmd":"pwd"}`,
						},
					},
				},
			})
			_ = acc.Content()
			_ = acc.ToolCalls()
			_ = acc.ToMessage()
		}(i)
	}

	wg.Wait()
	if !acc.HasToolCalls() {
		t.Fatal("expected tool calls after concurrent run")
	}
}
