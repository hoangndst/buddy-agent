package cmd

import (
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
)

type approvalModalState struct {
	editing bool
	editor  textarea.Model
	err     string
	width   int
	height  int
}

func newApprovalModalState(args map[string]any, width, height int) *approvalModalState {
	data, _ := json.MarshalIndent(args, "", "  ")
	editor := textarea.New()
	editor.ShowLineNumbers = false
	editor.SetValue(string(data))
	editor.SetWidth(approvalEditorWidth(width))
	editor.SetHeight(max(1, min(8, height/8)))
	editor.Prompt = ""
	editor.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("shift+enter"), key.WithHelp("shift+enter", "new line"))
	return &approvalModalState{editor: editor, width: width, height: height}
}

func (m *approvalModalState) resize(width, height int) {
	m.width, m.height = width, height
	m.editor.SetWidth(approvalEditorWidth(width))
	m.editor.SetHeight(max(1, min(8, height/8)))
}

func approvalEditorWidth(width int) int {
	return max(1, min(68, width-8))
}

func (m *approvalModalState) handleKey(msg tea.KeyPressMsg, request protocol.PermissionRequestNotification) (*protocol.PermissionRespondParams, bool, tea.Cmd) {
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
			if err := json.Unmarshal([]byte(m.editor.Value()), &args); err != nil || args == nil {
				m.err = "Arguments must be a JSON object."
				return nil, false, nil
			}
			return &protocol.PermissionRespondParams{
				Decision: protocol.DecisionAllowOnce, ModifiedArguments: &args,
			}, true, nil
		default:
			var cmd tea.Cmd
			m.editor, cmd = m.editor.Update(msg)
			m.err = ""
			return nil, false, cmd
		}
	}
	if !m.canDecide(request) {
		return nil, false, nil
	}
	switch strings.ToLower(msg.String()) {
	case "1", "y", "a":
		return &protocol.PermissionRespondParams{Decision: protocol.DecisionAllowOnce}, true, nil
	case "2":
		if request.Tool == "bash" || supportsSessionToolApproval(request.Tool) {
			response := &protocol.PermissionRespondParams{Decision: protocol.DecisionAllowSession, SessionWhitelist: true}
			if request.Tool == "bash" {
				response.CommandPattern, _ = request.Arguments["command"].(string)
			}
			return response, true, nil
		}
	case "3", "n", "d":
		return &protocol.PermissionRespondParams{Decision: protocol.DecisionDeny, DenialReason: "denied by user"}, true, nil
	case "4":
		m.editing = true
		cmd := m.editor.Focus()
		m.err = ""
		return nil, false, cmd
	}
	return nil, false, nil
}

func (m *approvalModalState) editSurfaceVisible() bool {
	modalHeight := min(max(2, m.height/2), max(2, m.height))
	return m.width >= 20 && modalHeight >= 6
}

func (m *approvalModalState) canDecide(request protocol.PermissionRequestNotification) bool {
	return m.width >= lipgloss.Width(compactApprovalActions(request))
}

func (m *approvalModalState) actionRow(request protocol.PermissionRequestNotification, width int) string {
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

func fullApprovalActions(request protocol.PermissionRequestNotification) string {
	actions := []string{"1 Allow once"}
	if request.Tool == "bash" || supportsSessionToolApproval(request.Tool) {
		actions = append(actions, "2 Allow session")
	}
	actions = append(actions, "3 Deny", "4 Edit")
	return strings.Join(actions, "  ")
}

func compactApprovalActions(request protocol.PermissionRequestNotification) string {
	actions := []string{"1"}
	if request.Tool == "bash" || supportsSessionToolApproval(request.Tool) {
		actions = append(actions, "2")
	}
	actions = append(actions, "3", "4")
	return strings.Join(actions, " ")
}

func supportsSessionToolApproval(tool string) bool {
	switch tool {
	case "read_file", "list_directory", "search_files":
		return true
	default:
		return false
	}
}

func (m *approvalModalState) view(request protocol.PermissionRequestNotification, width, height int) string {
	width, height = max(1, width), max(1, height)
	if m.editing && (width < 20 || height < 6) {
		return ansi.Truncate("Resize to edit · Esc cancels", width, "…")
	}
	if height == 1 {
		return m.actionRow(request, width)
	}
	if width < 20 {
		return strings.Join([]string{ansi.Truncate(request.Tool, width, "…"), m.actionRow(request, width)}, "\n")
	}
	if height == 2 {
		return strings.Join([]string{ansi.Truncate(request.Tool, width, "…"), m.actionRow(request, width)}, "\n")
	}
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#D6A23D")).Padding(0, 1)
	boxWidth := max(1, min(72, width-4))
	box = box.Width(boxWidth)
	var content strings.Builder
	content.WriteString(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#D6A23D")).Render("Tool approval required") + "\n")
	content.WriteString(ansi.Truncate(fmt.Sprintf("%s · %s", request.Tool, request.TurnID), boxWidth, "…") + "\n")
	if m.editing {
		content.WriteString("Edit JSON · Enter approve · Shift+Enter newline · Esc back\n")
		m.editor.SetHeight(max(1, min(m.editor.Height(), height-5)))
		content.WriteString(m.editor.View())
	} else {
		encoded, _ := json.MarshalIndent(request.Arguments, "", "  ")
		lines := strings.Split(string(encoded), "\n")
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
		content.WriteString(m.actionRow(request, boxWidth) + "\n")
	}
	if m.err != "" {
		content.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("#D35F5F")).Render(ansi.Truncate(m.err, boxWidth, "…")) + "\n")
	}
	lines := strings.Split(box.Render(strings.TrimRight(content.String(), "\n")), "\n")
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], width, "…")
	}
	lines = fitTerminalRows(lines, height)
	return strings.Join(lines, "\n")
}
