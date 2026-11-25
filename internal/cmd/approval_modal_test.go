package cmd

import (
	"testing"

	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
	"github.com/hoangndst/buddy-agent/internal/tui"
)

func TestApprovalModalBuildsStructuredDecisions(t *testing.T) {
	request := protocol.PermissionRequestNotification{
		Tool: "bash", Arguments: map[string]any{"command": "git status"},
	}
	response, err := approvalParamsForRequest(tui.ApprovalChoice{Decision: tui.ApprovalAllowSession}, request)
	if err != nil || response.Decision != protocol.DecisionAllowSession || response.CommandPattern != "git status" || !response.SessionWhitelist {
		t.Fatalf("session approval = %+v, err=%v", response, err)
	}
	response, err = approvalParamsForRequest(tui.ApprovalChoice{Decision: tui.ApprovalAllowOnce}, request)
	if err != nil || response.Decision != protocol.DecisionAllowOnce {
		t.Fatalf("one-time approval = %+v, err=%v", response, err)
	}
	request.Tool = "write_file"
	if response, err = approvalParamsForRequest(tui.ApprovalChoice{Decision: tui.ApprovalAllowSession}, request); err == nil || response != nil {
		t.Fatalf("mutating tool received session approval: %+v, err=%v", response, err)
	}
}
