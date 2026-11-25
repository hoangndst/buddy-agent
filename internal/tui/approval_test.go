package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestApprovalModalRequiresVisibleDecisionKeys(t *testing.T) {
	request := ApprovalRequest{Tool: "bash", ArgumentsJSON: `{"command":"go test ./..."}`, AllowSession: true}
	modal := NewApprovalModal(request.ArgumentsJSON, 6, 1)
	if got := modal.ActionRow(request, 6); got == "1 2 3 4" {
		t.Fatalf("unavailable action row was shown at width 6: %q", got)
	}
	if choice, done, _ := modal.HandleKey(tea.KeyPressMsg{Text: "1", Code: '1'}, request); done || choice != nil {
		t.Fatalf("approval accepted a hidden key at width 6: %+v", choice)
	}
	modal.Resize(7, 1)
	if got := modal.ActionRow(request, 7); got != "1 2 3 4" {
		t.Fatalf("compact action row at width 7 = %q", got)
	}
	if choice, done, _ := modal.HandleKey(tea.KeyPressMsg{Text: "3", Code: '3'}, request); !done || choice.Decision != ApprovalDeny {
		t.Fatalf("visible deny key was not accepted: %+v done=%t", choice, done)
	}
	request.Tool = "write_file"
	request.AllowSession = false
	modal.Resize(4, 1)
	if choice, done, _ := modal.HandleKey(tea.KeyPressMsg{Text: "1", Code: '1'}, request); done || choice != nil {
		t.Fatalf("approval accepted keys that do not fit at width 4: %+v", choice)
	}
	modal.Resize(5, 1)
	if got := modal.ActionRow(request, 5); got != "1 3 4" {
		t.Fatalf("non-session compact action row at width 5 = %q", got)
	}
}

func TestApprovalModalKeepsActionsVisibleAtCompactSizes(t *testing.T) {
	request := ApprovalRequest{Tool: "bash", TurnID: "turn-1", ArgumentsJSON: `{"command":"go test ./..."}`, AllowSession: true}
	modal := NewApprovalModal(request.ArgumentsJSON, 40, 12)
	for _, size := range []struct{ width, height int }{{80, 12}, {40, 6}, {40, 2}, {40, 1}} {
		view := modal.View(request, size.width, size.height)
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
	request := ApprovalRequest{Tool: "write_file", ArgumentsJSON: `{"path":"important.txt"}`}
	modal := NewApprovalModal(request.ArgumentsJSON, 40, 12)
	approvalKey(t, modal, tea.KeyPressMsg{Text: "4", Code: '4'}, request)
	if !modal.Editing() {
		t.Fatal("edit action did not open the argument editor")
	}
	modal.Resize(10, 1)
	if got := ansi.Strip(modal.View(request, 10, 1)); !strings.Contains(got, "Resize") {
		t.Fatalf("hidden editor did not explain the resize requirement: %q", got)
	}
	for _, key := range []tea.KeyPressMsg{{Code: tea.KeyEnter}, {Text: "1", Code: '1'}} {
		if choice, done, _ := modal.HandleKey(key, request); done || choice != nil {
			t.Fatalf("hidden editor accepted an approval key: %+v", choice)
		}
	}
	if !modal.Editing() {
		t.Fatal("blocked approval key unexpectedly exited edit mode")
	}
	approvalKey(t, modal, tea.KeyPressMsg{Code: tea.KeyEscape}, request)
	if modal.Editing() {
		t.Fatal("Esc did not leave hidden edit mode")
	}
}

func TestApprovalModalEditsJSONArguments(t *testing.T) {
	request := ApprovalRequest{Tool: "write_file", ArgumentsJSON: `{"path":"old.go"}`}
	modal := NewApprovalModal(request.ArgumentsJSON, 80, 24)
	approvalKey(t, modal, tea.KeyPressMsg{Text: "4", Code: '4'}, request)
	if !modal.Editor.Focused() {
		t.Fatal("argument editor is not focused")
	}
	modal.Editor.CursorEnd()
	for range len(modal.Editor.Value()) {
		approvalKey(t, modal, tea.KeyPressMsg{Code: tea.KeyBackspace}, request)
	}
	for _, ch := range `{"path":"new.go"}` {
		approvalKey(t, modal, tea.KeyPressMsg{Text: string(ch), Code: ch}, request)
	}
	choice, done, _ := approvalKey(t, modal, tea.KeyPressMsg{Code: tea.KeyEnter}, request)
	if !done || choice.Decision != ApprovalAllowOnce || choice.EditedArgumentsJSON == "" || !strings.Contains(choice.EditedArgumentsJSON, "new.go") {
		t.Fatalf("edited approval = %+v, done=%t", choice, done)
	}
	modal.Editor.SetValue("[")
	choice, done, _ = approvalKey(t, modal, tea.KeyPressMsg{Code: tea.KeyEnter}, request)
	if done || choice != nil || modal.Error() == "" {
		t.Fatalf("invalid arguments were accepted: choice=%+v done=%t err=%q", choice, done, modal.Error())
	}
}

func TestApprovalModalShiftEnterInsertsNewline(t *testing.T) {
	request := ApprovalRequest{Tool: "write_file", ArgumentsJSON: `{"path":"old.go"}`}
	modal := NewApprovalModal(request.ArgumentsJSON, 80, 24)
	approvalKey(t, modal, tea.KeyPressMsg{Text: "4", Code: '4'}, request)

	_, done, _ := approvalKey(t, modal, tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift}, request)
	if done {
		t.Fatal("Shift+Enter submitted the approval")
	}
	if got := modal.Editor.Value(); !strings.Contains(got, "\n") {
		t.Fatalf("Shift+Enter did not insert a newline: %q", got)
	}
	choice, done, _ := approvalKey(t, modal, tea.KeyPressMsg{Code: tea.KeyEnter}, request)
	if !done || choice == nil || choice.Decision != ApprovalAllowOnce {
		t.Fatalf("normal Enter did not submit approval: choice=%+v done=%t", choice, done)
	}
}

func TestApprovalModalReceivesPastedJSON(t *testing.T) {
	request := ApprovalRequest{Tool: "write_file", ArgumentsJSON: "{}"}
	modal := NewApprovalModal("", 80, 24)
	approvalKey(t, modal, tea.KeyPressMsg{Text: "4", Code: '4'}, request)
	modal.UpdateEditor(tea.PasteMsg{Content: `{"path":"pasted.go"}`})
	if got := modal.Editor.Value(); got != `{"path":"pasted.go"}` {
		t.Fatalf("pasted JSON = %q", got)
	}
	choice, done, _ := approvalKey(t, modal, tea.KeyPressMsg{Code: tea.KeyEnter}, request)
	if !done || choice == nil || choice.EditedArgumentsJSON != `{"path":"pasted.go"}` {
		t.Fatalf("pasted JSON choice = %+v done=%t", choice, done)
	}
}

func approvalKey(t *testing.T, modal *ApprovalModal, msg tea.KeyPressMsg, request ApprovalRequest) (*ApprovalChoice, bool, tea.Cmd) {
	t.Helper()
	return modal.HandleKey(msg, request)
}
