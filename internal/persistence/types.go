// Package persistence provides pure-Go SQLite episodic state persistence,
// atomic turn recording, secret redaction, and crash recovery for buddy-agent.
package persistence

import (
	"context"
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

// TokenUsage aggregates token metrics across model generation turns.
type TokenUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// SessionRecord stores lifecycle metadata and token totals for an execution session.
type SessionRecord struct {
	ID           string        `json:"id"`
	CreatedAt    time.Time     `json:"created_at"`
	UpdatedAt    time.Time     `json:"updated_at"`
	Status       SessionStatus `json:"status"`
	Task         string        `json:"task"`
	Model        string        `json:"model"`
	WorkingDir   string        `json:"working_dir"`
	MaxSteps     int           `json:"max_steps"`
	TotalSteps   int           `json:"total_steps"`
	TokenUsage   TokenUsage    `json:"usage"`
	ErrorMessage *string       `json:"error_message,omitempty"`
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
	SessionID        string           `json:"session_id"`
	StepNumber       int              `json:"step_number"`
	Role             string           `json:"role"`
	Content          string           `json:"content"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCallRecord `json:"tool_calls,omitempty"`
	TokenUsage       TokenUsage       `json:"usage"`
	DurationMs       int64            `json:"duration_ms"`
	CreatedAt        time.Time        `json:"created_at"`
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
}

// Store defines thread-safe persistence and recovery operations over SQLite.
type Store interface {
	// Lifecycle & Migrations
	Migrate(ctx context.Context) error
	Close() error

	// Sessions
	CreateSession(ctx context.Context, session *SessionRecord) error
	GetSession(ctx context.Context, idOrPrefix string) (*SessionRecord, error)
	GetLatestResumableSession(ctx context.Context, workingDir string) (*SessionRecord, error)
	ListSessions(ctx context.Context, workingDir string, limit int) ([]*SessionRecord, error)
	UpdateSessionStatus(ctx context.Context, id string, status SessionStatus, steps int, usage TokenUsage, errMsg string) error
	PruneSessions(ctx context.Context, olderThan time.Duration, keepMax int) (int64, error)

	// Turns
	CommitTurn(ctx context.Context, turn *TurnRecord, totalSteps int, sessionUsage TokenUsage) error
	GetTurnsAfterStep(ctx context.Context, sessionID string, afterStep int) ([]*TurnRecord, error)
	GetAllTurns(ctx context.Context, sessionID string) ([]*TurnRecord, error)

	// Snapshots
	SaveSnapshot(ctx context.Context, snapshot *SnapshotRecord) error
	GetLatestSnapshot(ctx context.Context, sessionID string) (*SnapshotRecord, error)
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
