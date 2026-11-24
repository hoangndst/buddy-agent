package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// Screen contains the already-rendered regions for one fullscreen frame.
type Screen struct {
	Width, Height        int
	Header, Body, Footer []string
	CenterBody           bool
}

// RenderScreen allocates terminal rows to the header, body, and footer.
func RenderScreen(screen Screen) tea.View {
	width, height := max(1, screen.Width), max(1, screen.Height)
	rows := append([]string(nil), screen.Header...)
	available := max(0, height-len(rows)-len(screen.Footer))
	body := screen.Body
	if len(body) > available {
		body = body[len(body)-available:]
	}
	if screen.CenterBody && len(body) < available {
		leading := (available - len(body)) / 2
		rows = append(rows, make([]string, leading)...)
		rows = append(rows, body...)
		rows = append(rows, make([]string, available-leading-len(body))...)
	} else {
		rows = append(rows, body...)
		rows = append(rows, make([]string, max(0, available-len(body)))...)
	}
	rows = append(rows, screen.Footer...)
	rows = fitRows(rows, height)
	for i := range rows {
		rows[i] = ansi.Truncate(rows[i], width, "…")
	}
	view := tea.NewView(strings.Join(rows, "\n"))
	view.AltScreen = true
	view.MouseMode = tea.MouseModeCellMotion
	return view
}

func fitRows(rows []string, height int) []string {
	if height <= 0 {
		return nil
	}
	if len(rows) > height {
		return rows[len(rows)-height:]
	}
	return rows
}
