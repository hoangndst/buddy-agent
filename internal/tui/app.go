package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// FlushMsg asks the model to publish buffered streaming text.
type FlushMsg struct{}

// Model owns the protocol-neutral conversation state rendered by the TUI.
// Runtime requests and protocol ordering remain in the command adapter.
type Model struct {
	Transcript    Transcript
	Composer      *Composer
	Approvals     []ApprovalRequest
	Choices       []Choice
	Picker        PickerKind
	PickerCursor  int
	PickerGen     uint64
	Width, Height int
	Metadata      Header
	State         string

	deltaBuffer       strings.Builder
	tickerActive      bool
	activeBlockID     string
	streamingText     string
	blockSequence     uint64
	approvalModal     *ApprovalModal
	inspection        inspectionState
	screenContent     *ScreenContent
	TranscriptFocused bool
	SelectedActivity  string
	selection         transcriptSelection
}

// NewModel creates conversation presentation state for one terminal session.
func NewModel(metadata Header) *Model {
	return &Model{Metadata: metadata, Composer: NewComposer(nil), Width: 80, Height: 24, State: "ready"}
}

// StreamingText returns the accumulated text for the active response.
func (m *Model) StreamingText() string { return m.streamingText }

// ActiveBlockID returns the block receiving stream updates, if any.
func (m *Model) ActiveBlockID() string { return m.activeBlockID }

// TickerActive reports whether streaming refresh messages are scheduled.
func (m *Model) TickerActive() bool { return m.tickerActive }

// HasBufferedDelta reports whether unflushed stream text is pending.
func (m *Model) HasBufferedDelta() bool { return m.deltaBuffer.Len() > 0 }

// ClearActiveBlock forgets the current streaming block.
func (m *Model) ClearActiveBlock() { m.activeBlockID = "" }

// ResetStreaming drops transient text and closes the active streaming block.
func (m *Model) ResetStreaming() {
	m.deltaBuffer.Reset()
	m.streamingText = ""
	m.finishStreaming()
}

// Init has no startup work; the command adapter owns runtime watches.
func (m *Model) Init() tea.Cmd { return nil }

