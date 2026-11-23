package protocol

import (
	"encoding/json"

	"github.com/hoangndst/buddy-agent/internal/mcp"
)

// Canonical JSON-RPC 2.0 Error Codes.
const (
	ErrCodeParseError       = -32700
	ErrCodeInvalidRequest   = -32600
	ErrCodeMethodNotFound   = -32601
	ErrCodeInvalidParams    = -32602
	ErrCodeInternalError    = -32603
	ErrCodeSessionNotFound  = -32001
	ErrCodePermissionDenied = -32002
	ErrCodeConflict         = -32009
)

// Supported JSON-RPC 2.0 Methods and Notifications.
const (
	MethodSessionStart           = "session.start"
	MethodSessionAttach          = "session.attach"
	MethodSessionDetach          = "session.detach"
	MethodSessionCancel          = "session.cancel"
	MethodSessionTakeControl     = "session.take_control"
	MethodPermissionRespond      = "permission.respond"
	MethodConversationStart      = "conversation.start"
	MethodConversationResume     = "conversation.resume"
	MethodConversationSubmit     = "conversation.submit"
	MethodConversationSync       = "conversation.sync"
	MethodConversationWorkspaces = "conversation.list_workspaces"
	MethodConversationList       = "conversation.list"
	MethodTurnInterrupt          = "turn.interrupt"
	MethodDaemonPing             = "daemon.ping"
	MethodDaemonStatus           = "daemon.status"
	MethodDaemonCapabilities     = "daemon.capabilities"
	MethodDaemonStop             = "daemon.stop"

	// Server-to-client notifications.
	NotifySessionEvent        = "session.event"
	NotifyConversationEvent   = "conversation.event"
	NotifyConversationEvicted = "conversation.evicted"
	NotifyPermissionRequired  = "permission.requested"
	NotifyDaemonShuttingDown  = "daemon.shutting_down"
)

