package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestModelComposesPickerAndInspectionScreens(t *testing.T) {
	m := NewModel(Header{})
	m.Width, m.Height = 40, 12
	m.SetScreenContent(ScreenContent{Body: []string{"transcript body"}, Footer: []string{"normal footer"}})
	m.Update(Event{Kind: EventPickerSnapshot, PickerKind: PickerWorkspace, PickerGeneration: 1, Choices: []Choice{{ID: "w", Label: "workspace choice"}}})
	view := m.View().Content
	if !strings.Contains(view, "workspace choice") || strings.Contains(view, "transcript body") {
		t.Fatalf("picker did not take composition focus: %q", view)
	}
	m.Update(Event{Kind: EventInspectionStart, InspectionKind: InspectionOutput, InspectionGeneration: 2, InspectionRef: "out"})
	m.Update(Event{Kind: EventOutputPage, InspectionGeneration: 2, OutputPage: &OutputPage{Data: "inspector body"}})
	if view = m.View().Content; !strings.Contains(view, "workspace choice") || strings.Contains(view, "inspector body") {
		t.Fatalf("picker did not retain priority over inspector: %q", view)
	}
	m.Update(Event{Kind: EventPickerClosed})
	view = m.View().Content
	if !strings.Contains(view, "inspector body") || strings.Contains(view, "workspace choice") {
		t.Fatalf("inspector did not replace the picker screen: %q", view)
	}
}

func TestModelSubmitsComposerInputAsNeutralAction(t *testing.T) {
	m := NewModel(Header{Workspace: t.TempDir()})
	m.Composer.Editor.SetValue("implement the change")
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter did not emit a submit action")
	}
	msg, ok := cmd().(ActionMsg)
	if !ok || msg.Action.Kind != ActionSubmitPrompt || msg.Action.Prompt != "implement the change" || msg.Action.Mode != "" {
		t.Fatalf("unexpected composer action: %#v", msg)
	}
	if m.Composer.Editor.Value() != "" {
		t.Fatal("submitted prompt remained in the composer")
	}
}

func TestModelOwnsTranscriptActivityFocusAndActivation(t *testing.T) {
	m := NewModel(Header{})
	m.Width, m.Height = 60, 16
	m.Transcript.Append(TranscriptBlock{ID: "first", Kind: BlockToolActivity, Header: "read_file", Collapsed: true, Activity: &Activity{Tool: "read_file"}}, m.Width)
	m.Transcript.Append(TranscriptBlock{ID: "second", Kind: BlockToolActivity, Header: "bash", Collapsed: true, Activity: &Activity{Tool: "bash", OutputRef: "out-1"}}, m.Width)
	m.Update(tea.KeyPressMsg{Code: tea.KeyF6})
	if !m.TranscriptFocused || m.SelectedActivity != "first" {
		t.Fatal("F6 did not focus the first activity")
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.SelectedActivity != "second" {
		t.Fatal("Down did not select the next activity")
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("Enter did not request activity output")
	}
	msg, ok := cmd().(ActionMsg)
	if !ok || msg.Action.Kind != ActionOpenOutput || msg.Action.Ref != "out-1" {
		t.Fatalf("unexpected activity action: %#v", msg)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyF6})
	if m.TranscriptFocused || !m.Composer.Editor.Focused() {
		t.Fatal("F6 did not restore composer focus")
	}
}

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