// Update reduces terminal and neutral conversation events into UI state.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.Width, m.Height = msg.Width, msg.Height
		m.selection = transcriptSelection{}
		if m.Composer != nil {
			m.Composer.Resize(msg.Width, msg.Height)
		}
		if m.approvalModal != nil {
			m.approvalModal.Resize(msg.Width, msg.Height)
		}
		m.Transcript.ClearCache()
	case tea.MouseWheelMsg:
		m.selection = transcriptSelection{}
		if m.HasInspection() {
			return m, m.updateInspectionWheel(msg)
		}
		switch msg.Button {
		case tea.MouseWheelUp:
			m.Transcript.ScrollOffset += 3
		case tea.MouseWheelDown:
			m.Transcript.ScrollOffset = max(0, m.Transcript.ScrollOffset-3)
		}
	case FlushMsg:
		m.clearTranscriptSelectionIfIdle()
		m.tickerActive = false
		m.flushDelta()
	case Event:
		m.clearTranscriptSelectionIfIdle()
		return m, m.reduce(msg)
	case tea.KeyPressMsg:
		if request := m.ActiveApproval(); request != nil && m.approvalModal != nil {
			choice, done, cmd := m.approvalModal.HandleKey(msg, *request)
			if done && choice != nil {
				return m, EmitAction(Action{Kind: ActionPermissionChoice, ApprovalGeneration: request.Generation, ApprovalChoice: *choice})
			}
			return m, cmd
		}
		if m.HasPicker() {
			switch msg.String() {
			case "up", "k":
				m.PickerCursor = max(0, m.PickerCursor-1)
			case "down", "j":
				m.PickerCursor = min(max(0, len(m.Choices)-1), m.PickerCursor+1)
			case "esc":
				picker := m.Picker
				generation := m.PickerGen
				m.closePicker()
				return m, EmitAction(Action{Kind: ActionCancelPicker, PickerKind: picker, PickerGeneration: generation})
			case "enter":
				if len(m.Choices) > 0 {
					return m, EmitAction(Action{Kind: ActionSelectChoice, PickerKind: m.Picker, PickerGeneration: m.PickerGen, ChoiceID: m.Choices[m.PickerCursor].ID})
				}
			}
			return m, nil
		}
		if m.HasInspection() {
			return m.updateInspectionKey(msg)
		}
		if msg.String() == "ctrl+o" {
			m.toggleActivityGroups()
			return m, nil
		}
		if msg.String() == "f6" {
			m.TranscriptFocused = !m.TranscriptFocused
			if m.TranscriptFocused {
				m.Composer.Editor.Blur()
				m.selectActivity(0)
			} else {
				m.Composer.Editor.Focus()
				m.invalidateActivity(m.SelectedActivity)
			}
			return m, nil
		}
		if m.TranscriptFocused {
			switch msg.String() {
			case "up":
				m.selectActivity(-1)
			case "down":
				m.selectActivity(1)
			case "enter":
				return m, m.activateSelectedActivity()
			case "pgup":
				m.Transcript.ScrollOffset += max(1, m.Height/2)
			case "pgdown":
				m.Transcript.ScrollOffset = max(0, m.Transcript.ScrollOffset-max(1, m.Height/2))
			}
			return m, nil
		}
		if m.Composer.CompletionKind() != "" {
			switch msg.String() {
			case "esc":
				m.Composer.ClearCompletion()
				return m, nil
			case "up", "down":
				step := -1
				if msg.String() == "down" {
					step = 1
				}
				m.Composer.MoveCompletion(step)
				return m, nil
			case "tab", "enter":
				if m.Composer.AcceptCompletion(msg.String() == "enter") {
					return m, nil
				}
			}
		}
		if m.Composer.HasHistory() {
			switch msg.String() {
			case "ctrl+p":
				if prompt, ok := m.Composer.Previous(m.Composer.Editor.Value()); ok {
					m.Composer.SetValue(prompt, m.Width, m.Height)
					m.Composer.ClearCompletion()
				}
				return m, nil
			case "ctrl+n":
				if prompt, ok := m.Composer.Next(); ok {
					m.Composer.SetValue(prompt, m.Width, m.Height)
					m.Composer.ClearCompletion()
				}
				return m, nil
			case "up":
				lineInfo := m.Composer.Editor.LineInfo()
				if m.Composer.Editor.Line() == 0 && lineInfo.RowOffset == 0 {
					if prompt, ok := m.Composer.Previous(m.Composer.Editor.Value()); ok {
						m.Composer.SetValue(prompt, m.Width, m.Height)
						m.Composer.ClearCompletion()
						return m, nil
					}
				}
			case "down":
				lineInfo := m.Composer.Editor.LineInfo()
				if m.Composer.Editor.Line() == m.Composer.Editor.LineCount()-1 && lineInfo.RowOffset+1 >= lineInfo.Height {
					if prompt, ok := m.Composer.Next(); ok {
						m.Composer.SetValue(prompt, m.Width, m.Height)
						m.Composer.ClearCompletion()
						return m, nil
					}
				}
			}
		}
		switch msg.String() {
		case "pgup":
			m.Transcript.ScrollOffset += max(1, m.Height/2)
			return m, nil
		case "pgdown":
			m.Transcript.ScrollOffset = max(0, m.Transcript.ScrollOffset-max(1, m.Height/2))
			return m, nil
		case "end":
			m.Transcript.ScrollOffset = 0
			return m, nil
		case "enter":
			prompt := strings.TrimSpace(m.Composer.Editor.Value())
			if prompt == "" {
				return m, nil
			}
			m.Composer.Reset(m.Width, m.Height)
			return m, EmitAction(Action{Kind: ActionSubmitPrompt, Prompt: prompt})
		}
	case tea.PasteMsg:
		if m.approvalModal != nil && m.approvalModal.Editing() {
			return m, m.approvalModal.UpdateEditor(msg)
		}
		if m.HasPicker() || m.HasInspection() {
			return m, nil
		}
	}
	return m, m.Composer.Update(msg, m.Width, m.Height, m.Metadata.Workspace)
}

