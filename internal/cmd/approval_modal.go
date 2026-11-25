package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
	"github.com/hoangndst/buddy-agent/internal/tui"
)

func approvalViewRequest(request protocol.PermissionRequestNotification, generation uint64) tui.ApprovalRequest {
	arguments, _ := json.MarshalIndent(request.Arguments, "", "  ")
	return tui.ApprovalRequest{
		Tool: request.Tool, TurnID: request.TurnID, ArgumentsJSON: string(arguments),
		AllowSession: request.Tool == "bash" || supportsSessionToolApproval(request.Tool), Generation: generation,
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

func approvalParamsForRequest(choice tui.ApprovalChoice, request protocol.PermissionRequestNotification) (*protocol.PermissionRespondParams, error) {
	if choice.Decision == tui.ApprovalAllowSession && request.Tool != "bash" && !supportsSessionToolApproval(request.Tool) {
		return nil, fmt.Errorf("session approval is not supported for tool %q", request.Tool)
	}
	params, err := approvalProtocolParams(choice)
	if err != nil {
		return nil, err
	}
	if choice.Decision == tui.ApprovalAllowSession && request.Tool == "bash" {
		params.CommandPattern, _ = request.Arguments["command"].(string)
	}
	return params, nil
}

func supportsSessionToolApproval(tool string) bool {
	switch tool {
	case "read_file", "list_directory", "search_files":
		return true
	default:
		return false
	}
}
