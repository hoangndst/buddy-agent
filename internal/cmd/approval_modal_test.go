package cmd

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
)

func TestApprovalModalBuildsStructuredDecisions(t *testing.T) {
	request := protocol.PermissionRequestNotification{
		Tool: "bash", Arguments: map[string]any{"command": "git status"},
	}
	modal := newApprovalModalState(request.Arguments, 80, 24)
	response, done, _ := modal.handleKey(tea.KeyPressMsg{Text: "2", Code: '2'}, request)
	if !done || response.Decision != protocol.DecisionAllowSession || response.CommandPattern != "git status" || !response.SessionWhitelist {
		t.Fatalf("session approval = %+v, done=%t", response, done)
	}
	response, done, _ = modal.handleKey(tea.KeyPressMsg{Text: "1", Code: '1'}, request)
	if !done || response.Decision != protocol.DecisionAllowOnce {
		t.Fatalf("one-time approval = %+v, done=%t", response, done)
	}
	request.Tool = "write_file"
	if response, done, _ = modal.handleKey(tea.KeyPressMsg{Text: "2", Code: '2'}, request); done || response != nil {
		t.Fatalf("mutating tool received session approval: %+v, done=%t", response, done)
	}
}

func TestApprovalModalEditsJSONArguments(t *testing.T) {
	request := protocol.PermissionRequestNotification{Tool: "write_file", Arguments: map[string]any{"path": "old.go"}}
	modal := newApprovalModalState(request.Arguments, 80, 24)
	modal.handleKey(tea.KeyPressMsg{Text: "4", Code: '4'}, request)
	if !modal.editor.Focused() {
		t.Fatal("argument editor is not focused")
	}
	modal.editor.CursorEnd()
	for range len(modal.editor.Value()) {
		modal.handleKey(tea.KeyPressMsg{Code: tea.KeyBackspace}, request)
	}
	for _, ch := range `{"path":"new.go"}` {
		modal.handleKey(tea.KeyPressMsg{Text: string(ch), Code: ch}, request)
	}
	response, done, _ := modal.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter}, request)
	if !done || response.Decision != protocol.DecisionAllowOnce || response.ModifiedArguments == nil || (*response.ModifiedArguments)["path"] != "new.go" {
		t.Fatalf("edited approval = %+v, done=%t", response, done)
	}
	modal.editor.SetValue("[")
	response, done, _ = modal.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter}, request)
	if done || response != nil || modal.err == "" {
		t.Fatalf("invalid arguments were accepted: response=%+v done=%t err=%q", response, done, modal.err)
	}
}

func TestApprovalModalShiftEnterInsertsNewline(t *testing.T) {
	request := protocol.PermissionRequestNotification{Tool: "write_file", Arguments: map[string]any{"path": "old.go"}}
	modal := newApprovalModalState(request.Arguments, 80, 24)
	modal.handleKey(tea.KeyPressMsg{Text: "4", Code: '4'}, request)

	_, done, _ := modal.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift}, request)
	if done {
		t.Fatal("Shift+Enter submitted the approval")
	}
	if got := modal.editor.Value(); !strings.Contains(got, "\n") {
		t.Fatalf("Shift+Enter did not insert a newline: %q", got)
	}
	response, done, _ := modal.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter}, request)
	if !done || response == nil || response.Decision != protocol.DecisionAllowOnce {
		t.Fatalf("normal Enter did not submit approval: response=%+v done=%t", response, done)
	}
}