// View renders the transcript model without any runtime or command state.
func (m *Model) View() tea.View {
	m.Transcript.SetRenderState(m.streamingText, m.SelectedActivity, m.TranscriptFocused)
	if m.screenContent != nil {
		content := m.screenContent
		body, footer := content.Body, content.Footer
		if m.HasPicker() && !m.HasPendingApproval() {
			footer = nil
			if m.Width >= 40 && m.Height >= 12 {
				footer = []string{mutedStyle.Render("↑/↓ select · Enter choose · Esc back")}
			}
			body = m.PickerView(m.Height - len(content.Header) - len(footer))
		} else if m.HasInspection() && !m.HasPendingApproval() {
			footer = nil
			body = m.InspectionView(max(0, m.Height-len(content.Header)))
		} else if content.Body == nil {
			available := max(0, m.Height-len(content.Header)-len(footer))
			if len(m.Transcript.Blocks) == 0 && m.streamingText == "" {
				if m.State == "running" {
					body = []string{mutedStyle.Render("Working…")}
				} else {
					status := content.WelcomeStatus
					if status == "" {
						status = "Connected"
					}
					body = RenderWelcome(Welcome{Width: m.Width, Height: available, Model: m.Metadata.Model, Workspace: m.Metadata.Workspace, GitBranch: m.Metadata.GitBranch, Version: content.Version, Status: status})
				}
			} else {
				body = m.renderTranscriptSelection(m.transcriptLines(available))
			}
		}
		return RenderScreen(Screen{Width: m.Width, Height: m.Height, Header: content.Header, Body: body, Footer: footer})
	}
	body := m.renderTranscriptSelection(m.transcriptLines(max(0, m.Height-2)))
	return RenderScreen(Screen{
		Width: m.Width, Height: m.Height,
		Header: RenderHeader(Header{Width: m.Width, Model: m.Metadata.Model, Workspace: m.Metadata.Workspace, GitBranch: m.Metadata.GitBranch, State: m.State, Standard: m.Metadata.Standard}),
		Body:   body,
	})
}

// SetScreenContent supplies rows the runtime adapter derives from daemon state.
func (m *Model) SetScreenContent(content ScreenContent) {
	if previous := m.screenContent; previous != nil && len(previous.Header)+len(previous.Footer) != len(content.Header)+len(content.Footer) {
		m.clearTranscriptSelectionIfIdle()
	}
	m.screenContent = &content
}

// ActiveApproval returns the oldest pending approval shown by the modal.
func (m *Model) ActiveApproval() *ApprovalRequest {
	if len(m.Approvals) == 0 {
		return nil
	}
	return &m.Approvals[0]
}

// HasPendingApproval reports whether approval interaction currently has focus.
func (m *Model) HasPendingApproval() bool { return len(m.Approvals) > 0 }

// HasPicker reports whether a workspace or conversation picker is active.
func (m *Model) HasPicker() bool { return m.Picker != PickerNone }

// HasInspection reports whether an output or diff inspector is active.
func (m *Model) HasInspection() bool { return m.inspection.Kind != InspectionNone }

// HyperlinkAt resolves a screen coordinate to a visible Markdown link.
func (m *Model) HyperlinkAt(row, column int) string {
	if m.HasPicker() || m.HasInspection() || m.HasPendingApproval() || m.screenContent == nil || m.screenContent.Body != nil {
		return ""
	}
	headerRows, footerRows := len(m.screenContent.Header), len(m.screenContent.Footer)
	bodyHeight := max(0, m.Height-headerRows-footerRows)
	return m.Transcript.HyperlinkAt(row-headerRows, column, bodyHeight, m.Width)
}

// InspectionKind returns the currently active inspector type.
func (m *Model) InspectionKind() InspectionKind { return m.inspection.Kind }

