// Package agent implements the core ReAct execution engine and event contracts.
package agent

import (
	"context"

	"github.com/openai/openai-go"
)

// ChatClient abstracts the OpenAI-compatible completion client for testability.
type ChatClient interface {
	CreateChatCompletion(ctx context.Context, params openai.ChatCompletionNewParams) (*openai.ChatCompletion, error)
}

// ClientAdapter adapts an openai.Client to the ChatClient interface.
type ClientAdapter struct {
	client *openai.Client
}

// NewClientAdapter wraps an openai.Client.
func NewClientAdapter(client *openai.Client) *ClientAdapter {
	return &ClientAdapter{client: client}
}

// CreateChatCompletion executes a completion request using the official SDK.
func (a *ClientAdapter) CreateChatCompletion(ctx context.Context, params openai.ChatCompletionNewParams) (*openai.ChatCompletion, error) {
	return a.client.Chat.Completions.New(ctx, params)
}

// EventType distinguishes agent execution events.
type EventType string

// Supported agent event types.
const (
	EventStepStart  EventType = "step_start"
	EventThought    EventType = "thought"
	EventToolCall   EventType = "tool_call"
	EventToolResult EventType = "tool_result"
	EventFinished   EventType = "finished"
	EventError      EventType = "error"
)

// Event conveys real-time execution telemetry to listeners.
type Event struct {
	Type       EventType
	Step       int
	Content    string
	ToolName   string
	ToolArgs   string
	ToolResult string
	Error      error
}

// EventHandler receives agent lifecycle events.
type EventHandler func(event Event)
