// Package agent implements the core ReAct execution engine and event contracts.
package agent

import (
	"context"
	"time"

	"github.com/hoangndst/buddy-agent/internal/agent/provider"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

// ChatClient abstracts the OpenAI-compatible completion client for testability.
type ChatClient interface {
	CreateChatCompletion(ctx context.Context, params openai.ChatCompletionNewParams, opts ...option.RequestOption) (*openai.ChatCompletion, error)
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
func (a *ClientAdapter) CreateChatCompletion(ctx context.Context, params openai.ChatCompletionNewParams, opts ...option.RequestOption) (*openai.ChatCompletion, error) {
	return a.client.Chat.Completions.New(ctx, params, opts...)
}

// EventType distinguishes agent execution events.
type EventType string

// Supported agent event types.
const (
	EventStepStart          EventType = "step_start"
	EventReasoning          EventType = "reasoning"
	EventThought            EventType = "reasoning"  // Deprecated alias for backward test compatibility
	EventTextDelta          EventType = "text_delta" // Reserved for Milestone 9 streaming tokens
	EventTextComplete       EventType = "text_complete"
	EventStepCommitted      EventType = "step_committed"
	EventSnapshotCommitted  EventType = "snapshot_committed"
	EventSubmissionAccepted EventType = "submission_accepted"
	EventSubmissionApplied  EventType = "submission_applied"
	EventToolCall           EventType = "tool_call"
	EventToolResult         EventType = "tool_result"
	EventFinished           EventType = "finished"
	EventError              EventType = "error"
	EventCompactionStart    EventType = "compaction_start"
	EventCompactionDone     EventType = "compaction_done"
	EventMicroPruning       EventType = "micro_pruning"
	EventContextRetrying    EventType = "context_retrying"
	EventContextExhausted   EventType = "context_exhausted"
)

// Event conveys real-time execution telemetry to listeners.
type Event struct {
	Type            EventType
	Step            int
	Timestamp       time.Time
	Duration        time.Duration
	Protocol        string
	Content         string
	ToolCallID      string
	ToolName        string
	ToolArgs        string
	ToolResult      string
	ToolExitCode    int
	Usage           provider.TokenUsage
	HistoryRevision int64
	Error           error
}

// EventHandler receives agent lifecycle events.
type EventHandler func(event Event)