// InspectionRef returns the output reference displayed by the active output inspector.
func (m *Model) InspectionRef() string { return m.inspection.Ref }

// CurrentOutputPage returns the output page currently displayed by the model.
func (m *Model) CurrentOutputPage() *OutputPage { return m.inspection.Output }

func (m *Model) selectActivity(delta int) {
	activities := make([]string, 0)
	for _, block := range m.Transcript.Blocks {
		if block.Kind == BlockActivityGroup {
			activities = append(activities, block.ID)
			continue
		}
		if block.Kind != BlockToolActivity && block.Kind != BlockSubagentActivity {
			continue
		}
		if block.ParentID != "" && parentActivityIsExpanded(block, m.Transcript.Blocks) || block.ParentID == "" && (block.GroupID == "" || activityGroupIsExpanded(block, m.Transcript.Blocks)) {
			activities = append(activities, block.ID)
		}
	}
	if len(activities) == 0 {
		m.invalidateActivity(m.SelectedActivity)
		m.SelectedActivity = ""
		m.Transcript.SetRenderState(m.streamingText, "", true)
		return
	}
	index := 0
	for i, id := range activities {
		if id == m.SelectedActivity {
			index = i
			break
		}
	}
	previous := m.SelectedActivity
	m.SelectedActivity = activities[max(0, min(len(activities)-1, index+delta))]
	m.invalidateActivity(previous)
	m.invalidateActivity(m.SelectedActivity)
	m.Transcript.SetRenderState(m.streamingText, m.SelectedActivity, true)
	selected := m.Transcript.Index(m.SelectedActivity)
	if selected < 0 {
		return
	}
	container := m.activityContainerID(m.Transcript.Blocks[selected])
	target := m.SelectedActivity
	if container != "" {
		target = container
	}
	viewportHeight := max(1, m.Height-len(m.screenContentHeader())-len(m.screenContentFooter()))
	rowsAfter := 0
	for i := len(m.Transcript.Blocks) - 1; i >= 0; i-- {
		block := m.Transcript.Blocks[i]
		count := m.Transcript.LineCount(block, m.Width)
		if block.ID == target {
			row := count / 2
			if container != "" {
				rendered, childOffset := RenderBlock(block, m.Transcript.Blocks, m.Width, m.streamingText, m.SelectedActivity, true)
				if childOffset >= 0 {
					row = len(WrapLines(rendered[:childOffset], m.Width))
				}
			}
			m.Transcript.ScrollOffset = max(0, rowsAfter+count-row-viewportHeight/2)
			break
		}
		rowsAfter += count
	}
}

func (m *Model) screenContentHeader() []string {
	if m.screenContent == nil {
		return nil
	}
	return m.screenContent.Header
}
func (m *Model) screenContentFooter() []string {
	if m.screenContent == nil {
		return nil
	}
	return m.screenContent.Footer
}

func (m *Model) invalidateActivity(id string) {
	if id == "" {
		return
	}
	if index := m.Transcript.Index(id); index >= 0 {
		m.Transcript.Invalidate(id)
		if container := m.activityContainerID(m.Transcript.Blocks[index]); container != "" {
			m.Transcript.Invalidate(container)
		}
	}
}

func (m *Model) activityContainerID(block TranscriptBlock) string {
	if block.ParentID == "" {
		return block.GroupID
	}
	if index := m.Transcript.Index(block.ParentID); index >= 0 && m.Transcript.Blocks[index].GroupID != "" {
		return m.Transcript.Blocks[index].GroupID
	}
	return block.ParentID
}

func parentActivityIsExpanded(block TranscriptBlock, blocks []TranscriptBlock) bool {
	for _, parent := range blocks {
		if parent.ID == block.ParentID {
			return !parent.Collapsed && (parent.GroupID == "" || activityGroupIsExpanded(parent, blocks))
		}
	}
	return false
}

