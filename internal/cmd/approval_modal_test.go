package cmd

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
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

func TestApprovalModalRequiresVisibleDecisionKeys(t *testing.T) {
	request := protocol.PermissionRequestNotification{Tool: "bash", Arguments: map[string]any{"command": "go test ./..."}}
	modal := newApprovalModalState(request.Arguments, 6, 1)
	if got := modal.actionRow(request, 6); got == "1 2 3 4" {
		t.Fatalf("unavailable action row was shown at width 6: %q", got)
	}
	if response, done, _ := modal.handleKey(tea.KeyPressMsg{Text: "1", Code: '1'}, request); done || response != nil {
		t.Fatalf("approval accepted a hidden key at width 6: %+v", response)
	}
	modal.resize(7, 1)
	if got := modal.actionRow(request, 7); got != "1 2 3 4" {
		t.Fatalf("compact action row at width 7 = %q", got)
	}
	if response, done, _ := modal.handleKey(tea.KeyPressMsg{Text: "3", Code: '3'}, request); !done || response.Decision != protocol.DecisionDeny {
		t.Fatalf("visible deny key was not accepted: %+v done=%t", response, done)
	}
	request.Tool = "write_file"
	modal.resize(4, 1)
	if response, done, _ := modal.handleKey(tea.KeyPressMsg{Text: "1", Code: '1'}, request); done || response != nil {
		t.Fatalf("approval accepted keys that do not fit at width 4: %+v", response)
	}
	modal.resize(5, 1)
	if got := modal.actionRow(request, 5); got != "1 3 4" {
		t.Fatalf("non-session compact action row at width 5 = %q", got)
	}
}

func TestApprovalModalKeepsActionsVisibleAtCompactSizes(t *testing.T) {
	request := protocol.PermissionRequestNotification{Tool: "bash", TurnID: "turn-1", Arguments: map[string]any{"command": "go test ./..."}}
	modal := newApprovalModalState(request.Arguments, 40, 12)
	for _, size := range []struct{ width, height int }{{80, 12}, {40, 6}, {40, 2}, {40, 1}} {
		view := modal.view(request, size.width, size.height)
		if !strings.Contains(ansi.Strip(view), "1 2 3 4") && !strings.Contains(ansi.Strip(view), "1 Allow once") {
			t.Errorf("%dx%d approval view hid decision keys: %q", size.width, size.height, view)
		}
		if len(strings.Split(view, "\n")) > size.height {
			t.Errorf("%dx%d approval view exceeded height: %q", size.width, size.height, view)
		}
		for _, line := range strings.Split(view, "\n") {
			if width := ansi.StringWidth(line); width > size.width {
				t.Errorf("%dx%d approval line exceeded width (%d): %q", size.width, size.height, width, line)
			}
		}
	}
}

func TestApprovalModalResizeCannotApproveHiddenEditor(t *testing.T) {
	request := protocol.PermissionRequestNotification{Tool: "write_file", Arguments: map[string]any{"path": "important.txt"}}
	modal := newApprovalModalState(request.Arguments, 40, 12)
	modal.handleKey(tea.KeyPressMsg{Text: "4", Code: '4'}, request)
	if !modal.editing {
		t.Fatal("edit action did not open the argument editor")
	}
	modal.resize(10, 1)
	if got := ansi.Strip(modal.view(request, 10, 1)); !strings.Contains(got, "Resize") {
		t.Fatalf("hidden editor did not explain the resize requirement: %q", got)
	}
	for _, key := range []tea.KeyPressMsg{{Code: tea.KeyEnter}, {Text: "1", Code: '1'}} {
		if response, done, _ := modal.handleKey(key, request); done || response != nil {
			t.Fatalf("hidden editor accepted an approval key: %+v", response)
		}
	}
	if !modal.editing {
		t.Fatal("blocked approval key unexpectedly exited edit mode")
	}
	modal.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape}, request)
	if modal.editing {
		t.Fatal("Esc did not leave hidden edit mode")
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
