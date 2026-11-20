// Package mcp provides Model Context Protocol (MCP) client integration,
// two-tier configuration management, supervised process group lifecycle,
// dynamic tool adaptation, and on-demand resource bridge tools.
package mcp

import "errors"

// ServerStatus represents the operational status of a configured MCP server.
type ServerStatus string

// Server status enumeration values.
const (
	StatusRunning ServerStatus = "running"
	StatusFailed  ServerStatus = "failed"
	StatusStopped ServerStatus = "stopped"
)

// TransportType represents the communication transport protocol.
type TransportType string

// Supported MCP transport protocols.
const (
	TransportStdio TransportType = "stdio"
	TransportSSE   TransportType = "sse"
)

// ServerState encapsulates runtime diagnostic state and metadata for a server.
type ServerState struct {
	Name            string        `json:"name"`
	Transport       TransportType `json:"transport"`
	Source          string        `json:"source"` // "global" or "project"
	Status          ServerStatus  `json:"status"`
	DiscoveredTools int           `json:"discoveredTools"`
	LastError       string        `json:"lastError,omitempty"`
}

// Common sentinel errors for MCP operations.
var (
	ErrServerNotFound        = errors.New("mcp server not found")
	ErrServerUnavailable     = errors.New("mcp server unavailable")
	ErrHandshakeTimeout      = errors.New("mcp server handshake timeout")
	ErrToolExecutionTimeout  = errors.New("mcp tool execution timeout")
	ErrInvalidConfig         = errors.New("invalid mcp configuration")
	ErrMissingServerName     = errors.New("mcp server name is required")
	ErrMissingCommandOrURL   = errors.New("command or url must be specified")
)
