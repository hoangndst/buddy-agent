// Package persistence provides pure-Go SQLite episodic state persistence,
// atomic turn recording, secret redaction, and crash recovery for buddy-agent.
package persistence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrSessionNotFound is returned when a session ID or prefix does not match any stored session.
	ErrSessionNotFound = errors.New("session not found")

	// ErrAmbiguousSessionID is returned when a short session ID prefix matches more than one session.
	ErrAmbiguousSessionID = errors.New("ambiguous session ID prefix")

	// ErrSessionCompleted is returned when attempting to resume a session that already completed successfully.
	ErrSessionCompleted = errors.New("session already completed")

	// ErrSubmissionNotFound is returned when a conversation submission does not exist.
	ErrSubmissionNotFound = errors.New("conversation submission not found")
)

// SessionStatus represents the operational status of an agent execution session.
type SessionStatus string

const (
	// StatusRunning indicates the session is actively executing or was interrupted abruptly without signal cleanup.
	StatusRunning SessionStatus = "running"
	// StatusCompleted indicates the session completed its task successfully.
	StatusCompleted SessionStatus = "completed"
	// StatusFailed indicates the session failed due to budget exhaustion or unrecoverable error.
	StatusFailed SessionStatus = "failed"
	// StatusInterrupted indicates the session was gracefully trapped during interruption (SIGINT, SIGTERM, or cancellation).
	StatusInterrupted SessionStatus = "interrupted"
)

// InteractionKind distinguishes legacy one-shot task executions from M9 conversations.
type InteractionKind string

const (
	// InteractionTask identifies a legacy one-shot task execution.
	InteractionTask InteractionKind = "task"
	// InteractionConversation identifies a persistent multi-turn conversation.
	InteractionConversation InteractionKind = "conversation"
)

// SubmissionMode is the delivery behavior selected for a conversation input.
type SubmissionMode string

const (
	// SubmissionQueue schedules a prompt for a later turn.
	SubmissionQueue SubmissionMode = "queue"
	// SubmissionSteer applies a prompt at the next model boundary when possible.
	SubmissionSteer SubmissionMode = "steer"
)

// SubmissionStatus tracks an accepted user input through turn execution.
type SubmissionStatus string

const (
	// SubmissionQueued indicates a prompt is waiting for turn execution.
	SubmissionQueued SubmissionStatus = "queued"
	// SubmissionSteerPending indicates a steer is waiting for a model boundary.
	SubmissionSteerPending SubmissionStatus = "steer_pending"
	// SubmissionAttached indicates a prompt has been attached to an active turn.
	SubmissionAttached SubmissionStatus = "attached"
	// SubmissionConsumed indicates a prompt was applied and its turn completed.
	SubmissionConsumed SubmissionStatus = "consumed"
)

// ErrSubmissionConflict indicates a submission ID was reused with a different payload.
var ErrSubmissionConflict = errors.New("submission ID already exists with a different payload")

// SubmissionFingerprint returns a stable SHA-256 fingerprint of a prompt and delivery mode.
func SubmissionFingerprint(prompt string, mode SubmissionMode) string {
	hash := sha256.Sum256([]byte(string(mode) + "\x00" + prompt))
	return hex.EncodeToString(hash[:])
}

