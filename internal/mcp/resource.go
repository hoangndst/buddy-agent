package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hoangndst/buddy-agent/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ResourceListerTool exposes an on-demand tool for dynamic discovery of MCP server resources.
type ResourceListerTool struct {
	manager *ClientManager
}

// NewResourceListerTool creates a new tools.Tool for listing available server resources.
func NewResourceListerTool(manager *ClientManager) tools.Tool {
	return &ResourceListerTool{manager: manager}
}

// Name returns the identifier for the resource lister tool.
func (t *ResourceListerTool) Name() string {
	return "mcp_list_resources"
}

// Description returns the summary of the resource lister tool.
func (t *ResourceListerTool) Description() string {
	return "List available resource URIs and descriptions exposed by an external Model Context Protocol (MCP) server."
}

// Parameters returns the parameter schema for mcp_list_resources.
func (t *ResourceListerTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"server": map[string]any{
				"type":        "string",
				"description": "The name of the configured MCP server.",
			},
		},
		"required": []string{"server"},
	}
}

// Execute queries the MCP server session for exposed resources.
func (t *ResourceListerTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input struct {
		Server string `json:"server"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return fmt.Sprintf("Invalid arguments for mcp_list_resources: %v", err), nil
	}
	if input.Server == "" {
		return "Error: server argument is required", nil
	}

	session, ok := t.manager.GetSession(input.Server)
	if !ok {
		return fmt.Sprintf("Error: MCP server %q is not running or not found", input.Server), nil
	}

	res, err := session.ListResources(ctx, nil)
	if err != nil {
		return fmt.Sprintf("Failed to list resources from server %q: %v", input.Server, err), nil
	}

	if len(res.Resources) == 0 {
		return fmt.Sprintf("Server %q has no exposed resources.", input.Server), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Exposed Resources for server %q:\n", input.Server))
	for _, r := range res.Resources {
		sb.WriteString(fmt.Sprintf("- URI: %s\n  Name: %s\n", r.URI, r.Name))
		if r.Description != "" {
			sb.WriteString(fmt.Sprintf("  Description: %s\n", r.Description))
		}
		if r.MIMEType != "" {
			sb.WriteString(fmt.Sprintf("  MIMEType: %s\n", r.MIMEType))
		}
	}

	return sb.String(), nil
}

// ResourceReaderTool exposes an on-demand tool for reading content from an MCP resource URI.
type ResourceReaderTool struct {
	manager *ClientManager
}

// NewResourceReaderTool creates a new tools.Tool for reading content from an MCP server resource.
func NewResourceReaderTool(manager *ClientManager) tools.Tool {
	return &ResourceReaderTool{manager: manager}
}

// Name returns the identifier for the resource reader tool.
func (t *ResourceReaderTool) Name() string {
	return "mcp_read_resource"
}

// Description returns the summary of the resource reader tool.
func (t *ResourceReaderTool) Description() string {
	return "Read content from an external Model Context Protocol (MCP) server resource URI."
}

// Parameters returns the parameter schema for mcp_read_resource.
func (t *ResourceReaderTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"server": map[string]any{
				"type":        "string",
				"description": "The name of the configured MCP server.",
			},
			"uri": map[string]any{
				"type":        "string",
				"description": "The resource URI identifier exposed by the server (e.g. 'postgres://schema/public').",
			},
		},
		"required": []string{"server", "uri"},
	}
}

// Execute retrieves and formats the resource content from the MCP server.
func (t *ResourceReaderTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var input struct {
		Server string `json:"server"`
		URI    string `json:"uri"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return fmt.Sprintf("Invalid arguments for mcp_read_resource: %v", err), nil
	}
	if input.Server == "" || input.URI == "" {
		return "Error: server and uri arguments are required", nil
	}

	session, ok := t.manager.GetSession(input.Server)
	if !ok {
		return fmt.Sprintf("Error: MCP server %q is not running or not found", input.Server), nil
	}

	res, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: input.URI})
	if err != nil {
		return fmt.Sprintf("Failed to read resource %q from server %q: %v", input.URI, input.Server, err), nil
	}

	var sb strings.Builder
	for _, c := range res.Contents {
		if c.Text != "" {
			sb.WriteString(c.Text)
		}
		if len(c.Blob) > 0 {
			sb.WriteString(fmt.Sprintf("[Binary Resource: uri=%s, mimeType=%s, size=%d bytes]\n", c.URI, c.MIMEType, len(c.Blob)))
		}
	}

	return sb.String(), nil
}
