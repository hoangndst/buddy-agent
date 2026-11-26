package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestTranscriptMouseSelectionCopiesRenderedUnicodeAcrossLines(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	m := NewModel(Header{})
	m.Width, m.Height = 40, 8
	m.SetScreenContent(ScreenContent{Header: []string{"header"}, Footer: []string{"footer"}})
	m.Transcript.Append(TranscriptBlock{ID: "user", Kind: BlockUserMessage, Body: "alpha界beta\nsecond line"}, m.Width)
	before := m.View().Content

	if !m.BeginTranscriptSelection(1, 2) {
		t.Fatal("did not start a selection in transcript body")
	}
	if !m.MoveTranscriptSelection(2, 5) {
		t.Fatal("did not update transcript selection during drag")
	}
	if m.View().Content == before {
		t.Fatal("drag selection was not highlighted in the transcript")
	}
	got, selected := m.EndTranscriptSelection(2, 5)
	if !selected || got != "alpha界beta\nsecond" {
		t.Fatalf("selected text = %q, selected=%v", got, selected)
	}
	if view := ansi.Strip(m.View().Content); !strings.Contains(view, "alpha界beta") {
		t.Fatalf("selection changed the rendered transcript: %q", view)
	}
}

func TestTranscriptMouseSelectionNormalizesReverseDragAndStripsControls(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	m := NewModel(Header{})
	m.Width, m.Height = 40, 8
	m.SetScreenContent(ScreenContent{Header: []string{"header"}, Footer: []string{"footer"}})
	m.Transcript.Append(TranscriptBlock{ID: "answer", Kind: BlockAssistantMessage, Body: "**bold** and [link](https://example.com)"}, m.Width)
	line := ansi.Strip(m.Transcript.VisibleLines(6, m.Width)[0])
	start, end := strings.Index(line, "bold"), strings.Index(line, "link")+3
	if !m.BeginTranscriptSelection(1, end) {
		t.Fatal("did not start reverse drag")
	}
	got, selected := m.EndTranscriptSelection(1, start)
	if !selected || !strings.Contains(got, "bold") || !strings.Contains(got, "link") || strings.Contains(got, "\x1b") || strings.Contains(got, "https://") {
		t.Fatalf("reverse selection copied %q, selected=%v", got, selected)
	}
}

func TestTranscriptClickActivatesVisibleActivityGroup(t *testing.T) {
	m := NewModel(Header{})
	m.Width, m.Height = 60, 12
	m.SetScreenContent(ScreenContent{Header: []string{"header"}, Footer: []string{"footer"}})
	m.Transcript.Append(TranscriptBlock{ID: "group", Kind: BlockActivityGroup, Collapsed: true}, m.Width)
	m.Transcript.Append(TranscriptBlock{ID: "call", GroupID: "group", Kind: BlockToolActivity, Header: "read_file", Collapsed: true}, m.Width)
	activated, cmd := m.ActivateTranscriptAt(1)
	if !activated || cmd != nil || m.Transcript.Blocks[0].Collapsed {
		t.Fatalf("click did not expand activity group: activated=%v cmd=%v block=%+v", activated, cmd, m.Transcript.Blocks[0])
	}
}
