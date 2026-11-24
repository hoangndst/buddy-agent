package cmd

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
	"github.com/hoangndst/buddy-agent/internal/tui"
)

func TestApprovalModalBuildsStructuredDecisions(t *testing.T) {
	request := protocol.PermissionRequestNotification{
		Tool: "bash", Arguments: map[string]any{"command": "git status"},
	}
	modal := newApprovalModalState(request.Arguments, 80, 24)
	response, done, _ := approvalModalKey(t, modal, tea.KeyPressMsg{Text: "2", Code: '2'}, request)
	if !done || response.Decision != protocol.DecisionAllowSession || response.CommandPattern != "git status" || !response.SessionWhitelist {
		t.Fatalf("session approval = %+v, done=%t", response, done)
	}
	response, done, _ = approvalModalKey(t, modal, tea.KeyPressMsg{Text: "1", Code: '1'}, request)
	if !done || response.Decision != protocol.DecisionAllowOnce {
		t.Fatalf("one-time approval = %+v, done=%t", response, done)
	}
	request.Tool = "write_file"
	if response, done, _ = approvalModalKey(t, modal, tea.KeyPressMsg{Text: "2", Code: '2'}, request); done || response != nil {
		t.Fatalf("mutating tool received session approval: %+v, done=%t", response, done)
	}
}

func approvalModalKey(t *testing.T, modal *tui.ApprovalModal, msg tea.KeyPressMsg, request protocol.PermissionRequestNotification) (*protocol.PermissionRespondParams, bool, tea.Cmd) {
	t.Helper()
	response, done, cmd, err := approvalKeyResult(modal, msg, request)
	if err != nil {
		t.Fatal(err)
	}
	return response, done, cmd
}
