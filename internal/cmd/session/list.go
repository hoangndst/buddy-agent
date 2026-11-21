// Package session provides commands for managing and inspecting historical agent trajectories.
package session

import (
	"fmt"
	"text/tabwriter"

	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	"github.com/hoangndst/buddy-agent/internal/persistence"
	"github.com/spf13/cobra"
)

// NewCmdList creates the session list subcommand.
func NewCmdList(f *cmdutil.Factory) *cobra.Command {
	var limit int
	var all bool
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

			var sessions []*persistence.SessionRecord
			if all {
				sessions, err = store.ListAllSessions(c.Context(), workDir, limit)
			} else {
				sessions, err = store.ListSessions(c.Context(), workDir, limit)
			}
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
				taskSnippet := s.Task
				if len(taskSnippet) > 40 {
					taskSnippet = taskSnippet[:40] + "..."
				}
				_, _ = fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\t%s\n",
					s.ID,
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
	cmd.Flags().BoolVar(&all, "all", false, "Include child subagent sessions in the listing")
	return cmd
}