// JSONRPCMessage represents the standard JSON-RPC 2.0 frame.
type JSONRPCMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *string         `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *JSONRPCError   `json:"error,omitempty"`
}

// JSONRPCError defines structured errors in JSON-RPC 2.0 format.
type JSONRPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Error implements the standard Go error interface.
func (e *JSONRPCError) Error() string {
	return e.Message
}

// SessionStartParams specifies options for session.start.
type SessionStartParams struct {
	Task             string      `json:"task"`
	WorkspaceDir     string      `json:"workspace_dir,omitempty"`
	Model            string      `json:"model,omitempty"`
	ReasoningEffort  *string     `json:"reasoning_effort,omitempty"`
	MaxSteps         int         `json:"max_steps,omitempty"`
	MaxContextTokens int         `json:"max_context_tokens,omitempty"`
	MaxOutputChars   int         `json:"max_output_chars,omitempty"`
	WorkingTailSize  int         `json:"working_tail_size,omitempty"`
	BaseURL          string      `json:"base_url,omitempty"`
	APIKey           *string     `json:"api_key,omitempty"`
	APIType          string      `json:"api_type,omitempty"`
	CredentialSource string      `json:"credential_source,omitempty"`
	MCPConfig        *mcp.Config `json:"mcp_config,omitempty"`
	DBPath           string      `json:"db_path,omitempty"`
	Env              []string    `json:"env,omitempty"`
}

// SessionStartResult returns the created session identifier.
type SessionStartResult struct {
	SessionID string `json:"session_id"`
}

// ConversationStartParams creates a durable conversation and accepts its first prompt.
// StartID and SubmissionID are client-generated retry identities.
type ConversationStartParams struct {
	StartID          string      `json:"start_id"`
	SubmissionID     string      `json:"submission_id"`
	Prompt           string      `json:"prompt"`
	Mode             string      `json:"mode,omitempty"` // queue or steer; initial prompts are normally queue
	WorkspaceDir     string      `json:"workspace_dir,omitempty"`
	Model            string      `json:"model,omitempty"`
	ReasoningEffort  *string     `json:"reasoning_effort,omitempty"`
	MaxSteps         int         `json:"max_steps,omitempty"`
	MaxContextTokens int         `json:"max_context_tokens,omitempty"`
	MaxOutputChars   int         `json:"max_output_chars,omitempty"`
	WorkingTailSize  int         `json:"working_tail_size,omitempty"`
	BaseURL          string      `json:"base_url,omitempty"`
	APIKey           *string     `json:"api_key,omitempty"`
	APIType          string      `json:"api_type,omitempty"`
	MCPConfig        *mcp.Config `json:"mcp_config,omitempty"`
	DBPath           string      `json:"db_path,omitempty"`
	Env              []string    `json:"env,omitempty"`
}

// ConversationStartResult identifies the accepted conversation and initial turn.
type ConversationStartResult struct {
	ConversationID string `json:"conversation_id"`
	SessionID      string `json:"session_id"` // compatibility alias
	SubmissionID   string `json:"submission_id"`
	TurnID         string `json:"turn_id"`
	Existing       bool   `json:"existing,omitempty"`
}

// ConversationResumeParams restores a conversation by ID or selects the latest in a workspace.
type ConversationResumeParams struct {
	WorkspaceDir     string      `json:"workspace_dir"`
	ConversationID   string      `json:"conversation_id,omitempty"`
	DBPath           string      `json:"db_path,omitempty"`
	Model            string      `json:"model,omitempty"`
	BaseURL          string      `json:"base_url,omitempty"`
	APIType          string      `json:"api_type,omitempty"`
	CredentialSource string      `json:"credential_source,omitempty"`
	APIKey           *string     `json:"api_key,omitempty"`
	ReasoningEffort  *string     `json:"reasoning_effort,omitempty"`
	MaxSteps         int         `json:"max_steps,omitempty"`
	MaxContextTokens int         `json:"max_context_tokens,omitempty"`
	MaxOutputChars   int         `json:"max_output_chars,omitempty"`
	WorkingTailSize  int         `json:"working_tail_size,omitempty"`
	MCPConfig        *mcp.Config `json:"mcp_config,omitempty"`
	Env              []string    `json:"env,omitempty"`
}

// ConversationResumeResult reports the attached conversation and current state.
type ConversationResumeResult struct {
	ConversationID  string `json:"conversation_id"`
	SessionID       string `json:"session_id"` // compatibility alias
	Role            string `json:"role"`
	WorkspaceDir    string `json:"workspace_dir"`
	Task            string `json:"task"`
	HistoryRevision int64  `json:"history_revision"`
	State           string `json:"state"` // idle, running, awaiting_approval, or terminal
}

// ConversationSubmitParams durably accepts a later prompt before acknowledging it.
type ConversationSubmitParams struct {
	ConversationID string `json:"conversation_id"`
	SubmissionID   string `json:"submission_id"`
	Prompt         string `json:"prompt"`
	Mode           string `json:"mode"` // queue or steer
}

// ConversationSubmitResult confirms durable acceptance of a conversation prompt.
type ConversationSubmitResult struct {
	ConversationID string `json:"conversation_id"`
	SubmissionID   string `json:"submission_id"`
	Sequence       int64  `json:"sequence"`
	Status         string `json:"status"`
	TurnID         string `json:"turn_id,omitempty"`
	Existing       bool   `json:"existing,omitempty"`
}

// ConversationSyncParams requests a durable transcript page and the event watermark.
type ConversationSyncParams struct {
	ConversationID  string `json:"conversation_id"`
	WorkerEpoch     string `json:"worker_epoch,omitempty"`
	AfterSequence   uint64 `json:"after_sequence,omitempty"`
	HistoryRevision int64  `json:"history_revision,omitempty"`
	PageToken       string `json:"page_token,omitempty"`
	PageSize        int    `json:"page_size,omitempty"`
}

// ConversationTranscriptEntry is one durable user or assistant transcript item.
type ConversationTranscriptEntry struct {
	SubmissionID     string `json:"submission_id,omitempty"`
	TurnID           string `json:"turn_id,omitempty"`
	StepNumber       int    `json:"step_number,omitempty"`
	Role             string `json:"role"`
	Mode             string `json:"mode,omitempty"`
	Status           string `json:"status,omitempty"`
	Content          string `json:"content"`
	ContentTruncated bool   `json:"content_truncated,omitempty"`
	CreatedAt        string `json:"created_at,omitempty"`
}

// ConversationTurnState reports the persisted lifecycle of one conversation turn.
type ConversationTurnState struct {
	TurnID       string `json:"turn_id"`
	SubmissionID string `json:"submission_id"`
	Status       string `json:"status"`
	StartedAt    string `json:"started_at"`
	FinishedAt   string `json:"finished_at,omitempty"`
	Error        string `json:"error,omitempty"`
}

// ConversationSyncResult contains a transcript page and event synchronization state.
type ConversationSyncResult struct {
	ConversationID   string                          `json:"conversation_id"`
	Role             string                          `json:"role"`
	WorkerEpoch      string                          `json:"worker_epoch"`
	EventWatermark   uint64                          `json:"event_watermark"`
	HistoryRevision  int64                           `json:"history_revision"`
	State            string                          `json:"state"`
	ActiveTurnID     string                          `json:"active_turn_id,omitempty"`
	Transcript       []ConversationTranscriptEntry   `json:"transcript"`
	Turns            []ConversationTurnState         `json:"turns"`
	NextPageToken    string                          `json:"next_page_token,omitempty"`
	PendingApprovals []PermissionRequestNotification `json:"pending_approvals,omitempty"`
	OutputIncomplete bool                            `json:"output_incomplete,omitempty"`
	ExecutionBlocked string                          `json:"execution_blocked,omitempty"`
}

// ConversationListWorkspacesParams limits workspace discovery results.
type ConversationListWorkspacesParams struct {
	Limit int `json:"limit,omitempty"`
}

// ConversationWorkspace summarizes a workspace containing conversations.
type ConversationWorkspace struct {
	WorkspaceDir      string `json:"workspace_dir"`
	ConversationCount int    `json:"conversation_count"`
	UpdatedAt         string `json:"updated_at,omitempty"`
}

// ConversationListWorkspacesResult contains discovered workspaces.
type ConversationListWorkspacesResult struct {
	Workspaces []ConversationWorkspace `json:"workspaces"`
}

// ConversationListParams selects conversations in a workspace.
type ConversationListParams struct {
	WorkspaceDir string `json:"workspace_dir"`
	Limit        int    `json:"limit,omitempty"`
}

// ConversationSummary is a conversation entry in a workspace list.
type ConversationSummary struct {
	ConversationID  string `json:"conversation_id"`
	DBPath          string `json:"db_path,omitempty"`
	Task            string `json:"task"`
	State           string `json:"state"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
	HistoryRevision int64  `json:"history_revision"`
}

