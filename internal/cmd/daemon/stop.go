package daemon

import (
	"fmt"

	"github.com/hoangndst/buddy-agent/internal/client"
	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	"github.com/hoangndst/buddy-agent/internal/daemon"
	"github.com/spf13/cobra"
)

func newCmdStop(f *cmdutil.Factory) *cobra.Command {
	var socketPath string

	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the running buddy-agent daemon",
		RunE: func(_ *cobra.Command, _ []string) error {
			if socketPath == "" {
				socketPath = daemon.GetDefaultSocketPath()
			}

			cl, err := client.Connect(socketPath)
			if err != nil {
				return fmt.Errorf("daemon is not running on %s", socketPath)
			}
			defer func() { _ = cl.Close() }()

			if err := cl.StopDaemon(); err != nil {
				return fmt.Errorf("failed to stop daemon: %w", err)
			}

			_, _ = fmt.Fprintf(f.IOStreams.Out, "Daemon at %s stopped successfully.\n", socketPath)
			return nil
		},
	}

	cmd.Flags().StringVar(&socketPath, "socket", "", "Path to the Unix domain socket")
	return cmd
}
