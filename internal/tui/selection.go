package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

type transcriptPoint struct{ row, column int }

type transcriptSelection struct {
	start, end transcriptPoint
	dragging   bool
	active     bool
}

// ClearTranscriptSelection removes the visible selection and drag state.
func (m *Model) ClearTranscriptSelection() { m.selection = transcriptSelection{} }

func (m *Model) clearTranscriptSelectionIfIdle() {
	if !m.selection.dragging {
		m.ClearTranscriptSelection()
	}
}

// TranscriptSelectionInProgress reports whether a left-button drag is active.
func (m *Model) TranscriptSelectionInProgress() bool { return m.selection.dragging }

// BeginTranscriptSelection starts a drag selection at a terminal screen cell.
func (m *Model) BeginTranscriptSelection(screenRow, column int) bool {
	row, ok := m.transcriptBodyRow(screenRow)
	if !ok || column < 0 || column >= m.Width {
		m.selection = transcriptSelection{}
		return false
	}
	m.selection = transcriptSelection{start: transcriptPoint{row, column}, end: transcriptPoint{row, column}, dragging: true}
	return true
}

// MoveTranscriptSelection updates an in-progress drag.
func (m *Model) MoveTranscriptSelection(screenRow, column int) bool {
	if !m.selection.dragging {
		return false
	}
	row, column := m.clampTranscriptPoint(screenRow, column)
	m.selection.end = transcriptPoint{row, column}
	return true
}

// EndTranscriptSelection finishes a drag and returns selected rendered text.
func (m *Model) EndTranscriptSelection(screenRow, column int) (string, bool) {
	if !m.selection.dragging {
		return "", false
	}
	row, column := m.clampTranscriptPoint(screenRow, column)
	m.selection.end = transcriptPoint{row, column}
	m.selection.dragging = false
	m.selection.active = m.selection.start != m.selection.end
	if !m.selection.active {
		return "", false
	}
	return m.SelectedText(), true
}

// SelectedText returns plain text from the current rendered selection.
func (m *Model) SelectedText() string {
	if !m.selection.active {
		return ""
	}
	_, _, lines := m.transcriptBody()
	start, end := m.selection.start, m.selection.end
	if pointBefore(end, start) {
		start, end = end, start
	}
	if start.row < 0 || end.row >= len(lines) {
		return ""
	}
	parts := make([]string, 0, end.row-start.row+1)
	for row := start.row; row <= end.row; row++ {
		left, right := 0, lipgloss.Width(lines[row])
		if row == start.row {
			left = start.column
		}
		if row == end.row {
			right = end.column + 1
		}
		parts = append(parts, ansi.Strip(ansi.Cut(lines[row], left, right)))
	}
	return strings.Join(parts, "\n")
}

// ActivateTranscriptAt toggles the activity block at a screen row, if present.
func (m *Model) ActivateTranscriptAt(screenRow int) (bool, tea.Cmd) {
	row, ok := m.transcriptBodyRow(screenRow)
	if !ok {
		return false, nil
	}
	blockID := m.Transcript.BlockAt(row, m.transcriptBodyHeight(), m.Width)
	index := m.Transcript.Index(blockID)
	if index < 0 {
		return false, nil
	}
	block := m.Transcript.Blocks[index]
	if block.Kind != BlockActivityGroup && block.Kind != BlockToolActivity && block.Kind != BlockSubagentActivity {
		return false, nil
	}
	m.SelectedActivity = block.ID
	return true, m.activateSelectedActivity()
}

func (m *Model) transcriptBodyRow(screenRow int) (int, bool) {
	headerRows, bodyHeight, lines := m.transcriptBody()
	row := screenRow - headerRows
	return row, row >= 0 && row < bodyHeight && row < len(lines)
}

func (m *Model) transcriptBody() (int, int, []string) {
	if m.HasPicker() || m.HasInspection() || m.HasPendingApproval() || m.screenContent != nil && m.screenContent.Body != nil {
		return 0, 0, nil
	}
	headerRows, footerRows := 0, 0
	if m.screenContent != nil {
		headerRows, footerRows = len(m.screenContent.Header), len(m.screenContent.Footer)
	} else {
		headerRows, footerRows = 1, 1
	}
	height := max(0, m.Height-headerRows-footerRows)
	return headerRows, height, m.Transcript.VisibleLines(height, m.Width)
}

func (m *Model) transcriptBodyHeight() int {
	_, height, _ := m.transcriptBody()
	return height
}

func (m *Model) clampTranscriptPoint(screenRow, column int) (int, int) {
	headerRows, height, lines := m.transcriptBody()
	row := min(max(screenRow-headerRows, 0), max(0, min(height, len(lines))-1))
	column = min(max(column, 0), max(0, m.Width-1))
	return row, column
}

func (m *Model) renderTranscriptSelection(lines []string) []string {
	if !m.selection.active && !m.selection.dragging || len(lines) == 0 {
		return lines
	}
	start, end := m.selection.start, m.selection.end
	if pointBefore(end, start) {
		start, end = end, start
	}
	if start.row < 0 || end.row >= len(lines) {
		return lines
	}
	selectedStyle := lipgloss.NewStyle().Reverse(true)
	for row := start.row; row <= end.row; row++ {
		left, right := 0, lipgloss.Width(lines[row])
		if row == start.row {
			left = start.column
		}
		if row == end.row {
			right = end.column + 1
		}
		if left >= right {
			continue
		}
		lines[row] = ansi.Cut(lines[row], 0, left) + selectedStyle.Render(ansi.Strip(ansi.Cut(lines[row], left, right))) + ansi.Cut(lines[row], right, lipgloss.Width(lines[row]))
	}
	return lines
}

func pointBefore(a, b transcriptPoint) bool {
	return a.row < b.row || a.row == b.row && a.column < b.column
}
