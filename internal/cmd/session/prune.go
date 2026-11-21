package session

import (
	"fmt"
	"time"

	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	"github.com/spf13/cobra"
)

// NewCmdPrune creates the session prune subcommand.
func NewCmdPrune(f *cmdutil.Factory) *cobra.Command {
	var olderThan string
	var keepMax int

	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Prune historical sessions older than retention threshold",
		RunE: func(c *cobra.Command, _ []string) error {
			if f.Store == nil {
				return fmt.Errorf("persistence store is not configured")
			}
			store, cleanup, err := f.Store()
			if err != nil {
				return fmt.Errorf("failed to open persistence store: %w", err)
			}
			defer cleanup()

			dur, err := time.ParseDuration(olderThan)
			if err != nil {
				return fmt.Errorf("invalid duration format for --older-than (e.g. 720h): %w", err)
			}

			affected, err := store.PruneSessions(c.Context(), dur, keepMax)
			if err != nil {
				return fmt.Errorf("failed to prune sessions: %w", err)
			}

			_, _ = fmt.Fprintf(f.IOStreams.Out, "Pruned %d expired sessions (keeping %d most recent).\n", affected, keepMax)
			return nil
		},
	}

	cmd.Flags().StringVar(&olderThan, "older-than", "720h", "Prune sessions older than duration (default 720h / 30 days)")
	cmd.Flags().IntVar(&keepMax, "keep", 50, "Maximum number of recent sessions to preserve")
	return cmd
}
