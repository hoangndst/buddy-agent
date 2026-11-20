package mcp

import (
	"errors"
	"fmt"

	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	core "github.com/hoangndst/buddy-agent/internal/mcp"
	"github.com/spf13/cobra"
)

// NewCmdRemove constructs the 'buddy mcp remove' subcommand.
func NewCmdRemove(f *cmdutil.Factory) *cobra.Command {
	var isGlobal bool

	cmd := &cobra.Command{
		Use:     "remove <name>",
		Aliases: []string{"rm"},
		Short:   "Remove an MCP server from configuration",
		Args:    cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			serverName := args[0]
			targetPath, err := getTargetConfigPath(f, isGlobal)
			if err != nil {
				return err
			}

			if err := core.RemoveServerEntry(targetPath, serverName); err != nil {
				if errors.Is(err, core.ErrServerNotFound) {
					scope := "project"
					if isGlobal {
						scope = "global"
					}
					return fmt.Errorf("server %q not found in %s configuration (%s)", serverName, scope, targetPath)
				}
				return fmt.Errorf("failed to remove server %q: %w", serverName, err)
			}

			scope := "project"
			if isGlobal {
				scope = "global"
			}
			_, _ = fmt.Fprintf(f.IOStreams.Out, "✓ Removed MCP server %q from %s configuration (%s)\n", serverName, scope, targetPath)
			return nil
		},
	}

	cmd.Flags().BoolVar(&isGlobal, "global", false, "Remove from global config (~/.buddy/mcp.json)")
	return cmd
}
