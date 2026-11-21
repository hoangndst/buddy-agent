package session

import (
	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	"github.com/spf13/cobra"
)

// NewCmdSession creates the parent session command tree.
func NewCmdSession(f *cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "session",
		Aliases: []string{"sessions"},
		Short:   "Manage and inspect historical agent trajectories",
		Long:    `session provides commands to list, inspect, and prune historical agent execution trajectories.`,
	}

	cmd.AddCommand(NewCmdList(f))
	cmd.AddCommand(NewCmdShow(f))
	cmd.AddCommand(NewCmdPrune(f))

	return cmd
}
