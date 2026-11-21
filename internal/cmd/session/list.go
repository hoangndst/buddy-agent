// Package session provides commands for managing and inspecting historical agent trajectories.
package session

import (
	"fmt"
	"text/tabwriter"

	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	"github.com/spf13/cobra"
)

// NewCmdList creates the session list subcommand.
func NewCmdList(f *cmdutil.Factory) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List recent agent sessions in the active workspace",
		RunE: func(c *cobra.Command, _ []string) error {
			if f.Store == nil {
				return fmt.Errorf("persistence store is not configured")
			}
			store, cleanup, err := f.Store()
			if err != nil {
				return fmt.Errorf("failed to open persistence store: %w", err)
			}
			defer cleanup()

			cfg, _ := f.ConfigLoader()
			workDir := "."
			if cfg != nil && cfg.WorkspaceDir != "" {
				workDir = cfg.WorkspaceDir
			}

			sessions, err := store.ListSessions(c.Context(), workDir, limit)
			if err != nil {
				return fmt.Errorf("failed to list sessions: %w", err)
			}

			if len(sessions) == 0 {
				_, _ = fmt.Fprintln(f.IOStreams.Out, "No historical sessions found in active workspace.")
				return nil
			}

			w := tabwriter.NewWriter(f.IOStreams.Out, 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(w, "ID\tSTATUS\tSTEPS\tTOKENS\tUPDATED\tTASK")
			for _, s := range sessions {
				shortID := s.ID
				if len(shortID) > 12 {
					shortID = shortID[:12]
				}
				taskSnippet := s.Task
				if len(taskSnippet) > 40 {
					taskSnippet = taskSnippet[:40] + "..."
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\t%s\n",
					shortID,
					s.Status,
					s.TotalSteps,
					s.TokenUsage.TotalTokens,
					s.UpdatedAt.Format("2006-01-02 15:04"),
					taskSnippet,
				)
			}
			return w.Flush()
		},
	}
	cmd.Flags().IntVarP(&limit, "limit", "n", 20, "Maximum number of sessions to display")
	return cmd
}
