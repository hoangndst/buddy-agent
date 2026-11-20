// Package mcp implements CLI subcommands for managing Model Context Protocol (MCP) servers.
package mcp

import (
	"fmt"
	"path/filepath"

	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	"github.com/spf13/cobra"
)

// NewCmdMCP constructs the 'buddy mcp' command group.
func NewCmdMCP(f *cmdutil.Factory) *cobra.Command {
	mcpCmd := &cobra.Command{
		Use:   "mcp",
		Short: "Manage Model Context Protocol (MCP) server configurations",
		Long: `Configure, inspect, and remove Model Context Protocol (MCP) servers
operating via local stdio subprocesses or remote Server-Sent Events (SSE).`,
	}

	mcpCmd.AddCommand(NewCmdList(f))
	mcpCmd.AddCommand(NewCmdAdd(f))
	mcpCmd.AddCommand(NewCmdRemove(f))

	return mcpCmd
}

func getTargetConfigPath(f *cmdutil.Factory, global bool) (string, error) {
	if global {
		home, err := f.UserHome()
		if err != nil {
			return "", fmt.Errorf("failed to determine user home directory: %w", err)
		}
		return filepath.Join(home, ".buddy", "mcp.json"), nil
	}
	wd, err := f.WorkDir()
	if err != nil {
		return "", fmt.Errorf("failed to determine working directory: %w", err)
	}
	return filepath.Join(wd, ".buddy", "mcp.json"), nil
}
