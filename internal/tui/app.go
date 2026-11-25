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
	Width, Height int
	Metadata      Header
	State         string

	deltaBuffer   strings.Builder
	tickerActive  bool
	activeBlockID string
	streamingText string
	blockSequence uint64
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
		if m.Composer != nil {
			m.Composer.Resize(msg.Width, msg.Height)
		}
		m.Transcript.ClearCache()
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelUp:
			m.Transcript.ScrollOffset += 3
		case tea.MouseWheelDown:
			m.Transcript.ScrollOffset = max(0, m.Transcript.ScrollOffset-3)
		}
	case FlushMsg:
		m.flushDelta()
		if m.tickerActive {
			return m, tea.Tick(33*time.Millisecond, func(time.Time) tea.Msg { return FlushMsg{} })
		}
	case Event:
		return m, m.reduce(msg)
	}
	return m, nil
}

// View renders the transcript model without any runtime or command state.
func (m *Model) View() tea.View {
	m.Transcript.SetRenderState(m.streamingText, "", false)
	body := m.Transcript.VisibleLines(max(0, m.Height-2), m.Width)
	return RenderScreen(Screen{
		Width: m.Width, Height: m.Height,
		Header: RenderHeader(Header{Width: m.Width, Model: m.Metadata.Model, Workspace: m.Metadata.Workspace, GitBranch: m.Metadata.GitBranch, State: m.State, Standard: m.Metadata.Standard}),
		Body:   body,
	})
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
		if event.Text != "" {
			m.Transcript.Append(TranscriptBlock{ID: m.newBlockID(), TurnID: event.TurnID, Kind: BlockAssistantMessage, Role: "reasoning", Header: "Reasoning", Body: event.Text, CreatedAt: at, Activity: event.Activity}, m.Width)
		}
	case EventActivityStarted:
		if event.Activity == nil {
			return nil
		}
		a := *event.Activity
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
		block := TranscriptBlock{ID: m.newBlockID(), TurnID: event.TurnID, Kind: kind, Role: "tool", Header: "Running " + header, CreatedAt: at, Activity: &a}
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
		matched := false
		for i := len(m.Transcript.Blocks) - 1; i >= 0; i-- {
			block := m.Transcript.Blocks[i]
			if block.TurnID != event.TurnID || block.Activity == nil || block.Activity.Identity != activity.Identity || activity.Identity == "" {
				continue
			}
			m.Transcript.Update(i, func(block *TranscriptBlock) {
				block.Body, block.Activity, block.Completed, block.Collapsed = event.Text, &activity, true, true
				name, mark, elapsed := activity.Tool, "Done", ""
				if activity.ExitCode != nil && *activity.ExitCode != 0 {
					mark = "Failed"
				}
				if activity.Elapsed > 0 {
					elapsed = "  ·  " + activity.Elapsed.Round(100*time.Millisecond).String()
				}
				if name != "" {
					block.Header = mark + " " + name + elapsed
				}
			}, m.Width)
			matched = true
			break
		}
		if !matched && event.Text != "" {
			kind, header := BlockToolActivity, "Result"
			if activity.ParentIdentity != "" {
				kind = BlockSubagentActivity
				header = activity.Tool
				if header == "" {
					header = "tool"
				}
			}
			block := TranscriptBlock{ID: m.newBlockID(), TurnID: event.TurnID, Kind: kind, Role: "tool", Header: header, Body: event.Text, CreatedAt: at, Collapsed: len(event.Text) > 500 || activity.OutputRef != "", Completed: true, Activity: &activity}
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

func (m *Model) newBlockID() string {
	m.blockSequence++
	return fmt.Sprintf("ui-block-%d", m.blockSequence)
}
