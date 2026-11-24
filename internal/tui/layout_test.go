package tui

import (
	"strings"
	"testing"
)

func TestRenderScreenAllocatesRegions(t *testing.T) {
	view := RenderScreen(Screen{
		Width: 8, Height: 5,
		Header: []string{"head"}, Body: []string{"body"}, Footer: []string{"foot"},
	})
	if got, want := view.Content, "head\nbody\n\n\nfoot"; got != want {
		t.Fatalf("screen content = %q, want %q", got, want)
	}
	if !view.AltScreen {
		t.Fatal("fullscreen screen did not request alternate buffer")
	}
}

func TestRenderScreenCentersEmptyConversationAndClipsWideRows(t *testing.T) {
	view := RenderScreen(Screen{
		Width: 5, Height: 6,
		Header: []string{"header"}, Body: []string{"one", "two"}, Footer: []string{"footer"},
		CenterBody: true,
	})
	lines := strings.Split(view.Content, "\n")
	if len(lines) != 6 {
		t.Fatalf("got %d rows, want 6: %q", len(lines), view.Content)
	}
	if lines[2] != "one" || lines[3] != "two" {
		t.Fatalf("body not vertically centered: %q", view.Content)
	}
	if strings.Contains(lines[0], "header") || strings.Contains(lines[5], "footer") {
		t.Fatalf("wide rows were not clipped to terminal width: %q", view.Content)
	}
}
