package cmd

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
)

func TestApprovalModalBuildsStructuredDecisions(t *testing.T) {
	request := protocol.PermissionRequestNotification{
		Tool: "bash", Arguments: map[string]any{"command": "git status"},
	}
	modal := newApprovalModalState(request.Arguments, 80, 24)
	response, done := modal.handleKey(tea.KeyPressMsg{Text: "2", Code: '2'}, request)
	if !done || response.Decision != protocol.DecisionAllowSession || response.CommandPattern != "git status" || !response.SessionWhitelist {
		t.Fatalf("session approval = %+v, done=%t", response, done)
	}
	response, done = modal.handleKey(tea.KeyPressMsg{Text: "1", Code: '1'}, request)
	if !done || response.Decision != protocol.DecisionAllowOnce {
		t.Fatalf("one-time approval = %+v, done=%t", response, done)
	}
	request.Tool = "write_file"
	if response, done = modal.handleKey(tea.KeyPressMsg{Text: "2", Code: '2'}, request); done || response != nil {
		t.Fatalf("mutating tool received session approval: %+v, done=%t", response, done)
	}
}

func TestApprovalModalEditsJSONArguments(t *testing.T) {
	request := protocol.PermissionRequestNotification{Tool: "write_file", Arguments: map[string]any{"path": "old.go"}}
	modal := newApprovalModalState(request.Arguments, 80, 24)
	modal.handleKey(tea.KeyPressMsg{Text: "4", Code: '4'}, request)
	modal.editor.SetValue(`{"path":"new.go"}`)
	response, done := modal.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter}, request)
	if !done || response.Decision != protocol.DecisionAllowOnce || response.ModifiedArguments["path"] != "new.go" {
		t.Fatalf("edited approval = %+v, done=%t", response, done)
	}
	modal.editor.SetValue("[")
	response, done = modal.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter}, request)
	if done || response != nil || modal.err == "" {
		t.Fatalf("invalid arguments were accepted: response=%+v done=%t err=%q", response, done, modal.err)
	}
}