func activityGroupIsExpanded(block TranscriptBlock, blocks []TranscriptBlock) bool {
	for _, group := range blocks {
		if group.ID == block.GroupID {
			return !group.Collapsed
		}
	}
	return false
}

func (m *Model) activateSelectedActivity() tea.Cmd {
	m.Transcript.SetRenderState(m.streamingText, m.SelectedActivity, m.TranscriptFocused)
	for index, block := range m.Transcript.Blocks {
		if block.ID != m.SelectedActivity {
			continue
		}
		if block.Kind == BlockActivityGroup {
			m.setActivityGroupCollapsed(index, !block.Collapsed)
			return nil
		}
		if block.Activity != nil && block.Activity.OutputRef != "" {
			return EmitAction(Action{Kind: ActionOpenOutput, Ref: block.Activity.OutputRef})
		}
		m.Transcript.Update(index, func(current *TranscriptBlock) { current.Collapsed = !current.Collapsed }, m.Width)
		return nil
	}
	return nil
}

func (m *Model) toggleActivityGroups() {
	allExpanded := true
	for _, block := range m.Transcript.Blocks {
		if block.Kind == BlockActivityGroup && block.Collapsed {
			allExpanded = false
			break
		}
	}
	for index := range m.Transcript.Blocks {
		if m.Transcript.Blocks[index].Kind == BlockActivityGroup {
			m.setActivityGroupCollapsed(index, allExpanded)
		}
	}
}

func (m *Model) setActivityGroupCollapsed(index int, collapsed bool) {
	groupID := m.Transcript.Blocks[index].ID
	m.Transcript.Update(index, func(block *TranscriptBlock) { block.Collapsed = collapsed }, m.Width)
	for i := range m.Transcript.Blocks {
		if m.Transcript.Blocks[i].GroupID == groupID {
			m.Transcript.Update(i, func(block *TranscriptBlock) { block.Collapsed = collapsed }, m.Width)
		}
	}
}

// InspectionView renders the active inspector within the supplied row budget.
func (m *Model) InspectionView(height int) []string { return m.inspection.view(m.Width, height) }

// PickerView renders the active picker within the supplied row budget.
func (m *Model) PickerView(height int) []string {
	if !m.HasPicker() {
		return nil
	}
	label := "Choose a project"
	if m.Picker == PickerConversation {
		label = "Choose a conversation"
	}
	items := make([]string, len(m.Choices))
	for i, choice := range m.Choices {
		items[i] = choice.Label
	}
	return RenderChoiceList(ChoiceList{Label: label, Items: items, Cursor: m.PickerCursor, Height: height})
}

// ApprovalView renders the active approval within the supplied footer area.
func (m *Model) ApprovalView(height int) string {
	request := m.ActiveApproval()
	if request == nil || m.approvalModal == nil {
		return ""
	}
	modalHeight := min(max(2, m.Height/2), max(2, m.Height))
	return m.approvalModal.View(*request, m.Width, min(height, modalHeight))
}

// ApprovalActionRow renders the active approval's keyboard choices.
func (m *Model) ApprovalActionRow() string {
	request := m.ActiveApproval()
	if request == nil || m.approvalModal == nil {
		return "Resize terminal to decide"
	}
	return m.approvalModal.ActionRow(*request, max(1, m.Width))
}

// ApprovalEditing reports whether the active approval editor is open.
func (m *Model) ApprovalEditing() bool {
	return m.approvalModal != nil && m.approvalModal.Editing()
}

