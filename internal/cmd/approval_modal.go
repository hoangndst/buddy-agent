package cmd

import (
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
)

type approvalModalState struct {
	editing bool
	editor  textarea.Model
	err     string
}

func newApprovalModalState(args map[string]any, width, height int) *approvalModalState {
	data, _ := json.MarshalIndent(args, "", "  ")
	editor := textarea.New()
	editor.SetValue(string(data))
	editor.SetWidth(max(20, width-10))
	editor.SetHeight(min(8, max(3, height/4)))
	editor.Prompt = ""
	return &approvalModalState{editor: editor}
}

func (m *approvalModalState) resize(width, height int) {
	m.editor.SetWidth(max(20, width-10))
	m.editor.SetHeight(min(8, max(3, height/4)))
}

func (m *approvalModalState) handleKey(msg tea.KeyPressMsg, request protocol.PermissionRequestNotification) (*protocol.PermissionRespondParams, bool) {
	if m.editing {
		switch msg.String() {
		case "esc":
			m.editing = false
			m.err = ""
			return nil, false
		case "enter":
			var args map[string]any
			if err := json.Unmarshal([]byte(m.editor.Value()), &args); err != nil || args == nil {
				m.err = "Arguments must be a JSON object."
				return nil, false
			}
			return &protocol.PermissionRespondParams{
				Decision: protocol.DecisionAllowOnce, ModifiedArguments: &args,
			}, true
		default:
			var cmd tea.Cmd
			m.editor, cmd = m.editor.Update(msg)
			_ = cmd
			m.err = ""
			return nil, false
		}
	}
	switch strings.ToLower(msg.String()) {
	case "1", "y", "a":
		return &protocol.PermissionRespondParams{Decision: protocol.DecisionAllowOnce}, true
	case "2":
		if request.Tool == "bash" || supportsSessionToolApproval(request.Tool) {
			response := &protocol.PermissionRespondParams{Decision: protocol.DecisionAllowSession, SessionWhitelist: true}
			if request.Tool == "bash" {
				response.CommandPattern, _ = request.Arguments["command"].(string)
			}
			return response, true
		}
	case "3", "n", "d":
		return &protocol.PermissionRespondParams{Decision: protocol.DecisionDeny, DenialReason: "denied by user"}, true
	case "4":
		m.editing = true
		_ = m.editor.Focus()
		m.err = ""
	}
	return nil, false
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
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("220")).Padding(0, 1)
	boxWidth := max(20, min(76, width-4))
	box = box.Width(boxWidth)
	var content strings.Builder
	content.WriteString(lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220")).Render("Tool approval required") + "\n")
	content.WriteString(fmt.Sprintf("%s · %s\n", request.Tool, request.TurnID))
	if m.editing {
		content.WriteString("Edit arguments JSON · Enter approve once · Shift+Enter newline · Esc back\n")
		content.WriteString(m.editor.View())
	} else {
		encoded, _ := json.MarshalIndent(request.Arguments, "", "  ")
		lines := strings.Split(string(encoded), "\n")
		limit := min(6, max(2, height/8))
		for _, line := range lines[:min(limit, len(lines))] {
			content.WriteString(line + "\n")
		}
		if len(lines) > limit {
			content.WriteString(fmt.Sprintf("… %d more lines; press 4 to inspect or edit\n", len(lines)-limit))
		}
		content.WriteString("[1] Allow once   ")
		if request.Tool == "bash" || supportsSessionToolApproval(request.Tool) {
			content.WriteString("[2] Allow session   ")
		}
		content.WriteString("[3] Deny   [4] Edit args\n")
	}
	if m.err != "" {
		content.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render(m.err) + "\n")
	}
	return box.Render(strings.TrimRight(content.String(), "\n"))
}
