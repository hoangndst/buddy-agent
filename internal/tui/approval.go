package tui

import (
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// ApprovalRequest is the protocol-neutral content presented for a tool approval.
type ApprovalRequest struct {
	Tool, TurnID, ArgumentsJSON string
	AllowSession                bool
	Generation                  uint64
}

// ApprovalDecision is the user's selected approval action.
type ApprovalDecision uint8

const (
	// ApprovalAllowOnce approves only the pending tool invocation.
	ApprovalAllowOnce ApprovalDecision = iota
	// ApprovalAllowSession approves eligible invocations for the session.
	ApprovalAllowSession
	// ApprovalDeny rejects the pending tool invocation.
	ApprovalDeny
)

// ApprovalChoice carries the user intent back to the command adapter.
type ApprovalChoice struct {
	Decision            ApprovalDecision
	EditedArgumentsJSON string
}

// ApprovalModal owns the terminal editor and presentation state for approvals.
type ApprovalModal struct {
	editing bool
	Editor  textarea.Model
	err     string
	width   int
	height  int
}

// NewApprovalModal creates the JSON argument editor for a pending request.
func NewApprovalModal(argumentsJSON string, width, height int) *ApprovalModal {
	editor := textarea.New()
	editor.ShowLineNumbers = false
	editor.SetValue(argumentsJSON)
	editor.SetWidth(approvalEditorWidth(width))
	editor.SetHeight(max(1, min(8, height/8)))
	editor.Prompt = ""
	editor.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("shift+enter"), key.WithHelp("shift+enter", "new line"))
	return &ApprovalModal{Editor: editor, width: width, height: height}
}

// Resize updates the editor to the new terminal dimensions.
func (m *ApprovalModal) Resize(width, height int) {
	m.width, m.height = width, height
	m.Editor.SetWidth(approvalEditorWidth(width))
	m.Editor.SetHeight(max(1, min(8, height/8)))
}

// Editing reports whether the JSON editor currently has focus.
func (m *ApprovalModal) Editing() bool { return m.editing }

// Error reports the current JSON editor validation message.
func (m *ApprovalModal) Error() string { return m.err }

// UpdateEditor applies a paste or other non-key editor message.
func (m *ApprovalModal) UpdateEditor(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	m.Editor, cmd = m.Editor.Update(msg)
	return cmd
}

// HandleKey reduces an approval key into a UI-neutral choice or editor update.
func (m *ApprovalModal) HandleKey(msg tea.KeyPressMsg, request ApprovalRequest) (*ApprovalChoice, bool, tea.Cmd) {
	if m.editing {
		if !m.editSurfaceVisible() {
			if msg.String() == "esc" {
				m.editing = false
				m.err = ""
			}
			return nil, false, nil
		}
		switch msg.String() {
		case "esc":
			m.editing = false
			m.err = ""
			return nil, false, nil
		case "enter":
			var args map[string]any
			if err := json.Unmarshal([]byte(m.Editor.Value()), &args); err != nil || args == nil {
				m.err = "Arguments must be a JSON object."
				return nil, false, nil
			}
			return &ApprovalChoice{Decision: ApprovalAllowOnce, EditedArgumentsJSON: m.Editor.Value()}, true, nil
		default:
			var cmd tea.Cmd
			m.Editor, cmd = m.Editor.Update(msg)
			m.err = ""
			return nil, false, cmd
		}
	}
	if !m.canDecide(request) {
		return nil, false, nil
	}
	switch strings.ToLower(msg.String()) {
	case "1", "y", "a":
		return &ApprovalChoice{Decision: ApprovalAllowOnce}, true, nil
	case "2":
		if request.AllowSession {
			return &ApprovalChoice{Decision: ApprovalAllowSession}, true, nil
		}
	case "3", "n", "d":
		return &ApprovalChoice{Decision: ApprovalDeny}, true, nil
	case "4":
		m.editing = true
		cmd := m.Editor.Focus()
		m.err = ""
		return nil, false, cmd
	}
	return nil, false, nil
}

// ActionRow renders all approval actions when they fit, or a resize hint.
func (m *ApprovalModal) ActionRow(request ApprovalRequest, width int) string {
	full := fullApprovalActions(request)
	if lipgloss.Width(full) <= width {
		return full
	}
	compact := compactApprovalActions(request)
	if lipgloss.Width(compact) <= width {
		return compact
	}
	if width <= 0 {
		return ""
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color("#D6A23D")).Render(ansi.Truncate("Resize terminal to decide", width, "…"))
}

// View renders the approval card within the given terminal rectangle.
func (m *ApprovalModal) View(request ApprovalRequest, width, height int) string {
	width, height = max(1, width), max(1, height)
	if m.editing && (width < 20 || height < 6) {
		return ansi.Truncate("Resize to edit · Esc cancels", width, "…")
	}
	if height == 1 {
		return m.ActionRow(request, width)
	}
	if width < 20 || height == 2 {
		return strings.Join([]string{ansi.Truncate(request.Tool, width, "…"), m.ActionRow(request, width)}, "\n")
	}
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#D6A23D")).Padding(0, 1)
	boxWidth := max(1, min(72, width-4))
	box = box.Width(boxWidth)
	var content strings.Builder
	content.WriteString(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#D6A23D")).Render("Tool approval required") + "\n")
	content.WriteString(ansi.Truncate(fmt.Sprintf("%s · %s", request.Tool, request.TurnID), boxWidth, "…") + "\n")
	if m.editing {
		content.WriteString("Edit JSON · Enter approve · Shift+Enter newline · Esc back\n")
		m.Editor.SetHeight(max(1, min(m.Editor.Height(), height-5)))
		content.WriteString(m.Editor.View())
	} else {
		lines := strings.Split(request.ArgumentsJSON, "\n")
		limit := 1
		if width >= 80 && height >= 12 {
			limit = 6
		}
		if len(lines) > 0 && height >= 6 {
			for _, line := range lines[:min(limit, len(lines))] {
				content.WriteString(ansi.Truncate(line, boxWidth, "…") + "\n")
			}
		}
		if len(lines) > limit {
			content.WriteString(fmt.Sprintf("… %d more lines; press 4 to inspect or edit\n", len(lines)-limit))
		}
		content.WriteString(m.ActionRow(request, boxWidth) + "\n")
	}
	if m.err != "" {
		content.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("#D35F5F")).Render(ansi.Truncate(m.err, boxWidth, "…")) + "\n")
	}
	lines := strings.Split(box.Render(strings.TrimRight(content.String(), "\n")), "\n")
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], width, "…")
	}
	if len(lines) > height {
		lines = lines[len(lines)-height:]
	}
	return strings.Join(lines, "\n")
}

func (m *ApprovalModal) editSurfaceVisible() bool {
	modalHeight := min(max(2, m.height/2), max(2, m.height))
	return m.width >= 20 && modalHeight >= 6
}

func (m *ApprovalModal) canDecide(request ApprovalRequest) bool {
	return m.width >= lipgloss.Width(compactApprovalActions(request))
}

func fullApprovalActions(request ApprovalRequest) string {
	actions := []string{"1 Allow once"}
	if request.AllowSession {
		actions = append(actions, "2 Allow session")
	}
	actions = append(actions, "3 Deny", "4 Edit")
	return strings.Join(actions, "  ")
}

func compactApprovalActions(request ApprovalRequest) string {
	actions := []string{"1"}
	if request.AllowSession {
		actions = append(actions, "2")
	}
	actions = append(actions, "3", "4")
	return strings.Join(actions, " ")
}

func approvalEditorWidth(width int) int { return max(1, min(68, width-8)) }
