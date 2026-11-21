package history

import (
	"errors"
	"fmt"
	"time"

	"github.com/openai/openai-go"
)

// NewUserBlock creates an Atomic Turn Block representing a user prompt.
func NewUserBlock(prompt string) TurnBlock {
	return TurnBlock{
		ID:         fmt.Sprintf("user_%d", time.Now().UnixNano()),
		Kind:       BlockKindUser,
		Timestamp:  time.Now(),
		UserPrompt: prompt,
	}
}

// NewAssistantSimpleBlock creates an Atomic Turn Block for an assistant response without tool calls.
func NewAssistantSimpleBlock(msg openai.ChatCompletionMessage) TurnBlock {
	return TurnBlock{
		ID:              fmt.Sprintf("asst_%d", time.Now().UnixNano()),
		Kind:            BlockKindAssistantSimple,
		Timestamp:       time.Now(),
		AssistantSimple: &msg,
	}
}

// NewAssistantToolCallBlock creates an Atomic Turn Block for an assistant tool invocation
// and its corresponding tool execution responses.
func NewAssistantToolCallBlock(
	msg openai.ChatCompletionMessage,
	results []ToolResult,
) TurnBlock {
	copiedResults := make([]ToolResult, len(results))
	copy(copiedResults, results)

	return TurnBlock{
		ID:                fmt.Sprintf("tool_%d", time.Now().UnixNano()),
		Kind:              BlockKindAssistantToolCall,
		Timestamp:         time.Now(),
		AssistantToolCall: &msg,
		ToolResults:       copiedResults,
	}
}

// CompileMessages converts the TurnBlock into OpenAI message parameters.
// If tool execution was aborted midway, it synthesizes fallback error responses
// for any missing tool_call_ids, guaranteeing that the OpenAI API protocol is never violated.
func (b *TurnBlock) CompileMessages() []openai.ChatCompletionMessageParamUnion {
	switch b.Kind {
	case BlockKindUser:
		if b.UserPrompt == "" {
			return nil
		}
		return []openai.ChatCompletionMessageParamUnion{openai.UserMessage(b.UserPrompt)}

	case BlockKindAssistantSimple:
		if b.AssistantSimple == nil {
			return nil
		}
		return []openai.ChatCompletionMessageParamUnion{b.AssistantSimple.ToParam()}

	case BlockKindAssistantToolCall:
		if b.AssistantToolCall == nil {
			return nil
		}

		callCount := len(b.AssistantToolCall.ToolCalls)
		msgs := make([]openai.ChatCompletionMessageParamUnion, 0, 1+callCount)
		msgs = append(msgs, b.AssistantToolCall.ToParam())

		// Map available results by ToolCallID
		resultMap := make(map[string]string, len(b.ToolResults))
		for _, tr := range b.ToolResults {
			resultMap[tr.ToolCallID] = tr.Content
		}

		// Strictly pair every tool call with a corresponding tool message
		for _, tc := range b.AssistantToolCall.ToolCalls {
			if content, ok := resultMap[tc.ID]; ok {
				msgs = append(msgs, openai.ToolMessage(content, tc.ID))
			} else {
				// Synthesize fallback response to satisfy provider schema
				fallbackMsg := openai.ToolMessage(
					"[Execution aborted before tool completed: timeout, signal, or cancellation]",
					tc.ID,
				)
				msgs = append(msgs, fallbackMsg)
			}
		}
		return msgs

	default:
		return nil
	}
}

// ValidatePairing checks if all tool calls have corresponding tool responses.
func (b *TurnBlock) ValidatePairing() error {
	if b.Kind != BlockKindAssistantToolCall {
		return nil
	}
	if b.AssistantToolCall == nil {
		return errors.New("tool call block missing assistant message")
	}

	resultMap := make(map[string]struct{}, len(b.ToolResults))
	for _, tr := range b.ToolResults {
		resultMap[tr.ToolCallID] = struct{}{}
	}

	for _, tc := range b.AssistantToolCall.ToolCalls {
		if _, ok := resultMap[tc.ID]; !ok {
			return fmt.Errorf("missing tool result for tool_call_id %q (%s)", tc.ID, tc.Function.Name)
		}
	}
	return nil
}

// Clone creates a deep copy of the TurnBlock.
func (b *TurnBlock) Clone() TurnBlock {
	cloned := TurnBlock{
		ID:              b.ID,
		Kind:            b.Kind,
		Timestamp:       b.Timestamp,
		StepNumber:      b.StepNumber,
		UserPrompt:      b.UserPrompt,
		EstimatedTokens: b.EstimatedTokens,
	}

	if b.AssistantSimple != nil {
		as := *b.AssistantSimple
		cloned.AssistantSimple = &as
	}
	if b.AssistantToolCall != nil {
		atc := *b.AssistantToolCall
		cloned.AssistantToolCall = &atc
	}
	if b.ToolResults != nil {
		cloned.ToolResults = make([]ToolResult, len(b.ToolResults))
		copy(cloned.ToolResults, b.ToolResults)
	}

	return cloned
}
