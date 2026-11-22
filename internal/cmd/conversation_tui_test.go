package cmd

import (
	"strings"
	"testing"

	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
)

func TestConversationResumedMsgPreservesWorkspaceDuringEvictionRecovery(t *testing.T) {
	model := &conversationTUI{workspace: "/work/current", conversation: "conversation-1"}
	updated, _ := model.Update(conversationResumedMsg{result: &protocol.ConversationSyncResult{
		ConversationID: "conversation-1", State: "idle",
	}})
	if got := updated.(*conversationTUI).workspace; got != "/work/current" {
		t.Fatalf("automatic resume replaced current workspace with %q", got)
	}
}

func TestConversationResumedMsgUsesWorkspaceSelectedInPicker(t *testing.T) {
	model := &conversationTUI{workspace: "/work/current", selectedWorkspace: "/work/selected"}
	updated, _ := model.Update(conversationResumedMsg{
		workspace: "/work/selected",
		result:    &protocol.ConversationSyncResult{ConversationID: "conversation-2", State: "idle"},
	})
	if got := updated.(*conversationTUI).workspace; got != "/work/selected" {
		t.Fatalf("resume did not select the chosen workspace: got %q", got)
	}
}

func TestConversationSyncSurfacesExecutionBlockAfterHistoryRestore(t *testing.T) {
	model := &conversationTUI{workspace: "/work/current"}
	model.loadSync(&protocol.ConversationSyncResult{
		ConversationID: "conversation-3", State: "idle", ExecutionBlocked: "credentials are required again before execution",
	})
	if !strings.Contains(model.errorText, "credentials are required again before execution") {
		t.Fatalf("restore execution block was not surfaced: %q", model.errorText)
	}
}
