package cmd

import (
	"encoding/json"
	"fmt"

	tea "charm.land/bubbletea/v2"
	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
	"github.com/hoangndst/buddy-agent/internal/tui"
)

func newApprovalModalState(args map[string]any, width, height int) *tui.ApprovalModal {
	data, _ := json.MarshalIndent(args, "", "  ")
	return tui.NewApprovalModal(string(data), width, height)
}

func approvalViewRequest(request protocol.PermissionRequestNotification) tui.ApprovalRequest {
	arguments, _ := json.MarshalIndent(request.Arguments, "", "  ")
	return tui.ApprovalRequest{
		Tool: request.Tool, TurnID: request.TurnID, ArgumentsJSON: string(arguments),
		AllowSession: request.Tool == "bash" || supportsSessionToolApproval(request.Tool),
	}
}

func approvalProtocolParams(choice tui.ApprovalChoice) (*protocol.PermissionRespondParams, error) {
	params := &protocol.PermissionRespondParams{}
	switch choice.Decision {
	case tui.ApprovalAllowOnce:
		params.Decision = protocol.DecisionAllowOnce
	case tui.ApprovalAllowSession:
		params.Decision = protocol.DecisionAllowSession
		params.SessionWhitelist = true
	case tui.ApprovalDeny:
		params.Decision = protocol.DecisionDeny
		params.DenialReason = "denied by user"
	default:
		return nil, fmt.Errorf("unknown approval decision %d", choice.Decision)
	}
	if choice.EditedArgumentsJSON != "" {
		var args map[string]any
		if err := json.Unmarshal([]byte(choice.EditedArgumentsJSON), &args); err != nil || args == nil {
			if err == nil {
				err = fmt.Errorf("edited arguments must be a JSON object")
			}
			return nil, err
		}
		params.Decision = protocol.DecisionAllowOnce
		params.ModifiedArguments = &args
	}
	return params, nil
}

func approvalKeyResult(modal *tui.ApprovalModal, msg tea.KeyPressMsg, request protocol.PermissionRequestNotification) (*protocol.PermissionRespondParams, bool, tea.Cmd, error) {
	choice, done, cmd := modal.HandleKey(msg, approvalViewRequest(request))
	if !done || choice == nil {
		return nil, false, cmd, nil
	}
	params, err := approvalProtocolParams(*choice)
	if err == nil && choice.Decision == tui.ApprovalAllowSession && request.Tool == "bash" {
		params.CommandPattern, _ = request.Arguments["command"].(string)
	}
	return params, err == nil, cmd, err
}

func supportsSessionToolApproval(tool string) bool {
	switch tool {
	case "read_file", "list_directory", "search_files":
		return true
	default:
		return false
	}
}
