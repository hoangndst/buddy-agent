package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func updateEvent(m *Model, event Event) {
	m.Update(event)
}

func TestModelReducesStreamingAndSnapshotEvents(t *testing.T) {
	m := NewModel(Header{Workspace: "/work"})
	updateEvent(m, Event{Kind: EventAssistantDelta, TurnID: "turn-1", Text: "hello "})
	updateEvent(m, Event{Kind: EventAssistantDelta, TurnID: "turn-1", Text: "world"})
	if len(m.Transcript.Blocks) != 1 || !m.Transcript.Blocks[0].Streaming || !m.HasBufferedDelta() {
		t.Fatalf("delta did not create one buffered streaming block: %+v", m.Transcript.Blocks)
	}
	m.Update(FlushMsg{})
	updateEvent(m, Event{Kind: EventAssistantComplete, TurnID: "turn-1"})
	if len(m.Transcript.Blocks) != 1 || m.Transcript.Blocks[0].Streaming || m.Transcript.Blocks[0].Body != "hello world" || m.StreamingText() != "" {
		t.Fatalf("completion did not reconcile the active block: %+v, streaming=%q", m.Transcript.Blocks, m.StreamingText())
	}
	updateEvent(m, Event{Kind: EventSnapshot, Blocks: []TranscriptBlock{{ID: "restored", Kind: BlockUserMessage, Body: "restored"}}})
	if len(m.Transcript.Blocks) != 1 || m.Transcript.Blocks[0].ID != "restored" {
		t.Fatalf("snapshot did not replace transcript: %+v", m.Transcript.Blocks)
	}
}

func TestModelMatchesActivityStartAndResult(t *testing.T) {
	m := NewModel(Header{Workspace: "/work"})
	started := &Activity{Identity: "turn\x001\x00call", Tool: "read_file", Path: "main.go", StartedAt: time.Now()}
	updateEvent(m, Event{Kind: EventActivityStarted, TurnID: "turn", Activity: started})
	if len(m.Transcript.Blocks) != 1 || m.Transcript.Blocks[0].Activity.Identity != started.Identity {
		t.Fatalf("activity start was not recorded: %+v", m.Transcript.Blocks)
	}
	completed := *started
	code := 0
	completed.ExitCode, completed.Elapsed = &code, time.Second
	updateEvent(m, Event{Kind: EventActivityCompleted, TurnID: "turn", Activity: &completed, Text: "file contents"})
	if len(m.Transcript.Blocks) != 1 || !m.Transcript.Blocks[0].Completed || !m.Transcript.Blocks[0].Collapsed || m.Transcript.Blocks[0].Body != "file contents" || !strings.Contains(m.Transcript.Blocks[0].Header, "Done read_file") {
		t.Fatalf("activity result did not update its start block: %+v", m.Transcript.Blocks)
	}
}

func TestModelFinishesAndFailsStreamingWithoutStaleTail(t *testing.T) {
	for _, kind := range []EventKind{EventTurnFinished, EventFailure} {
		m := NewModel(Header{})
		updateEvent(m, Event{Kind: EventAssistantDelta, TurnID: "turn", Text: "partial"})
		m.Update(FlushMsg{})
		updateEvent(m, Event{Kind: kind, TurnID: "turn"})
		if m.ActiveBlockID() != "" || m.StreamingText() != "" || m.Transcript.Blocks[0].Streaming {
			t.Fatalf("event %v left active stream state: %+v", kind, m)
		}
	}
}

func TestModelIgnoresUnknownEventKind(t *testing.T) {
	m := NewModel(Header{})
	updateEvent(m, Event{Kind: EventKind(255), Text: "must not be displayed"})
	if len(m.Transcript.Blocks) != 0 {
		t.Fatalf("unknown event created transcript content: %+v", m.Transcript.Blocks)
	}
}

func TestModelOwnsResizeAndWheelViewportState(t *testing.T) {
	m := NewModel(Header{})
	m.Update(tea.WindowSizeMsg{Width: 58, Height: 18})
	m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	if m.Width != 58 || m.Height != 18 || m.Transcript.ScrollOffset != 3 {
		t.Fatalf("terminal events did not update model-owned layout state: %dx%d offset=%d", m.Width, m.Height, m.Transcript.ScrollOffset)
	}
}

type streamingProgramModel struct {
	inner *Model
}

func (m *streamingProgramModel) Init() tea.Cmd {
	return func() tea.Msg { return Event{Kind: EventAssistantDelta, TurnID: "turn", Text: "from live loop"} }
}

func (m *streamingProgramModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	_, cmd := m.inner.Update(msg)
	if _, ok := msg.(FlushMsg); ok {
		m.inner.Update(Event{Kind: EventAssistantComplete, TurnID: "turn"})
		return m, tea.Quit
	}
	return m, cmd
}

func (m *streamingProgramModel) View() tea.View { return m.inner.View() }

func TestBubbleTeaProgramDeliversAndStopsStreamingFlush(t *testing.T) {
	model := &streamingProgramModel{inner: NewModel(Header{})}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := tea.NewProgram(model, tea.WithContext(ctx), tea.WithInput(nil), tea.WithOutput(nil), tea.WithoutRenderer()).Run()
	if err != nil {
		t.Fatal(err)
	}
	if len(model.inner.Transcript.Blocks) != 1 || model.inner.Transcript.Blocks[0].Body != "from live loop" || model.inner.TickerActive() {
		t.Fatalf("Bubble Tea did not deliver the scheduled transcript flush: %+v", model.inner)
	}
}
