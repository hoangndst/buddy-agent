package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hoangndst/buddy-agent/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ToolAdapter wraps an external MCP tool definition to satisfy tools.Tool.
type ToolAdapter struct {
	serverName string
	tool       mcp.Tool
	manager    *ClientManager
	timeout    time.Duration
	name       string
}

// NewToolAdapter constructs a new ToolAdapter for an MCP server tool.
func NewToolAdapter(serverName string, tool mcp.Tool, manager *ClientManager, timeout time.Duration) *ToolAdapter {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &ToolAdapter{
		serverName: serverName,
		tool:       tool,
		manager:    manager,
		timeout:    timeout,
		name:       SanitizeToolName(serverName, tool.Name),
	}
}

// Name returns the deterministic sanitized and namespaced tool identifier.
func (a *ToolAdapter) Name() string {
	if a.name != "" {
		return a.name
	}
	return SanitizeToolName(a.serverName, a.tool.Name)
}

// SetName overrides the tool identifier (used for collision resolution).
func (a *ToolAdapter) SetName(name string) {
	a.name = name
}

// ServerName returns the originating server name.
func (a *ToolAdapter) ServerName() string {
	return a.serverName
}

// ToolName returns the original tool name.
func (a *ToolAdapter) ToolName() string {
	return a.tool.Name
}

// Description returns the tool's description.
func (a *ToolAdapter) Description() string {
	return a.tool.Description
}

// Parameters returns canonicalized OpenAI function parameters.
func (a *ToolAdapter) Parameters() any {
	return tools.CanonicalizeParameters(a.tool.InputSchema)
}

// Execute dispatches the tool call through the ClientManager with isolated timeout and error formatting.
func (a *ToolAdapter) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, a.timeout)
	defer cancel()

	var parsedArgs map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &parsedArgs); err != nil {
			return fmt.Sprintf("MCP tool parameter error (%s.%s): invalid JSON arguments: %v", a.serverName, a.tool.Name, err), nil
		}
	}

	res, err := a.manager.CallTool(callCtx, a.serverName, a.tool.Name, parsedArgs)
	if err != nil {
		return fmt.Sprintf("MCP tool error (%s.%s): %v", a.serverName, a.tool.Name, err), nil
	}

	var sb strings.Builder
	for _, content := range res.Content {
		switch c := content.(type) {
		case *mcp.TextContent:
			sb.WriteString(c.Text)
		case *mcp.ImageContent:
			sb.WriteString(fmt.Sprintf("[Image: mimeType=%s, size=%d bytes]\n", c.MIMEType, len(c.Data)))
		case *mcp.EmbeddedResource:
			if c.Resource != nil {
				sb.WriteString(fmt.Sprintf("[Embedded Resource: uri=%s]\n", c.Resource.URI))
			}
		}
	}

	if res.IsError {
		return fmt.Sprintf("MCP tool returned error (%s.%s): %s", a.serverName, a.tool.Name, sb.String()), nil
	}

	return sb.String(), nil
}

// SanitizeToolName formats and clamps tool identifiers to comply with OpenAI's
// regex ^[a-zA-Z0-9_-]{1,64}$ constraint while avoiding name truncation collisions.
// When names exceed 64 runes, the unambiguous original (server\x00tool) pair is hashed.
func SanitizeToolName(server, tool string) string {
	cleanServer := sanitizeString(server)
	cleanTool := sanitizeString(tool)
	base := fmt.Sprintf("mcp_%s_%s", cleanServer, cleanTool)

	if len(base) <= 64 {
		return base
	}

	// Clamp prefix to 55 runes and append 8-character hex hash of the unambiguous original (server\x00tool) pair
	pairKey := fmt.Sprintf("%s\x00%s", server, tool)
	hash := sha256.Sum256([]byte(pairKey))
	hashHex := hex.EncodeToString(hash[:4]) // 4 bytes = 8 hex characters
	prefix := base[:55]

	return fmt.Sprintf("%s_%s", prefix, hashHex)
}

// DisambiguateToolName generates a collision-free tool identifier by appending
// an 8-character hex hash of the unambiguous original (server\x00tool) pair,
// clamped to 64 runes.
func DisambiguateToolName(server, tool string) string {
	cleanServer := sanitizeString(server)
	cleanTool := sanitizeString(tool)
	base := fmt.Sprintf("mcp_%s_%s", cleanServer, cleanTool)

	pairKey := fmt.Sprintf("%s\x00%s", server, tool)
	hash := sha256.Sum256([]byte(pairKey))
	hashHex := hex.EncodeToString(hash[:4])

	if len(base) > 55 {
		base = base[:55]
	}
	return fmt.Sprintf("%s_%s", base, hashHex)
}

func sanitizeString(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			sb.WriteRune(r)
		} else {
			sb.WriteRune('_')
		}
	}
	res := sb.String()
	if res == "" {
		return "tool"
	}
	return res
}