func (m *Model) reduce(event Event) tea.Cmd {
	at := event.At
	if at.IsZero() {
		at = time.Now()
	}
	switch event.Kind {
	case EventSnapshot:
		m.flushDelta()
		m.activeBlockID, m.streamingText, m.tickerActive = "", "", false
		m.Transcript.Replace(event.Blocks)
	case EventApprovalSnapshot:
		m.setApprovals(event.Approvals)
	case EventApprovalPending:
		if event.Approval != nil {
			approvals := append([]ApprovalRequest(nil), m.Approvals...)
			found := false
			for i := range approvals {
				if approvals[i].Generation == event.Approval.Generation {
					approvals[i], found = *event.Approval, true
					break
				}
			}
			if !found {
				approvals = append(approvals, *event.Approval)
			}
			m.setApprovals(approvals)
		}
	case EventApprovalResolved:
		approvals := make([]ApprovalRequest, 0, len(m.Approvals))
		for _, approval := range m.Approvals {
			if approval.Generation != event.ApprovalGeneration {
				approvals = append(approvals, approval)
			}
		}
		m.setApprovals(approvals)
	case EventPickerSnapshot:
		m.Picker = event.PickerKind
		m.PickerGen = event.PickerGeneration
		m.Choices = append(m.Choices[:0], event.Choices...)
		m.PickerCursor = 0
	case EventPickerGeneration:
		m.PickerGen = event.PickerGeneration
	case EventPickerClosed:
		m.closePicker()
	case EventInspectionStart:
		m.inspection = inspectionState{Kind: event.InspectionKind, Generation: event.InspectionGeneration, Ref: event.InspectionRef, Loading: true}
	case EventInspectionPageLoading:
		if m.inspection.Generation == event.InspectionGeneration {
			m.inspection.Loading, m.inspection.Error = true, ""
			m.inspection.Scroll, m.inspection.Column = 0, 0
		}
	case EventOutputPage:
		if m.inspection.Generation == event.InspectionGeneration && m.inspection.Kind == InspectionOutput {
			m.inspection.Output = event.OutputPage
			m.inspection.Loading, m.inspection.Error = false, ""
		}
	case EventDiffResult:
		if m.inspection.Generation == event.InspectionGeneration && m.inspection.Kind == InspectionDiff {
			m.inspection.Diff = event.DiffView
			m.inspection.Loading, m.inspection.Error = false, ""
			m.inspection.SelectedFile, m.inspection.FileOpen, m.inspection.Scroll, m.inspection.Column = 0, false, 0, 0
		}
	case EventInspectionFailure:
		if m.inspection.Generation == event.InspectionGeneration {
			m.inspection.Loading, m.inspection.Error = false, event.InspectionError
		}
	case EventInspectionClosed:
		if event.InspectionGeneration == 0 || m.inspection.Generation == event.InspectionGeneration {
			m.closeInspection()
		}
	case EventAssistantDelta:
		if event.Text == "" {
			return nil
		}
		m.deltaBuffer.WriteString(event.Text)
		if m.activeBlockID == "" {
			block := TranscriptBlock{ID: m.newBlockID(), TurnID: event.TurnID, Kind: BlockAssistantMessage, Role: "assistant", Header: "Buddy", Streaming: true, CreatedAt: at}
			m.Transcript.Append(block, m.Width)
			m.activeBlockID = block.ID
		}
		if !m.tickerActive {
			m.tickerActive = true
			return tea.Tick(33*time.Millisecond, func(time.Time) tea.Msg { return FlushMsg{} })
		}
	case EventAssistantComplete:
		m.flushDelta()
		content := event.Text
		if content == "" {
			content = m.streamingText
		}
		m.streamingText, m.tickerActive = "", false
		if i := m.Transcript.Index(m.activeBlockID); i >= 0 {
			m.Transcript.Update(i, func(block *TranscriptBlock) {
				block.Streaming = false
				if content != "" {
					block.Body = content
				}
			}, m.Width)
			m.activeBlockID = ""
		} else if content != "" {
			m.Transcript.Append(TranscriptBlock{ID: m.newBlockID(), TurnID: event.TurnID, Kind: BlockAssistantMessage, Role: "assistant", Header: "Buddy", Body: content, CreatedAt: at, Activity: event.Activity}, m.Width)
		}
	case EventReasoning:
		// Reasoning events are model-internal; show progress through public activity instead.
	case EventActivityStarted:
		if event.Activity == nil {
			return nil
		}
		a := *event.Activity
		a.Tool = cleanText(a.Tool)
		a.Summary = cleanText(a.Summary)
		a.Path = cleanText(a.Path)
		kind := BlockToolActivity
		if a.ParentIdentity != "" {
			kind = BlockSubagentActivity
		}
		header := a.Tool
		if header == "" {
			header = "tool"
		}
		if a.Path != "" {
			header += " " + FormatWorkspaceFileHyperlink(m.Metadata.Workspace, a.Path, a.Line)
		}
		groupID := ""
		if a.ParentIdentity == "" {
			groupID = m.activityGroup(event.TurnID, a.Step, at)
		}
		block := TranscriptBlock{ID: m.newBlockID(), TurnID: event.TurnID, GroupID: groupID, Step: a.Step, Kind: kind, Role: "tool", Header: "Running " + header, CreatedAt: at, Activity: &a}
		if kind == BlockSubagentActivity {
			block.Collapsed = true
			if parent := m.activityBlock(a.ParentIdentity); parent >= 0 {
				block.ParentID = m.Transcript.Blocks[parent].ID
				m.Transcript.Update(parent, func(b *TranscriptBlock) { b.Collapsed = true }, m.Width)
			} else {
				block.Header = "Detached child · " + block.Header
			}
		}
		m.Transcript.Append(block, m.Width)
	case EventActivityCompleted:
		if event.Activity == nil {
			return nil
		}
		activity := *event.Activity
		activity.Tool = cleanText(activity.Tool)
		activity.Summary = cleanText(activity.Summary)
		activity.Path = cleanText(activity.Path)
		matched := false
		for i := len(m.Transcript.Blocks) - 1; i >= 0; i-- {
			block := m.Transcript.Blocks[i]
			if event.TurnID != "" && block.TurnID != event.TurnID || block.Activity == nil || activity.Identity == "" || block.Activity.Identity != activity.Identity {
				continue
			}
			m.completeActivity(i, event.Text, activity)
			matched = true
			break
		}
		if !matched {
			if activity.Identity == "" {
				if candidate := m.unambiguousActivity(event.TurnID, activity); candidate >= 0 {
					m.completeActivity(candidate, event.Text, activity)
					matched = true
				}
			}
			if !matched && (event.Text != "" || activity.ExitCode != nil) {
				kind := BlockToolActivity
				if activity.ParentIdentity != "" {
					kind = BlockSubagentActivity
				}
				tool := activity.Tool
				if tool == "" {
					tool = "tool"
				}
				mark := "Done"
				if activity.ExitCode != nil && *activity.ExitCode != 0 {
					mark = "Failed"
				}
				groupID := ""
				if activity.ParentIdentity == "" {
					groupID = m.activityGroup(event.TurnID, activity.Step, at)
				}
				block := TranscriptBlock{ID: m.newBlockID(), TurnID: event.TurnID, GroupID: groupID, Step: activity.Step, Kind: kind, Role: "tool", Header: mark + " " + tool, Body: event.Text, CreatedAt: at, Collapsed: len(event.Text) > 500 || activity.OutputRef != "", Completed: true, Activity: &activity}
				if kind == BlockSubagentActivity {
					if parent := m.activityBlock(activity.ParentIdentity); parent >= 0 {
						block.ParentID = m.Transcript.Blocks[parent].ID
						m.Transcript.Update(parent, func(b *TranscriptBlock) { b.Collapsed = true }, m.Width)
					} else {
						block.Header = "Detached child · " + block.Header
					}
				}
				m.Transcript.Append(block, m.Width)
			}
		}
	case EventTurnFinished:
		m.flushDelta()
		m.finishStreaming()
		m.streamingText = ""
	case EventFailure:
		m.deltaBuffer.Reset()
		m.streamingText = ""
		m.finishStreaming()
	case EventCompactionStatus:
		m.Transcript.Append(TranscriptBlock{ID: m.newBlockID(), TurnID: event.TurnID, Kind: BlockSystemStatus, Role: "system", Header: "System", Body: event.Text, CreatedAt: at}, m.Width)
	}
	return nil
}

