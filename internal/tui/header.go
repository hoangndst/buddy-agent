package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Header contains display metadata for the conversation header.
type Header struct {
	Width     int
	Model     string
	Workspace string
	GitBranch string
	State     string
	Standard  bool
}

// RenderHeader formats the compact or standard conversation header.
func RenderHeader(header Header) []string {
	width := max(1, header.Width)
	workspace := header.Workspace
	if workspace == "" {
		workspace = "workspace"
	}
	title := titleStyle.Render("Buddy Agent")
	if !header.Standard {
		state := header.State
		if state == "" {
			state = "idle"
		}
		prefix := title + mutedStyle.Render(" · "+state+" · ")
		return []string{prefix + mutedStyle.Render(ansi.Truncate(workspace, max(1, width-ansi.StringWidth(prefix)), "…"))}
	}
	metadata := []string{workspace}
	if model := strings.TrimSpace(header.Model); model != "" {
		metadata = append([]string{model}, metadata...)
	}
	if header.GitBranch != "" {
		metadata = append(metadata, header.GitBranch)
	}
	return []string{title, mutedStyle.Render(ansi.Truncate(strings.Join(metadata, " · "), width, "…"))}
}
