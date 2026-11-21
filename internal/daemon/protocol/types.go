package protocol

import "encoding/json"

// Canonical JSON-RPC 2.0 Error Codes.
const (
	ErrCodeParseError       = -32700
	ErrCodeInvalidRequest   = -32600
	ErrCodeMethodNotFound   = -32601
	ErrCodeInvalidParams    = -32602
	ErrCodeInternalError    = -32603
	ErrCodeSessionNotFound  = -32001
	ErrCodePermissionDenied = -32002
)

// Supported JSON-RPC 2.0 Methods and Notifications.
const (
	MethodSessionStart       = "session.start"
	MethodSessionAttach      = "session.attach"
	MethodSessionDetach      = "session.detach"
	MethodSessionCancel      = "session.cancel"
	MethodSessionTakeControl = "session.take_control"
	MethodPermissionRespond  = "permission.respond"
	MethodDaemonPing         = "daemon.ping"
	MethodDaemonStatus       = "daemon.status"
	MethodDaemonStop         = "daemon.stop"

	// Server-to-client notifications.
	NotifySessionEvent       = "session.event"
	NotifyPermissionRequired = "permission.requested"
	NotifyDaemonShuttingDown = "daemon.shutting_down"
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
	Task            string `json:"task"`
	WorkspaceDir    string `json:"workspace_dir,omitempty"`
	Model           string `json:"model,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	MaxSteps        int    `json:"max_steps,omitempty"`
	BaseURL         string `json:"base_url,omitempty"`
	APIKey          string `json:"api_key,omitempty"`
}

// SessionStartResult returns the created session identifier.
type SessionStartResult struct {
	SessionID string `json:"session_id"`
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

// PermissionRespondParams delivers user decision for a pending dangerous tool call.
type PermissionRespondParams struct {
	RequestID string `json:"request_id"`
	Decision  string `json:"decision"` // "allow" or "deny"
}

// PermissionRequestNotification prompts the client for human-in-the-loop tool approval.
type PermissionRequestNotification struct {
	RequestID string         `json:"request_id"`
	SessionID string         `json:"session_id"`
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
}

// SessionEventNotification delivers real-time agent execution events to clients.
type SessionEventNotification struct {
	SessionID string         `json:"session_id"`
	EventID   uint64         `json:"event_id"`
	Type      string         `json:"type"` // "reasoning", "text_delta", "tool_call", "tool_result", "step_start", "finished"
	Content   string         `json:"content,omitempty"`
	Data      map[string]any `json:"data,omitempty"`
}

// DaemonStatusResult reports overall daemon liveness and metrics.
type DaemonStatusResult struct {
	PID            int      `json:"pid"`
	UptimeSeconds  int64    `json:"uptime_seconds"`
	SocketPath     string   `json:"socket_path"`
	ActiveSessions []string `json:"active_sessions"`
}
