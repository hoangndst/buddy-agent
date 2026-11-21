// Package daemon implements CLI commands for supervising the background daemon.
package daemon

import (
	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	"github.com/spf13/cobra"
)

// NewCmdDaemon creates the parent 'daemon' Cobra command.
func NewCmdDaemon(f *cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the buddy-agent background execution daemon",
		Long:  `Start, stop, and inspect the status of the buddy-agent background daemon communicating over Unix domain sockets.`,
	}

	cmd.AddCommand(newCmdStart(f))
	cmd.AddCommand(newCmdStop(f))
	cmd.AddCommand(newCmdStatus(f))

	return cmd
}
