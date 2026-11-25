package tui

import (
	"context"
	"io"

	tea "charm.land/bubbletea/v2"
)

// Run owns the Bubble Tea program lifecycle for terminal-facing applications.
func Run(ctx context.Context, model tea.Model, input io.Reader, output io.Writer) error {
	_, err := tea.NewProgram(model,
		tea.WithContext(ctx),
		tea.WithInput(input),
		tea.WithOutput(output),
	).Run()
	return err
}