// TokenUsage aggregates token metrics across model generation turns.
type TokenUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// SessionRecord stores lifecycle metadata and token totals for an execution session.
type SessionRecord struct {
	ID              string          `json:"id"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
	Status          SessionStatus   `json:"status"`
	Task            string          `json:"task"`
	Model           string          `json:"model"`
	WorkingDir      string          `json:"working_dir"`
	MaxSteps        int             `json:"max_steps"`
	TotalSteps      int             `json:"total_steps"`
	TokenUsage      TokenUsage      `json:"usage"`
	ErrorMessage    *string         `json:"error_message,omitempty"`
	ParentSessionID *string         `json:"parent_session_id,omitempty"`
	Role            string          `json:"role"`
	Depth           int             `json:"depth"`
	InteractionKind InteractionKind `json:"interaction_kind"`
}

// SubmissionRecord stores one accepted user prompt and its queue/steer delivery state.
type SubmissionRecord struct {
	ConversationID         string           `json:"conversation_id"`
	SubmissionID           string           `json:"submission_id"`
	Prompt                 string           `json:"prompt"`
	Mode                   SubmissionMode   `json:"mode"`
	PayloadFingerprint     string           `json:"payload_fingerprint"`
	Sequence               int64            `json:"sequence"`
	TurnID                 *string          `json:"turn_id,omitempty"`
	Status                 SubmissionStatus `json:"status"`
	CreatedAt              time.Time        `json:"created_at"`
	HistoryRevision        int64            `json:"history_revision"`
	AppliedAfterStep       *int             `json:"applied_after_step,omitempty"`
	AppliedHistoryRevision int64            `json:"applied_history_revision,omitempty"`
}

// ConversationStartRequest atomically creates a conversation and admits its first prompt.
type ConversationStartRequest struct {
	Session            SessionRecord                `json:"session"`
	StartID            string                       `json:"start_id"`
	PayloadFingerprint string                       `json:"payload_fingerprint"`
	FirstSubmission    SubmissionRecord             `json:"first_submission"`
	ProviderProfile    *ConversationProviderProfile `json:"provider_profile,omitempty"`
}

// ConversationProviderProfile stores nonsecret execution settings required to restore a conversation.
type ConversationProviderProfile struct {
	BaseURL           string `json:"base_url"`
	APIType           string `json:"api_type"`
	Model             string `json:"model"`
	ReasoningEffort   string `json:"reasoning_effort,omitempty"`
	MaxSteps          int    `json:"max_steps"`
	MaxContextTokens  int    `json:"max_context_tokens"`
	MaxOutputChars    int    `json:"max_output_chars"`
	WorkingTailSize   int    `json:"working_tail_size"`
	CredentialSource  string `json:"credential_source"`
	RequiresMCPConfig bool   `json:"requires_mcp_config,omitempty"`
	RequiresEnv       bool   `json:"requires_env,omitempty"`
}

const (
	// ConversationCredentialDaemonDefault means restore resolves credentials from daemon configuration.
	ConversationCredentialDaemonDefault = "daemon-default"
	// ConversationCredentialEphemeral means execution credentials must be supplied again on resume.
	ConversationCredentialEphemeral = "ephemeral"
)

// ConversationStartResult identifies the durable conversation and first submission.
type ConversationStartResult struct {
	ConversationID string `json:"conversation_id"`
	SubmissionID   string `json:"submission_id"`
	TurnID         string `json:"turn_id,omitempty"`
	Existing       bool   `json:"existing"`
}

// ConversationTurnRecord tracks one user-facing agent execution separately from ReAct steps.
type ConversationTurnRecord struct {
	ConversationID  string        `json:"conversation_id"`
	TurnID          string        `json:"turn_id"`
	SubmissionID    string        `json:"submission_id"`
	Status          SessionStatus `json:"status"`
	StartedAt       time.Time     `json:"started_at"`
	FinishedAt      *time.Time    `json:"finished_at,omitempty"`
	ErrorMessage    *string       `json:"error_message,omitempty"`
	HistoryRevision int64         `json:"history_revision"`
}

// ToolCallRecord captures an individual tool invocation and its execution result.
type ToolCallRecord struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Args     string `json:"args"`
	Result   string `json:"result"`
	ExitCode int    `json:"exit_code,omitempty"`
}

// TurnRecord captures an atomic conversational step matching TurnBlock.
type TurnRecord struct {
	SessionID          string           `json:"session_id"`
	ConversationTurnID string           `json:"turn_id,omitempty"`
	HistoryRevision    int64            `json:"history_revision,omitempty"`
	StepNumber         int              `json:"step_number"`
	Role               string           `json:"role"`
	Content            string           `json:"content"`
	ReasoningContent   string           `json:"reasoning_content,omitempty"`
	ToolCalls          []ToolCallRecord `json:"tool_calls,omitempty"`
	TokenUsage         TokenUsage       `json:"usage"`
	DurationMs         int64            `json:"duration_ms"`
	CreatedAt          time.Time        `json:"created_at"`
}

// SnapshotRecord captures an immutable checkpoint of the Three-Partition context state
// produced during context compaction.
type SnapshotRecord struct {
	ID                 int64     `json:"id"`
	SessionID          string    `json:"session_id"`
	StepNumber         int       `json:"step_number"`
	DistilledNarrative string    `json:"distilled_narrative"`
	StateTrackerJSON   string    `json:"state_tracker"`
	CurrentTokens      int       `json:"current_tokens"`
	CreatedAt          time.Time `json:"created_at"`
	HistoryRevision    int64     `json:"history_revision"`
}

// Store defines thread-safe persistence and recovery operations over SQLite.
type Store interface {
	// Lifecycle & Migrations
	Migrate(ctx context.Context) error
	Close() error

	// Sessions
	CreateSession(ctx context.Context, session *SessionRecord) error
	CreateConversation(ctx context.Context, request *ConversationStartRequest) (*ConversationStartResult, error)
	GetConversationProviderProfile(ctx context.Context, conversationID string) (*ConversationProviderProfile, error)
	SaveConversationProviderProfile(ctx context.Context, conversationID string, profile *ConversationProviderProfile) error
	CreateConversationTurn(ctx context.Context, turn *ConversationTurnRecord) error
	UpdateConversationTurn(ctx context.Context, turn *ConversationTurnRecord) error
	ListConversationTurns(ctx context.Context, conversationID string) ([]*ConversationTurnRecord, error)
	AdmitSubmission(ctx context.Context, submission *SubmissionRecord) (*SubmissionRecord, error)
	ApplySubmission(ctx context.Context, conversationID, submissionID, turnID string, appliedAfterStep int) (*SubmissionRecord, error)
	UpdateSubmissionStatus(ctx context.Context, conversationID, submissionID string, status SubmissionStatus, turnID string) error
	GetSubmission(ctx context.Context, conversationID, submissionID string) (*SubmissionRecord, error)
	ListSubmissions(ctx context.Context, conversationID string) ([]*SubmissionRecord, error)
	ListSubmissionsThroughRevision(ctx context.Context, conversationID string, revision int64) ([]*SubmissionRecord, error)
	ListConversations(ctx context.Context, workingDir string, limit int) ([]*SessionRecord, error)
	GetHistoryRevision(ctx context.Context, conversationID string) (int64, error)
	GetSession(ctx context.Context, idOrPrefix string) (*SessionRecord, error)
	GetLatestResumableSession(ctx context.Context, workingDir string) (*SessionRecord, error)
	ListSessions(ctx context.Context, workingDir string, limit int) ([]*SessionRecord, error)
	ListAllSessions(ctx context.Context, workingDir string, limit int) ([]*SessionRecord, error)
	GetChildSessions(ctx context.Context, parentID string) ([]*SessionRecord, error)
	IncrementSessionTokens(ctx context.Context, sessionID string, usage TokenUsage) error
	UpdateSessionStatus(ctx context.Context, id string, status SessionStatus, steps int, usage TokenUsage, errMsg string) error
	PruneSessions(ctx context.Context, olderThan time.Duration, keepMax int) (int64, error)

	// Turns
	CommitTurn(ctx context.Context, turn *TurnRecord, totalSteps int, sessionUsage TokenUsage) error
	GetTurnsAfterStep(ctx context.Context, sessionID string, afterStep int) ([]*TurnRecord, error)
	GetTurnsThroughRevision(ctx context.Context, conversationID string, revision int64) ([]*TurnRecord, error)
	GetAllTurns(ctx context.Context, sessionID string) ([]*TurnRecord, error)

	// Snapshots
	SaveSnapshot(ctx context.Context, snapshot *SnapshotRecord) error
	GetLatestSnapshot(ctx context.Context, sessionID string) (*SnapshotRecord, error)
	GetSnapshotThroughRevision(ctx context.Context, conversationID string, revision int64) (*SnapshotRecord, error)
}

// AmbiguousIDError provides detailed candidate matches when a prefix query collides.
type AmbiguousIDError struct {
	Prefix     string
	Candidates []string
}

func (e *AmbiguousIDError) Error() string {
	return fmt.Sprintf("ambiguous short session ID %q matches %d sessions: %v; please specify more characters",
		e.Prefix, len(e.Candidates), e.Candidates)
}

// Is reports whether the target error matches ErrAmbiguousSessionID.
func (e *AmbiguousIDError) Is(target error) bool {
	return target == ErrAmbiguousSessionID
}
