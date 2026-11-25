package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
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

func TestCtrlOTogglesAllActivityGroupsWithoutChangingComposer(t *testing.T) {
	m := NewModel(Header{})
	m.Transcript.Append(TranscriptBlock{ID: "group-a", Kind: BlockActivityGroup, Collapsed: true}, 80)
	m.Transcript.Append(TranscriptBlock{ID: "group-b", Kind: BlockActivityGroup, Collapsed: true}, 80)
	m.Composer.Editor.SetValue("draft")
	m.Update(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	for _, block := range m.Transcript.Blocks {
		if block.Kind == BlockActivityGroup && block.Collapsed {
			t.Fatalf("Ctrl+O did not expand %q", block.ID)
		}
	}
	if m.Composer.Editor.Value() != "draft" {
		t.Fatal("Ctrl+O changed the composer draft")
	}
	m.Update(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	for _, block := range m.Transcript.Blocks {
		if block.Kind == BlockActivityGroup && !block.Collapsed {
			t.Fatalf("second Ctrl+O did not collapse %q", block.ID)
		}
	}
}

func TestModelResolvesVisibleMarkdownLinkScreenCoordinates(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	m := NewModel(Header{})
	m.Width, m.Height = 80, 12
	m.SetScreenContent(ScreenContent{Header: []string{"header"}, Footer: []string{"footer"}})
	m.Transcript.Append(TranscriptBlock{ID: "answer", Kind: BlockAssistantMessage, Body: "[Open Buddy](https://example.com/docs)"}, m.Width)
	plain := ansi.Strip(m.Transcript.WrappedLines(m.Transcript.Blocks[0], m.Width)[0])
	column := strings.Index(plain, "Open Buddy") + 1
	if got := m.HyperlinkAt(1, column); got != "https://example.com/docs" {
		t.Fatalf("screen hyperlink = %q, want URL", got)
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

func TestModelClearsActivityHighlightInScreenContentAfterF6(t *testing.T) {
	m := NewModel(Header{})
	m.Width, m.Height = 60, 16
	m.Transcript.Append(TranscriptBlock{
		ID: "activity", Kind: BlockToolActivity, Header: "bash", Collapsed: true,
		Activity: &Activity{Tool: "bash"},
	}, m.Width)
	m.SetScreenContent(ScreenContent{Header: []string{"header"}, Footer: []string{"footer"}})

	m.Update(tea.KeyPressMsg{Code: tea.KeyF6})
	if view := m.View().Content; !strings.Contains(view, "›") {
		t.Fatalf("selected activity was not highlighted while transcript had focus: %q", view)
	}
	m.Update(tea.KeyPressMsg{Code: tea.KeyF6})
	if view := m.View().Content; strings.Contains(view, "›") {
		t.Fatalf("activity highlight remained after focus returned to composer: %q", view)
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
	started := &Activity{Identity: "turn\x001\x00call", Tool: "read_file", Path: "main.go", Step: 1, StartedAt: time.Now()}
	updateEvent(m, Event{Kind: EventActivityStarted, TurnID: "turn", Activity: started})
	if len(m.Transcript.Blocks) != 2 || m.Transcript.Blocks[0].Kind != BlockActivityGroup || m.Transcript.Blocks[1].Activity.Identity != started.Identity || m.Transcript.Blocks[1].GroupID != m.Transcript.Blocks[0].ID {
		t.Fatalf("activity start was not recorded: %+v", m.Transcript.Blocks)
	}
	completed := *started
	code := 0
	completed.ExitCode, completed.Elapsed = &code, time.Second
	updateEvent(m, Event{Kind: EventActivityCompleted, TurnID: "turn", Activity: &completed, Text: "file contents"})
	if len(m.Transcript.Blocks) != 2 || !m.Transcript.Blocks[1].Completed || !m.Transcript.Blocks[1].Collapsed || m.Transcript.Blocks[1].Body != "file contents" || !strings.Contains(m.Transcript.Blocks[1].Header, "Done read_file") {
		t.Fatalf("activity result did not update its start block: %+v", m.Transcript.Blocks)
	}
}

func TestModelSanitizesActivityToolAndPathBeforeFormatting(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "")
	m := NewModel(Header{Workspace: "/work"})
	activity := &Activity{
		Identity: "turn\x001\x00call",
		Tool:     "bash\x1b[31m",
		Path:     "safe.go\x1b[31m",
		Line:     9,
	}
	updateEvent(m, Event{Kind: EventActivityStarted, TurnID: "turn", Activity: activity})
	if len(m.Transcript.Blocks) != 2 {
		t.Fatalf("activity start was not recorded: %+v", m.Transcript.Blocks)
	}
	header := m.Transcript.Blocks[1].Header
	if got := StripHyperlinks(header); got != "Running bash safe.go:9" {
		t.Fatalf("sanitized activity header = %q", got)
	}
	if !strings.Contains(header, "file:///work/safe.go#L9") {
		t.Fatalf("safe path lost its intended hyperlink: %q", header)
	}
	if strings.Contains(StripHyperlinks(header), "\x1b") {
		t.Fatalf("terminal control sequence remained in activity header: %q", header)
	}

	completed := *activity
	completed.ExitCode = new(int)
	updateEvent(m, Event{Kind: EventActivityCompleted, TurnID: "turn", Activity: &completed})
	if got := StripHyperlinks(m.Transcript.Blocks[1].Header); got != "Done bash" {
		t.Fatalf("completed tool header was not sanitized: %q", got)
	}
}

func TestModelGroupsActivitiesByTurnAndStep(t *testing.T) {
	m := NewModel(Header{})
	for _, activity := range []*Activity{
		{Identity: "turn\x001\x00a", Tool: "read_file", Step: 1},
		{Identity: "turn\x001\x00b", Tool: "list_directory", Step: 1},
		{Identity: "turn\x002\x00c", Tool: "bash", Step: 2},
	} {
		updateEvent(m, Event{Kind: EventActivityStarted, TurnID: "turn", Activity: activity})
	}
	if len(m.Transcript.Blocks) != 5 {
		t.Fatalf("want two groups and three activities, got %+v", m.Transcript.Blocks)
	}
	if m.Transcript.Blocks[1].GroupID != m.Transcript.Blocks[0].ID || m.Transcript.Blocks[2].GroupID != m.Transcript.Blocks[0].ID {
		t.Fatalf("same-step calls were not grouped: %+v", m.Transcript.Blocks)
	}
	if m.Transcript.Blocks[3].Kind != BlockActivityGroup || m.Transcript.Blocks[3].Step != 2 || m.Transcript.Blocks[4].GroupID != m.Transcript.Blocks[3].ID {
		t.Fatalf("different step reused the previous group: %+v", m.Transcript.Blocks)
	}
}

func TestModelDoesNotGroupActivitiesWithoutStepMetadata(t *testing.T) {
	m := NewModel(Header{})
	for _, id := range []string{"call-1", "call-2"} {
		updateEvent(m, Event{Kind: EventActivityStarted, TurnID: "turn", Activity: &Activity{Identity: id, Tool: "read_file"}})
	}
	if len(m.Transcript.Blocks) != 4 || m.Transcript.Blocks[0].ID == m.Transcript.Blocks[2].ID || m.Transcript.Blocks[1].GroupID == m.Transcript.Blocks[3].GroupID {
		t.Fatalf("calls without step metadata were merged: %+v", m.Transcript.Blocks)
	}
}

func TestModelPairsIdentitylessCompletionOnlyWhenUnambiguous(t *testing.T) {
	t.Run("single candidate", func(t *testing.T) {
		m := NewModel(Header{})
		updateEvent(m, Event{Kind: EventActivityStarted, TurnID: "turn", Activity: &Activity{Identity: "call-1", Tool: "read_file", Step: 3}})
		updateEvent(m, Event{Kind: EventActivityCompleted, TurnID: "turn", Activity: &Activity{Tool: "read_file", Step: 3}, Text: "contents"})
		if len(m.Transcript.Blocks) != 2 || !m.Transcript.Blocks[1].Completed || m.Transcript.Blocks[1].Body != "contents" || m.Transcript.Blocks[1].Activity.Identity != "call-1" {
			t.Fatalf("identity-less result did not update its unique start: %+v", m.Transcript.Blocks)
		}
	})
	t.Run("ambiguous candidates stay separate", func(t *testing.T) {
		m := NewModel(Header{})
		for _, id := range []string{"call-1", "call-2"} {
			updateEvent(m, Event{Kind: EventActivityStarted, TurnID: "turn", Activity: &Activity{Identity: id, Tool: "read_file", Step: 3}})
		}
		updateEvent(m, Event{Kind: EventActivityCompleted, TurnID: "turn", Activity: &Activity{Tool: "read_file", Step: 3, ExitCode: new(int)}, Text: "contents"})
		if len(m.Transcript.Blocks) != 4 || m.Transcript.Blocks[1].Completed || m.Transcript.Blocks[2].Completed || !m.Transcript.Blocks[3].Completed || m.Transcript.Blocks[3].Header != "Done read_file" {
			t.Fatalf("ambiguous result was incorrectly paired or unlabeled: %+v", m.Transcript.Blocks)
		}
	})
}

func TestModelDoesNotGuessMismatchedIdentityOrRenderReasoning(t *testing.T) {
	m := NewModel(Header{})
	updateEvent(m, Event{Kind: EventActivityStarted, TurnID: "turn", Activity: &Activity{Identity: "call-1", Tool: "read_file", Step: 1}})
	updateEvent(m, Event{Kind: EventActivityCompleted, TurnID: "turn", Activity: &Activity{Identity: "other-call", Tool: "read_file", Step: 1, ExitCode: new(int)}, Text: "result"})
	updateEvent(m, Event{Kind: EventReasoning, TurnID: "turn", Text: "private reasoning text"})
	if len(m.Transcript.Blocks) != 3 || m.Transcript.Blocks[1].Completed || m.Transcript.Blocks[2].Header != "Done read_file" {
		t.Fatalf("mismatched identity was guessed or orphan result was not labeled: %+v", m.Transcript.Blocks)
	}
	if strings.Contains(m.View().Content, "private reasoning text") {
		t.Fatal("raw reasoning content appeared in the TUI")
	}
}

func TestModelDoesNotAppendBlankUnmatchedCompletion(t *testing.T) {
	m := NewModel(Header{})
	updateEvent(m, Event{Kind: EventActivityStarted, TurnID: "turn", Activity: &Activity{Identity: "call-1", Tool: "read_file", Step: 1}})
	updateEvent(m, Event{Kind: EventActivityCompleted, TurnID: "turn", Activity: &Activity{Identity: "other-call", Tool: "read_file", Step: 1}})
	if len(m.Transcript.Blocks) != 2 || m.Transcript.Blocks[1].Completed {
		t.Fatalf("empty unmatched completion created a blank activity row: %+v", m.Transcript.Blocks)
	}
}

func TestModelIdentitylessCompletionRequiresMatchingTurnToolStepAndParent(t *testing.T) {
	tests := []struct {
		name     string
		turn     string
		activity Activity
	}{
		{name: "different turn", turn: "other", activity: Activity{Tool: "read_file", Step: 1}},
		{name: "different tool", turn: "turn", activity: Activity{Tool: "bash", Step: 1}},
		{name: "different step", turn: "turn", activity: Activity{Tool: "read_file", Step: 2}},
		{name: "different parent", turn: "turn", activity: Activity{Tool: "read_file", Step: 1, ParentIdentity: "parent-b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewModel(Header{})
			updateEvent(m, Event{Kind: EventActivityStarted, TurnID: "turn", Activity: &Activity{Identity: "call-1", Tool: "read_file", Step: 1, ParentIdentity: "parent-a"}})
			updateEvent(m, Event{Kind: EventActivityCompleted, TurnID: tt.turn, Activity: &tt.activity, Text: "result"})
			for _, block := range m.Transcript.Blocks {
				if block.Activity != nil && block.Activity.Identity == "call-1" && block.Completed {
					t.Fatalf("identity-less completion matched an unrelated call: %+v", m.Transcript.Blocks)
				}
			}
		})
	}
}

func TestModelActivityGroupCanBeExpandedFromTranscriptFocus(t *testing.T) {
	m := NewModel(Header{})
	updateEvent(m, Event{Kind: EventActivityStarted, TurnID: "turn", Activity: &Activity{Identity: "call-1", Tool: "read_file", Step: 1}})
	m.Update(tea.KeyPressMsg{Code: tea.KeyF6})
	m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.Transcript.Blocks[0].Collapsed || !strings.Contains(m.View().Content, "read_file") {
		t.Fatalf("selected activity group did not expand: %+v", m.Transcript.Blocks)
	}
}

func TestFlushStopsUntilNewDeltaArrives(t *testing.T) {
	m := NewModel(Header{})
	_, cmd := m.Update(Event{Kind: EventAssistantDelta, TurnID: "turn", Text: "first"})
	if cmd == nil || !m.TickerActive() {
		t.Fatal("first delta did not schedule a flush")
	}
	_, cmd = m.Update(FlushMsg{})
	if cmd != nil || m.TickerActive() || m.HasBufferedDelta() {
		t.Fatalf("flush left an idle timer running: cmd=%v model=%+v", cmd, m)
	}
	_, cmd = m.Update(Event{Kind: EventAssistantDelta, TurnID: "turn", Text: " second"})
	if cmd == nil || !m.TickerActive() {
		t.Fatal("a later delta did not schedule another flush")
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
