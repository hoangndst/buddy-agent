// Package tui contains terminal-facing models and rendering helpers.
package tui

import "time"

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