// ConversationListResult contains conversation summaries.
type ConversationListResult struct {
	Conversations []ConversationSummary `json:"conversations"`
}

// TurnInterruptParams identifies the conversation turn to interrupt.
type TurnInterruptParams struct {
	ConversationID string `json:"conversation_id"`
	TurnID         string `json:"turn_id"`
}

// TurnInterruptResult reports the result of a turn interruption request.
type TurnInterruptResult struct {
	ConversationID string `json:"conversation_id"`
	TurnID         string `json:"turn_id"`
	Status         string `json:"status"`
}

// DaemonCapabilitiesResult lists the daemon protocol version and supported methods.
type DaemonCapabilitiesResult struct {
	ProtocolVersion int      `json:"protocol_version"`
	Methods         []string `json:"methods"`
}

// SessionAttachParams specifies options for session.attach.
type SessionAttachParams struct {
	SessionID   string `json:"session_id"`
	LastEventID uint64 `json:"last_event_id,omitempty"`
	TakeControl bool   `json:"take_control,omitempty"`
}

// SessionAttachResult reports the attachment status and role.
type SessionAttachResult struct {
	SessionID string `json:"session_id"`
	Role      string `json:"role"` // "controller" or "observer"
	Active    bool   `json:"active"`
}

// SessionCancelParams identifies the session to be canceled.
type SessionCancelParams struct {
	SessionID string `json:"session_id"`
}

// PermissionDecision is the action taken on a pending tool approval.
type PermissionDecision string

const (
	// DecisionAllowOnce allows one tool invocation.
	DecisionAllowOnce PermissionDecision = "allow_once"
	// DecisionAllowSession allows matching tool invocations for the conversation.
	DecisionAllowSession PermissionDecision = "allow_session"
	// DecisionDeny rejects the pending tool invocation.
	DecisionDeny PermissionDecision = "deny"
)

// UnmarshalJSON accepts the legacy "allow" decision as allow_once.
func (d *PermissionDecision) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	if value == "allow" {
		value = string(DecisionAllowOnce)
	}
	*d = PermissionDecision(value)
	return nil
}

// PermissionRespondParams delivers a user's decision for a pending tool call.
type PermissionRespondParams struct {
	RequestID         string             `json:"request_id"`
	Decision          PermissionDecision `json:"decision"`
	SessionID         string             `json:"session_id,omitempty"`
	ConversationID    string             `json:"conversation_id,omitempty"`
	TurnID            string             `json:"turn_id,omitempty"`
	SessionWhitelist  bool               `json:"session_whitelist,omitempty"`
	CommandPattern    string             `json:"command_pattern,omitempty"`
	ModifiedArguments map[string]any     `json:"modified_arguments,omitempty"`
	DenialReason      string             `json:"denial_reason,omitempty"`
}

// PermissionRequestNotification prompts the client for human-in-the-loop tool approval.
type PermissionRequestNotification struct {
	RequestID      string         `json:"request_id"`
	SessionID      string         `json:"session_id"`
	ConversationID string         `json:"conversation_id,omitempty"`
	TurnID         string         `json:"turn_id,omitempty"`
	Tool           string         `json:"tool"`
	Arguments      map[string]any `json:"arguments"`
}

// SessionEventNotification delivers real-time agent execution events to clients.
type SessionEventNotification struct {
	SessionID       string         `json:"session_id"`
	ConversationID  string         `json:"conversation_id,omitempty"`
	TurnID          string         `json:"turn_id,omitempty"`
	WorkerEpoch     string         `json:"worker_epoch,omitempty"`
	Sequence        uint64         `json:"sequence,omitempty"`
	HistoryRevision int64          `json:"history_revision,omitempty"`
	EventID         uint64         `json:"event_id"`
	Type            string         `json:"type"` // reasoning, text_delta, tool_call, tool_result, step_start, finished
	Content         string         `json:"content,omitempty"`
	Data            map[string]any `json:"data,omitempty"`
}

// DaemonStatusResult reports overall daemon liveness and metrics.
type DaemonStatusResult struct {
	PID            int      `json:"pid"`
	UptimeSeconds  int64    `json:"uptime_seconds"`
	SocketPath     string   `json:"socket_path"`
	ActiveSessions []string `json:"active_sessions"`
}