func (m *Model) closePicker() {
	m.Picker = PickerNone
	m.PickerGen = 0
	m.Choices = nil
	m.PickerCursor = 0
}

func (m *Model) setApprovals(approvals []ApprovalRequest) {
	oldGeneration := uint64(0)
	if active := m.ActiveApproval(); active != nil {
		oldGeneration = active.Generation
	}
	m.Approvals = append(m.Approvals[:0], approvals...)
	if len(m.Approvals) == 0 {
		m.approvalModal = nil
		return
	}
	if m.Approvals[0].Generation != oldGeneration || m.approvalModal == nil {
		m.approvalModal = NewApprovalModal(m.Approvals[0].ArgumentsJSON, m.Width, m.Height)
	}
}

func (m *Model) flushDelta() {
	if m.deltaBuffer.Len() == 0 {
		return
	}
	text := m.deltaBuffer.String()
	m.deltaBuffer.Reset()
	if i := m.Transcript.Index(m.activeBlockID); i >= 0 {
		m.Transcript.Update(i, func(block *TranscriptBlock) { block.Body += text }, m.Width)
	}
	m.streamingText += text
}

func (m *Model) finishStreaming() {
	if i := m.Transcript.Index(m.activeBlockID); i >= 0 {
		m.Transcript.Update(i, func(block *TranscriptBlock) { block.Streaming = false }, m.Width)
	}
	m.activeBlockID, m.tickerActive = "", false
}

