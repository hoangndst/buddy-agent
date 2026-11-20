package mcp

import (
	"fmt"
	"strings"

	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	"github.com/spf13/cobra"
)

// NewCmdList constructs the 'buddy mcp list' subcommand.
func NewCmdList(f *cmdutil.Factory) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all configured MCP servers and transports",
		RunE: func(_ *cobra.Command, _ []string) error {
			return runList(f)
		},
	}
}

func runList(f *cmdutil.Factory) error {
	wd, _ := f.WorkDir()
	home, _ := f.UserHome()

	cfg, err := f.MCPConfigLoader(wd, home)
	if err != nil {
		return fmt.Errorf("failed to load mcp configuration: %w", err)
	}

	if len(cfg.MCPServers) == 0 {
		_, _ = fmt.Fprintln(f.IOStreams.Out, "No MCP servers configured.")
		_, _ = fmt.Fprintln(f.IOStreams.Out, "Add one using 'buddy mcp add <name> -- <command>' or 'buddy mcp add --url <url> <name>'.")
		return nil
	}

	_, _ = fmt.Fprintln(f.IOStreams.Out, "Configured Model Context Protocol (MCP) Servers:")
	_, _ = fmt.Fprintf(f.IOStreams.Out, "%-20s %-10s %-10s %-10s %s\n", "NAME", "TRANSPORT", "SOURCE", "STATUS", "TARGET / COMMAND")
	_, _ = fmt.Fprintln(f.IOStreams.Out, strings.Repeat("-", 80))

	for name, srv := range cfg.MCPServers {
		status := "enabled"
		if srv.Disabled {
			status = "disabled"
		}
		transport := string(srv.TransportType())
		target := srv.Command
		if len(srv.Args) > 0 {
			target += " " + strings.Join(srv.Args, " ")
		}
		if srv.URL != "" {
			target = srv.URL
		}
		_, _ = fmt.Fprintf(f.IOStreams.Out, "%-20s %-10s %-10s %-10s %s\n", name, transport, srv.Source, status, target)
	}

	return nil
}
