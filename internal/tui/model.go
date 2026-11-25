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
	// BlockActivityGroup summarizes tool calls from one model step.
	BlockActivityGroup
)

// TranscriptBlock is the canonical UI transcript entry. Body keeps source text
// so it can be reflowed after terminal resize and copied without presentation codes.
type TranscriptBlock struct {
	ID        string
	TurnID    string
	ParentID  string
	GroupID   string
	Step      int
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
	Step            int
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
	EventApprovalSnapshot
	EventApprovalPending
	EventApprovalResolved
	EventPickerSnapshot
	EventPickerGeneration
	EventPickerClosed
	EventInspectionStart
	EventInspectionPageLoading
	EventOutputPage
	EventDiffResult
	EventInspectionFailure
	EventInspectionClosed
)

// InspectionKind identifies a protocol-neutral output or diff view.
type InspectionKind uint8

const (
	// InspectionNone means no inspector is active.
	InspectionNone InspectionKind = iota
	// InspectionOutput displays captured tool output.
	InspectionOutput
	// InspectionDiff displays the working tree diff.
	InspectionDiff
)

// OutputPage contains one display-safe output page.
type OutputPage struct {
	Data       string
	Offset     int64
	TotalBytes int64
	NextOffset int64
	HasMore    bool
	Truncated  bool
}

// DiffFile contains display-safe content and attribution for one changed file.
type DiffFile struct {
	Path, Diff                              string
	WasDirtyAtStart, IsBuddyEdit            bool
	AttributionUnknown, IsBinary, Truncated bool
}

// DiffStats summarizes the changed files and line counts.
type DiffStats struct {
	FilesChanged, Additions, Deletions int
}

// DiffView contains the bounded, protocol-neutral working-tree view.
type DiffView struct {
	Files        []DiffFile
	Stats        DiffStats
	Warning      string
	OmittedPaths int
	Truncated    bool
}

// ScreenContent carries adapter-prepared transcript and footer rows into the UI compositor.
type ScreenContent struct {
	Header, Body, Footer []string
	WelcomeStatus        string
	Version              string
}

// PickerKind identifies a presentation-only choice flow.
type PickerKind uint8

const (
	// PickerNone means no choice picker is open.
	PickerNone PickerKind = iota
	// PickerWorkspace selects a workspace before listing its conversations.
	PickerWorkspace
	// PickerConversation selects a conversation to resume.
	PickerConversation
)

// Choice contains display data and an opaque UI ID resolved by the adapter.
type Choice struct {
	ID, Label, Detail, State, Timestamp string
}

// Event contains only display data. Runtime and protocol values stay in cmd.
type Event struct {
	Kind                 EventKind
	TurnID               string
	Text                 string
	At                   time.Time
	Blocks               []TranscriptBlock
	Activity             *Activity
	Approvals            []ApprovalRequest
	Approval             *ApprovalRequest
	ApprovalGeneration   uint64
	PickerKind           PickerKind
	PickerGeneration     uint64
	Choices              []Choice
	InspectionKind       InspectionKind
	InspectionGeneration uint64
	InspectionRef        string
	OutputPage           *OutputPage
	DiffView             *DiffView
	InspectionError      string
}

// ActionKind identifies an intent that the runtime adapter can execute.
type ActionKind uint8

// Action kinds describe runtime intents produced by the terminal UI.
const (
	ActionSubmitPrompt ActionKind = iota
	ActionInterruptTurn
	ActionOpenOutput
	ActionPermissionChoice
	ActionSelectChoice
	ActionCancelPicker
	ActionChangeOutputPage
	ActionCloseInspection
)

// Action is a protocol-neutral runtime intent.
type Action struct {
	Kind                 ActionKind
	Prompt               string
	Mode                 string
	Ref                  string
	ApprovalGeneration   uint64
	ApprovalChoice       ApprovalChoice
	PickerKind           PickerKind
	PickerGeneration     uint64
	ChoiceID             string
	InspectionGeneration uint64
	PageDelta            int
}

// ActionMsg carries a UI intent through Bubble Tea's message loop.
type ActionMsg struct{ Action Action }

// EmitAction sends a neutral action through Bubble Tea's normal message loop.
func EmitAction(action Action) func() tea.Msg {
	return func() tea.Msg { return ActionMsg{Action: action} }
}
