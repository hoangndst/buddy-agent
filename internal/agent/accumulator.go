package agent

import (
	"strings"
	"sync"

	"github.com/openai/openai-go"
)

// AccumulatedToolCall represents an individual tool call being reconstructed
// from streaming delta fragments.
type AccumulatedToolCall struct {
	Index     int64
	ID        string
	Type      string
	Name      string
	arguments strings.Builder
}

// Arguments returns the assembled JSON arguments string.
func (a *AccumulatedToolCall) Arguments() string {
	return a.arguments.String()
}

// Accumulator tracks streaming chunks and assembles complete responses
// and tool call arguments. It is safe for concurrent use.
type Accumulator struct {
	mu        sync.Mutex
	content   strings.Builder
	toolCalls map[int64]*AccumulatedToolCall
	order     []int64
}

// NewAccumulator initializes a new stream accumulator.
func NewAccumulator() *Accumulator {
	return &Accumulator{
		toolCalls: make(map[int64]*AccumulatedToolCall),
		order:     make([]int64, 0),
	}
}

// AddDelta ingests a single chunk delta from an LLM stream.
func (a *Accumulator) AddDelta(delta openai.ChatCompletionChunkChoiceDelta) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if delta.Content != "" {
		a.content.WriteString(delta.Content)
	}

	for _, tc := range delta.ToolCalls {
		call, exists := a.toolCalls[tc.Index]
		if !exists {
			call = &AccumulatedToolCall{
				Index: tc.Index,
				Type:  "function",
			}
			a.toolCalls[tc.Index] = call
			a.order = append(a.order, tc.Index)
		}

		if tc.ID != "" {
			call.ID = tc.ID
		}
		if tc.Type != "" {
			call.Type = tc.Type
		}
		if tc.Function.Name != "" {
			call.Name += tc.Function.Name
		}
		if tc.Function.Arguments != "" {
			call.arguments.WriteString(tc.Function.Arguments)
		}
	}
}

// Content returns the full accumulated text content.
func (a *Accumulator) Content() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.content.String()
}

// ToolCalls returns all accumulated tool calls in their original indexed order.
func (a *Accumulator) ToolCalls() []openai.ChatCompletionMessageToolCall {
	a.mu.Lock()
	defer a.mu.Unlock()

	calls := make([]openai.ChatCompletionMessageToolCall, 0, len(a.order))
	for _, idx := range a.order {
		call := a.toolCalls[idx]
		calls = append(calls, openai.ChatCompletionMessageToolCall{
			ID: call.ID,
			Function: openai.ChatCompletionMessageToolCallFunction{
				Name:      call.Name,
				Arguments: call.Arguments(),
			},
		})
	}
	return calls
}

// HasToolCalls returns true if at least one tool call was accumulated.
func (a *Accumulator) HasToolCalls() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.toolCalls) > 0
}

// ToMessage converts the accumulated state into an openai.ChatCompletionMessage.
func (a *Accumulator) ToMessage() openai.ChatCompletionMessage {
	a.mu.Lock()
	defer a.mu.Unlock()

	msg := openai.ChatCompletionMessage{
		Role:    "assistant",
		Content: a.content.String(),
	}

	if len(a.order) > 0 {
		msg.ToolCalls = make([]openai.ChatCompletionMessageToolCall, 0, len(a.order))
		for _, idx := range a.order {
			call := a.toolCalls[idx]
			msg.ToolCalls = append(msg.ToolCalls, openai.ChatCompletionMessageToolCall{
				ID: call.ID,
				Function: openai.ChatCompletionMessageToolCallFunction{
					Name:      call.Name,
					Arguments: call.Arguments(),
				},
			})
		}
	}

	return msg
}
