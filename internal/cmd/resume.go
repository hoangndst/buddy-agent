package cmd

import (
	"fmt"

	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	"github.com/spf13/cobra"
)

// NewCmdResume creates the resume command.
func NewCmdResume(f *cmdutil.Factory) *cobra.Command {
	var maxSteps int
	cmd := &cobra.Command{
		Use:   "resume [session_id]",
		Short: "Resume an interrupted or failed session",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			engine, cleanup, err := setupEngine(c.Context(), f)
			if err != nil {
				return err
			}
			defer cleanup()

			var targetID string
			if len(args) > 0 {
				targetID = args[0]
			} else {
				if f.Store == nil {
					return fmt.Errorf("persistence store is required to resume session")
				}
				store, storeCleanup, err := f.Store()
				if err != nil {
					return fmt.Errorf("failed to open persistence store: %w", err)
				}
				defer storeCleanup()

				cfg, _ := f.ConfigLoader()
				workDir := "."
				if cfg != nil && cfg.WorkspaceDir != "" {
					workDir = cfg.WorkspaceDir
				}
				latest, err := store.GetLatestResumableSession(c.Context(), workDir)
				if err != nil {
					return fmt.Errorf("no resumable sessions found in workspace %q: %w", workDir, err)
				}
				targetID = latest.ID
			}

			_, _ = fmt.Fprintf(f.IOStreams.Out, "Resuming session %s...\n", targetID)
			result, err := engine.Resume(c.Context(), targetID, maxSteps)
			if err != nil {
				return err
			}
			if result != "" {
				_, _ = fmt.Fprintln(f.IOStreams.Out, result)
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&maxSteps, "max-steps", 0, "Additional step budget override for resumption")
	return cmd
}
