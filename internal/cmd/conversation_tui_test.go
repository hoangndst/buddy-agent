package cmd

import (
	"strings"
	"testing"

	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
	"github.com/hoangndst/buddy-agent/internal/mcp"
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

func TestResumeSelectedWorkspaceReloadsAndRetainsMCPConfig(t *testing.T) {
	original := &mcp.Config{MCPServers: map[string]mcp.ServerConfig{"original": {Command: "original-mcp"}}}
	selected := &mcp.Config{MCPServers: map[string]mcp.ServerConfig{"selected": {Command: "selected-mcp"}}}
	loadedWorkspace := ""
	model := &conversationTUI{
		workspace:   "/work/original",
		startParams: protocol.ConversationStartParams{WorkspaceDir: "/work/original", MCPConfig: original},
		loadWorkspaceMCPConfig: func(workspace string) (*mcp.Config, error) {
			loadedWorkspace = workspace
			return selected, nil
		},
	}
	params, mcpConfig, err := model.resumeParamsForWorkspace("/work/selected", "conversation-4", "/work/selected/.buddy/history.db")
	if err != nil {
		t.Fatalf("build selected workspace resume params: %v", err)
	}
	if loadedWorkspace != "/work/selected" || params.MCPConfig != selected || mcpConfig != selected || params.DBPath != "/work/selected/.buddy/history.db" {
		t.Fatalf("resume did not use selected workspace MCP config: loaded=%q params=%+v config=%+v", loadedWorkspace, params, mcpConfig)
	}
	updated, _ := model.Update(conversationResumedMsg{
		workspace: "/work/selected", mcpConfig: mcpConfig, updateMCPConfig: true,
		result: &protocol.ConversationSyncResult{ConversationID: "conversation-4", State: "idle"},
	})
	resumed := updated.(*conversationTUI)
	if resumed.startParams.WorkspaceDir != "/work/selected" || resumed.startParams.MCPConfig != selected {
		t.Fatalf("selected MCP config was not retained for recovery: workspace=%q config=%+v", resumed.startParams.WorkspaceDir, resumed.startParams.MCPConfig)
	}
}

func TestResumeDifferentWorkspaceClearsMCPConfigWithoutLoader(t *testing.T) {
	original := &mcp.Config{MCPServers: map[string]mcp.ServerConfig{"original": {Command: "original-mcp"}}}
	model := &conversationTUI{startParams: protocol.ConversationStartParams{WorkspaceDir: "/work/original", MCPConfig: original}}
	params, config, err := model.resumeParamsForWorkspace("/work/selected", "conversation-5", "")
	if err != nil {
		t.Fatal(err)
	}
	if params.MCPConfig != nil || config != nil {
		t.Fatalf("workspace-specific MCP config leaked to another workspace: %+v", params.MCPConfig)
	}
}