func (m *Model) activityBlock(identity string) int {
	if identity == "" {
		return -1
	}
	for i := range m.Transcript.Blocks {
		if m.Transcript.Blocks[i].Activity != nil && m.Transcript.Blocks[i].Activity.Identity == identity {
			return i
		}
	}
	return -1
}

func (m *Model) activityGroup(turnID string, step int, at time.Time) string {
	if step > 0 {
		for i := range m.Transcript.Blocks {
			group := m.Transcript.Blocks[i]
			if group.Kind == BlockActivityGroup && group.TurnID == turnID && group.Step == step {
				m.Transcript.Invalidate(group.ID)
				return group.ID
			}
		}
	}
	id := m.newBlockID()
	m.Transcript.Append(TranscriptBlock{ID: id, TurnID: turnID, Kind: BlockActivityGroup, Step: step, Header: "Tool activity", Collapsed: true, CreatedAt: at}, m.Width)
	return id
}

func (m *Model) completeActivity(index int, output string, activity Activity) {
	if started := m.Transcript.Blocks[index].Activity; started != nil {
		if activity.Identity == "" {
			activity.Identity = started.Identity
		}
		if activity.Summary == "" {
			activity.Summary = started.Summary
		}
	}
	if activity.Step == 0 {
		activity.Step = m.Transcript.Blocks[index].Step
	}
	m.Transcript.Update(index, func(block *TranscriptBlock) {
		block.Body, block.Activity, block.Completed, block.Collapsed = output, &activity, true, true
		name, mark := activity.Tool, "Done"
		if activity.ExitCode != nil && *activity.ExitCode != 0 {
			mark = "Failed"
		}
		if name != "" {
			block.Header = mark + " " + name
		}
	}, m.Width)
}

func (m *Model) unambiguousActivity(turnID string, activity Activity) int {
	candidate := -1
	for i := len(m.Transcript.Blocks) - 1; i >= 0; i-- {
		block := m.Transcript.Blocks[i]
		if block.Activity == nil || block.Completed || activity.Tool == "" || block.Activity.Tool != activity.Tool || block.Activity.ParentIdentity != activity.ParentIdentity || turnID != "" && block.TurnID != turnID {
			continue
		}
		step := activity.Step
		if step == 0 {
			step = block.Step
		}
		if step > 0 && block.Step != step {
			continue
		}
		if candidate >= 0 {
			return -1
		}
		candidate = i
	}
	return candidate
}

func (m *Model) newBlockID() string {
	m.blockSequence++
	return fmt.Sprintf("ui-block-%d", m.blockSequence)
}