func TestModelOwnsOrderedApprovalsAndEmitsGenerationBoundChoice(t *testing.T) {
	m := NewModel(Header{})
	approvals := []ApprovalRequest{
		{Generation: 10, Tool: "write_file", TurnID: "turn-1", ArgumentsJSON: `{"path":"one.go"}`},
		{Generation: 11, Tool: "bash", TurnID: "turn-2", ArgumentsJSON: `{"command":"go test"}`, AllowSession: true},
	}
	updateEvent(m, Event{Kind: EventApprovalSnapshot, Approvals: approvals})
	if len(m.Approvals) != 2 || m.ActiveApproval().Generation != 10 {
		t.Fatalf("approval snapshot order = %+v", m.Approvals)
	}
	_, cmd := m.Update(tea.KeyPressMsg{Text: "1", Code: '1'})
	if cmd == nil {
		t.Fatal("visible approval did not emit a choice action")
	}
	choice, ok := cmd().(ActionMsg)
	if !ok || choice.Action.Kind != ActionPermissionChoice || choice.Action.ApprovalGeneration != 10 || choice.Action.ApprovalChoice.Decision != ApprovalAllowOnce {
		t.Fatalf("approval action = %#v", choice)
	}
	updateEvent(m, Event{Kind: EventApprovalResolved, ApprovalGeneration: 10})
	if len(m.Approvals) != 1 || m.ActiveApproval().Generation != 11 {
		t.Fatalf("resolving first approval did not focus next: %+v", m.Approvals)
	}
	if got := m.approvalModal.Editor.Value(); got != approvals[1].ArgumentsJSON {
		t.Fatalf("next approval inherited the previous editor contents: %q", got)
	}
}

func TestModelApprovalPendingAndResizeKeepEditorLocal(t *testing.T) {
	m := NewModel(Header{})
	approval := ApprovalRequest{Generation: 7, Tool: "write_file", TurnID: "turn-1", ArgumentsJSON: `{"path":"file"}`}
	updateEvent(m, Event{Kind: EventApprovalPending, Approval: &approval})
	if !m.HasPendingApproval() || m.ActiveApproval().Generation != 7 {
		t.Fatalf("pending event did not activate approval: %+v", m.Approvals)
	}
	_, _ = m.Update(tea.KeyPressMsg{Text: "4", Code: '4'})
	if !m.ApprovalEditing() {
		t.Fatal("approval edit did not open in the model")
	}
	m.Update(tea.WindowSizeMsg{Width: 10, Height: 1})
	if view := m.ApprovalView(1); !strings.Contains(view, "Resize") {
		t.Fatalf("small resize hid edit safety message: %q", view)
	}
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd != nil {
		t.Fatal("hidden editor emitted an approval action")
	}
}

func TestModelPickerNavigationBoundsResizeAndEscape(t *testing.T) {
	for _, kind := range []PickerKind{PickerWorkspace, PickerConversation} {
		m := NewModel(Header{})
		updateEvent(m, Event{Kind: EventPickerSnapshot, PickerKind: kind, Choices: []Choice{
			{ID: "one", Label: "One"}, {ID: "two", Label: "Two"},
		}})
		_, _ = m.Update(tea.KeyPressMsg{Text: "k", Code: 'k'})
		if m.PickerCursor != 0 {
			t.Fatalf("picker %d moved above the first row: %d", kind, m.PickerCursor)
		}
		_, _ = m.Update(tea.KeyPressMsg{Text: "j", Code: 'j'})
		_, _ = m.Update(tea.KeyPressMsg{Text: "j", Code: 'j'})
		if m.PickerCursor != 1 {
			t.Fatalf("picker %d moved past the last row: %d", kind, m.PickerCursor)
		}
		m.Update(tea.WindowSizeMsg{Width: 24, Height: 3})
		if rows := m.PickerView(3); len(rows) != 3 {
			t.Fatalf("resized picker rows = %d, want 3: %q", len(rows), rows)
		}
		_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
		if m.HasPicker() {
			t.Fatalf("Escape did not close picker %d", kind)
		}
	}
}

func TestModelPickerEmptyAndSelectionAction(t *testing.T) {
	m := NewModel(Header{})
	updateEvent(m, Event{Kind: EventPickerSnapshot, PickerKind: PickerWorkspace})
	if _, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}); cmd != nil {
		t.Fatal("empty picker emitted a selection")
	}
	updateEvent(m, Event{Kind: EventPickerSnapshot, PickerKind: PickerConversation, PickerGeneration: 12, Choices: []Choice{
		{ID: "conversation-2", Label: "second"},
	}})
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("picker did not emit selection")
	}
	action, ok := cmd().(ActionMsg)
	if !ok || action.Action.Kind != ActionSelectChoice || action.Action.PickerKind != PickerConversation || action.Action.PickerGeneration != 12 || action.Action.ChoiceID != "conversation-2" {
		t.Fatalf("picker selection action = %#v", action)
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
