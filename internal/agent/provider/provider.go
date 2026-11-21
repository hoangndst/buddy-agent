// Package provider encapsulates LLM wire protocols and auto-negotiation.
package provider

import (
	"context"

	"github.com/hoangndst/buddy-agent/internal/tools"
	"github.com/openai/openai-go"
)

// ModelProvider abstracts wire protocols for the agent execution engine.
type ModelProvider interface {
	// Protocol returns the canonical wire protocol identifier ("chat", "responses", or "auto").
	Protocol() string
	// Generate executes a completion or response step with the underlying LLM service.
	Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error)
}

// GenerateRequest defines canonical parameters for an LLM turn.
type GenerateRequest struct {
	Model           string
	Messages        []openai.ChatCompletionMessageParamUnion
	Tools           []tools.Tool
	ReasoningEffort string
	MaxTokens       int
	IsLocal         bool
	LocalNumCtx     int
}

// ToolCall represents a normalized tool call invocation.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// TokenUsage holds prompt, completion, and reasoning token metrics.
type TokenUsage struct {
	PromptTokens     int64
	CompletionTokens int64
	ReasoningTokens  int64
	TotalTokens      int64
}

// GenerateResponse holds the normalized output from an LLM step.
type GenerateResponse struct {
	Content          string
	ReasoningContent string
	ToolCalls        []ToolCall
	Usage            TokenUsage
	Protocol         string
}

// ToChatMessage converts normalized GenerateResponse into an openai.ChatCompletionMessage
// for seamless commitment into history.Session without breaking the TurnBlock data model.
func (r *GenerateResponse) ToChatMessage() openai.ChatCompletionMessage {
	msg := openai.ChatCompletionMessage{
		Content: r.Content,
	}
	if len(r.ToolCalls) > 0 {
		msg.ToolCalls = make([]openai.ChatCompletionMessageToolCall, len(r.ToolCalls))
		for i, tc := range r.ToolCalls {
			msg.ToolCalls[i] = openai.ChatCompletionMessageToolCall{
				ID: tc.ID,
				Function: openai.ChatCompletionMessageToolCallFunction{
					Name:      tc.Name,
					Arguments: tc.Arguments,
				},
			}
		}
	}
	return msg
}
