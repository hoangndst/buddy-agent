// Package tui contains terminal-facing models and rendering helpers.
package tui

import (
	"time"

	tea "charm.land/bubbletea/v2"
)

// BlockKind identifies one renderable entry in a conversation transcript.
type BlockKind uint8

const (
	// BlockUserMessage is a submitted prompt.
	BlockUserMessage BlockKind = iota
	// BlockAssistantMessage is an assistant response.
	BlockAssistantMessage
	// BlockToolActivity is a tool execution.
	BlockToolActivity
	// BlockSubagentActivity is delegated tool activity.
	BlockSubagentActivity
	// BlockSystemStatus is a system notice.
	BlockSystemStatus
)

// TranscriptBlock is the canonical UI transcript entry. Body keeps source text
// so it can be reflowed after terminal resize and copied without presentation codes.
type TranscriptBlock struct {
	ID        string
	TurnID    string
	ParentID  string
	Kind      BlockKind
	Role      string
	Header    string
	Body      string
	Streaming bool
	Collapsed bool
	Completed bool
	Depth     int
	CreatedAt time.Time
	Activity  *Activity
}

// Activity contains only allowlisted presentation data for a tool invocation.
type Activity struct {
	Identity        string
	ParentIdentity  string
	Tool            string
	AgentName       string
	Path            string
	Line            int
	StartedAt       time.Time
	Elapsed         time.Duration
	ExitCode        *int
	OutputRef       string
	OutputBytes     int64
	OutputTruncated bool
}

// EventKind identifies a protocol-neutral change to conversation presentation.
type EventKind uint8

// Event kinds describe protocol-neutral presentation updates.
const (
	EventSnapshot EventKind = iota
	EventAssistantDelta
	EventAssistantComplete
	EventReasoning
	EventActivityStarted
	EventActivityCompleted
	EventTurnFinished
	EventFailure
	EventCompactionStatus
)

// Event contains only display data. Runtime and protocol values stay in cmd.
type Event struct {
	Kind     EventKind
	TurnID   string
	Text     string
	At       time.Time
	Blocks   []TranscriptBlock
	Activity *Activity
}

// ActionKind identifies an intent that the runtime adapter can execute.
type ActionKind uint8

// Action kinds describe runtime intents produced by the terminal UI.
const (
	ActionSubmitPrompt ActionKind = iota
	ActionInterruptTurn
	ActionOpenOutput
)

// Action is a protocol-neutral runtime intent.
type Action struct {
	Kind   ActionKind
	Prompt string
	Mode   string
	Ref    string
}

// ActionMsg carries a UI intent through Bubble Tea's message loop.
type ActionMsg struct{ Action Action }

// EmitAction sends a neutral action through Bubble Tea's normal message loop.
func EmitAction(action Action) func() tea.Msg {
	return func() tea.Msg { return ActionMsg{Action: action} }
}
