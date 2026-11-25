package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestInspectionOwnsDiffNavigationAndCloseAction(t *testing.T) {
	m := NewModel(Header{})
	m.Width, m.Height = 60, 20
	m.Update(Event{Kind: EventInspectionStart, InspectionKind: InspectionDiff, InspectionGeneration: 7})
	m.Update(Event{Kind: EventDiffResult, InspectionGeneration: 7, DiffView: &DiffView{Files: []DiffFile{
		{Path: "one.go", Diff: "one"},
		{Path: "two.go", Diff: "header\n@@ -1 +1 @@\n-a\n+b\n@@ -4 +4 @@\n-c\n+d"},
	}, Stats: DiffStats{FilesChanged: 2, Additions: 2, Deletions: 2}}})
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if !strings.Contains(strings.Join(m.InspectionView(18), "\n"), "› two.go") {
		t.Fatal("Down did not move the selected file")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if !strings.Contains(strings.Join(m.InspectionView(18), "\n"), "@@ -1 +1 @") {
		t.Fatal("Enter did not open the selected diff")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	if !strings.Contains(strings.Join(m.InspectionView(18), "\n"), "@@ -4 +4 @") {
		t.Fatal("PageDown did not move to the next hunk")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if !m.HasInspection() || !strings.Contains(strings.Join(m.InspectionView(18), "\n"), "› two.go") {
		t.Fatal("Escape did not return to the file list")
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	if m.HasInspection() {
		t.Fatal("Escape did not close the inspector")
	}
	msg := cmd()
	action, ok := msg.(ActionMsg)
	if !ok || action.Action.Kind != ActionCloseInspection || action.Action.InspectionGeneration != 7 {
		t.Fatalf("close action lost its generation: %#v", msg)
	}
}

func TestInspectionOutputPagingActionAndResize(t *testing.T) {
	m := NewModel(Header{})
	m.Width, m.Height = 12, 8
	m.Update(Event{Kind: EventInspectionStart, InspectionKind: InspectionOutput, InspectionGeneration: 3, InspectionRef: "output-1"})
	m.Update(Event{Kind: EventOutputPage, InspectionGeneration: 3, OutputPage: &OutputPage{Data: "0123456789abcdef\nsecond", TotalBytes: 23, NextOffset: 23, HasMore: true}})
	m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	action, ok := cmd().(ActionMsg)
	if !ok || action.Action.Kind != ActionChangeOutputPage || action.Action.PageDelta != 1 || action.Action.InspectionGeneration != 3 {
		t.Fatalf("page action mismatch: %#v", cmd())
	}
	m.Update(Event{Kind: EventInspectionPageLoading, InspectionGeneration: 3})
	if m.inspection.Scroll != 0 || m.inspection.Column != 0 || !m.inspection.Loading {
		t.Fatal("new page did not reset its viewport")
	}
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	view := m.InspectionView(10)
	if len(view) > 10 {
		t.Fatalf("resize exceeded the available rows: %d", len(view))
	}
	for _, line := range view {
		if width := ansi.StringWidth(line); width > 40 {
			t.Fatalf("line width %d exceeded resized terminal: %q", width, line)
		}
	}
}

func TestInspectionRendersEmptyDiffAndFailureWithinBounds(t *testing.T) {
	m := NewModel(Header{})
	m.Width, m.Height = 36, 10
	m.Update(Event{Kind: EventInspectionStart, InspectionKind: InspectionDiff, InspectionGeneration: 1})
	m.Update(Event{Kind: EventDiffResult, InspectionGeneration: 1, DiffView: &DiffView{Warning: "unsafe path omitted", OmittedPaths: 1}})
	view := strings.Join(m.InspectionView(8), "\n")
	if !strings.Contains(view, "No changed files found") || !strings.Contains(view, "unsafe path omitted") || !strings.Contains(view, "paths omitted") {
		t.Fatalf("empty diff notices missing: %q", view)
	}
	m.Update(Event{Kind: EventInspectionFailure, InspectionGeneration: 1, InspectionError: "request failed"})
	if view = strings.Join(m.InspectionView(8), "\n"); !strings.Contains(view, "request failed") {
		t.Fatalf("inspector failure missing: %q", view)
	}
	_, _ = m.Update(Event{Kind: EventInspectionClosed, InspectionGeneration: 99})
	if !m.HasInspection() {
		t.Fatal("stale close event closed the active inspector")
	}
}

func TestInspectorScrollClampsToFullViewportAndOwnsMouseWheel(t *testing.T) {
	m := NewModel(Header{})
	m.Width, m.Height = 80, 12
	data := strings.TrimSuffix(strings.Repeat("output row\n", 20), "\n")
	m.Update(Event{Kind: EventInspectionStart, InspectionKind: InspectionOutput, InspectionGeneration: 1})
	m.Update(Event{Kind: EventOutputPage, InspectionGeneration: 1, OutputPage: &OutputPage{Data: data}})
	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if m.Transcript.ScrollOffset != 0 {
		t.Fatal("inspector wheel changed hidden transcript scroll")
	}
	m.inspection.Scroll = 100
	view := strings.Join(m.InspectionView(10), "\n")
	if m.inspection.Scroll >= 19 || !strings.Contains(view, "output row") || strings.Count(view, "output row") < 3 {
		t.Fatalf("scroll did not clamp to a full viewport: offset=%d view=%q", m.inspection.Scroll, view)
	}
}

func TestDiffFileListOwnsMouseWheelSelection(t *testing.T) {
	m := NewModel(Header{})
	m.Width, m.Height = 50, 12
	files := make([]DiffFile, 6)
	for i := range files {
		files[i].Path = "file"
	}
	m.Update(Event{Kind: EventInspectionStart, InspectionKind: InspectionDiff, InspectionGeneration: 1})
	m.Update(Event{Kind: EventDiffResult, InspectionGeneration: 1, DiffView: &DiffView{Files: files}})
	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if !strings.Contains(strings.Join(m.InspectionView(10), "\n"), "› file") || m.inspection.SelectedFile != 3 || m.Transcript.ScrollOffset != 0 {
		t.Fatalf("diff list wheel did not select files locally: selected=%d", m.inspection.SelectedFile)
	}
}
