package daemon

import (
	"fmt"

	"github.com/hoangndst/buddy-agent/internal/client"
	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	"github.com/hoangndst/buddy-agent/internal/daemon"
	"github.com/spf13/cobra"
)

func newCmdStatus(f *cmdutil.Factory) *cobra.Command {
	var socketPath string

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Display the status of the buddy-agent daemon",
		RunE: func(_ *cobra.Command, _ []string) error {
			if socketPath == "" {
				socketPath = daemon.GetDefaultSocketPath()
			}

			cl, err := client.Connect(socketPath)
			if err != nil {
				_, _ = fmt.Fprintf(f.IOStreams.Out, "Daemon Status: Stopped (no active daemon on %s)\n", socketPath)
				return nil
			}
			defer func() { _ = cl.Close() }()

			status, err := cl.Status()
			if err != nil {
				return fmt.Errorf("failed to query daemon status: %w", err)
			}

			_, _ = fmt.Fprintf(f.IOStreams.Out, "Daemon Status:   Active\n")
			_, _ = fmt.Fprintf(f.IOStreams.Out, "PID:             %d\n", status.PID)
			_, _ = fmt.Fprintf(f.IOStreams.Out, "Socket:          %s\n", status.SocketPath)
			_, _ = fmt.Fprintf(f.IOStreams.Out, "Uptime:          %ds\n", status.UptimeSeconds)
			_, _ = fmt.Fprintf(f.IOStreams.Out, "Active Sessions: %d\n", len(status.ActiveSessions))
			for _, id := range status.ActiveSessions {
				_, _ = fmt.Fprintf(f.IOStreams.Out, "  - %s\n", id)
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&socketPath, "socket", "", "Path to the Unix domain socket")
	return cmd
}
