package session

import (
	"fmt"
	"strings"

	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	"github.com/hoangndst/buddy-agent/internal/persistence"
	"github.com/spf13/cobra"
)

// NewCmdShow creates the session show subcommand.
func NewCmdShow(f *cmdutil.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <session_id>",
		Short: "Display detailed turn-by-turn trajectory for a session",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if f.Store == nil {
				return fmt.Errorf("persistence store is not configured")
			}
			store, cleanup, err := f.Store()
			if err != nil {
				return fmt.Errorf("failed to open persistence store: %w", err)
			}
			defer cleanup()

			sess, err := store.GetSession(c.Context(), args[0])
			if err != nil {
				return fmt.Errorf("failed to retrieve session %q: %w", args[0], err)
			}

			turns, err := store.GetAllTurns(c.Context(), sess.ID)
			if err != nil {
				return fmt.Errorf("failed to retrieve turns: %w", err)
			}

			_, _ = fmt.Fprintf(f.IOStreams.Out, "Session: %s (%s at step %d)\n", sess.ID, sess.Status, sess.TotalSteps)
			_, _ = fmt.Fprintf(f.IOStreams.Out, "Task: %s\n", sess.Task)
			_, _ = fmt.Fprintf(f.IOStreams.Out, "Model: %s | Workspace: %s\n", sess.Model, sess.WorkingDir)
			_, _ = fmt.Fprintf(f.IOStreams.Out, "Tokens: prompt=%d, completion=%d, total=%d\n\n",
				sess.TokenUsage.PromptTokens, sess.TokenUsage.CompletionTokens, sess.TokenUsage.TotalTokens)

			for _, t := range turns {
				_, _ = fmt.Fprintf(f.IOStreams.Out, "[Step %d]\n", t.StepNumber)
				if t.ReasoningContent != "" {
					styledReasoning := cmdutil.ThoughtStyle.Render(fmt.Sprintf("💭 Thinking:\n   %s",
						strings.ReplaceAll(strings.TrimSpace(t.ReasoningContent), "\n", "\n   ")))
					_, _ = fmt.Fprintln(f.IOStreams.Out, styledReasoning)
				}
				for _, tc := range t.ToolCalls {
					preview := cmdutil.FormatToolPreview(tc.Name, tc.Args)
					_, _ = fmt.Fprintln(f.IOStreams.Out, cmdutil.ToolStyle.Render(preview))
					trimmed := strings.TrimSpace(tc.Result)
					if len(trimmed) > 300 {
						trimmed = trimmed[:300] + "..."
					}
					if tc.ExitCode != 0 {
						_, _ = fmt.Fprintln(f.IOStreams.Out, cmdutil.ResultStyle.Render(fmt.Sprintf("→ (exit %d) %s", tc.ExitCode, trimmed)))
					} else {
						_, _ = fmt.Fprintln(f.IOStreams.Out, cmdutil.ResultStyle.Render(fmt.Sprintf("→ %s", trimmed)))
					}
				}
				if t.Content != "" {
					_, _ = fmt.Fprintln(f.IOStreams.Out, t.Content)
				}
				_, _ = fmt.Fprintln(f.IOStreams.Out)
			}

			switch sess.Status {
			case persistence.StatusCompleted:
				_, _ = fmt.Fprintln(f.IOStreams.Out, cmdutil.DoneStyle.Render("✓ Completed successfully"))
			case persistence.StatusInterrupted:
				errMsg := "Process interrupted by user or signal"
				if sess.ErrorMessage != nil && *sess.ErrorMessage != "" {
					errMsg = *sess.ErrorMessage
				}
				_, _ = fmt.Fprintln(f.IOStreams.Out, cmdutil.ErrStyle.Render(fmt.Sprintf("✖ Interrupted: %s", errMsg)))
			case persistence.StatusFailed:
				errMsg := "Execution failed"
				if sess.ErrorMessage != nil && *sess.ErrorMessage != "" {
					errMsg = *sess.ErrorMessage
				}
				_, _ = fmt.Fprintln(f.IOStreams.Out, cmdutil.ErrStyle.Render(fmt.Sprintf("✖ Failed: %s", errMsg)))
			}

			return nil
		},
	}
	return cmd
}
