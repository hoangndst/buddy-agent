package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/hoangndst/buddy-agent/internal/agent"
	"github.com/hoangndst/buddy-agent/internal/cmdutil"

	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
	"github.com/hoangndst/buddy-agent/internal/mcp"
	"github.com/hoangndst/buddy-agent/internal/persistence"
)

type conversationAPITestDouble struct {
	startGate   chan struct{}
	startCalls  int
	submissions chan protocol.ConversationSubmitParams
	events      chan protocol.SessionEventNotification
}

func (f *conversationAPITestDouble) Capabilities() (*protocol.DaemonCapabilitiesResult, error) {
	return nil, nil
}
func (f *conversationAPITestDouble) StartConversation(protocol.ConversationStartParams) (*protocol.ConversationStartResult, error) {
	f.startCalls++
	if f.startGate != nil {
		<-f.startGate
	}
	return &protocol.ConversationStartResult{ConversationID: "conversation-1", SubmissionID: "first-submission", TurnID: "turn-1"}, nil
}
func (f *conversationAPITestDouble) ResumeConversation(protocol.ConversationResumeParams) (*protocol.ConversationResumeResult, error) {
	return nil, errors.New("unexpected resume")
}
func (f *conversationAPITestDouble) SyncConversation(protocol.ConversationSyncParams) (*protocol.ConversationSyncResult, error) {
	return &protocol.ConversationSyncResult{}, nil
}
func (f *conversationAPITestDouble) ListConversationWorkspaces(int) (*protocol.ConversationListWorkspacesResult, error) {
	return nil, nil
}
func (f *conversationAPITestDouble) ListConversations(protocol.ConversationListParams) (*protocol.ConversationListResult, error) {
	return nil, nil
}
func (f *conversationAPITestDouble) SubmitConversation(params protocol.ConversationSubmitParams) (*protocol.ConversationSubmitResult, error) {
	f.submissions <- params
	return &protocol.ConversationSubmitResult{ConversationID: params.ConversationID, SubmissionID: params.SubmissionID}, nil
}
func (f *conversationAPITestDouble) InterruptTurn(protocol.TurnInterruptParams) (*protocol.TurnInterruptResult, error) {
	return nil, nil
}
func (f *conversationAPITestDouble) RespondConversationPermission(string, string, string, string) error {
	return nil
}
func (f *conversationAPITestDouble) ConversationEvents() <-chan protocol.SessionEventNotification {
	return f.events
}
func (f *conversationAPITestDouble) PermissionRequests() <-chan protocol.PermissionRequestNotification {
	return nil
}
func (f *conversationAPITestDouble) ConversationEventGaps() <-chan struct{} { return nil }
func (f *conversationAPITestDouble) ConversationEvictions() <-chan string   { return nil }
func (f *conversationAPITestDouble) ClearConversationEventGap()             {}
func (f *conversationAPITestDouble) Close() error                           { return nil }

func runBatchCommand(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var result []tea.Msg
		for _, child := range batch {
			result = append(result, runBatchCommand(child)...)
		}
		return result
	}
	if msg == nil {
		return nil
	}
	return []tea.Msg{msg}
}

func TestConversationTUIBuffersPromptsWhileStartIsPending(t *testing.T) {
	gate := make(chan struct{})
	api := &conversationAPITestDouble{startGate: gate, submissions: make(chan protocol.ConversationSubmitParams, 2)}
	model := &conversationTUI{client: api, startParams: protocol.ConversationStartParams{WorkspaceDir: "/work"}}
	firstStart := model.submit("first", persistence.SubmissionQueue)
	if !model.startPending {
		t.Fatal("first prompt did not mark conversation start pending")
	}
	if cmd := model.submit("second", persistence.SubmissionQueue); cmd != nil {
		t.Fatal("second prompt should be buffered while start RPC is pending")
	}
	if cmd := model.submit("third", persistence.SubmissionQueue); cmd != nil {
		t.Fatal("third prompt should be buffered while start RPC is pending")
	}
	if api.startCalls != 0 {
		t.Fatalf("start RPC ran before its asynchronous command: calls=%d", api.startCalls)
	}
	started := make(chan tea.Msg, 1)
	go func() { started <- firstStart() }()
	close(gate)
	startMsg := <-started
	if api.startCalls != 1 {
		t.Fatalf("buffered prompt started another conversation: calls=%d", api.startCalls)
	}
	updated, followup := model.Update(startMsg)
	model = updated.(*conversationTUI)
	if model.conversation != "conversation-1" || len(model.pendingStart) != 2 {
		t.Fatalf("start result did not attach and retain buffered prompts for serialized submission: conversation=%q pending=%+v", model.conversation, model.pendingStart)
	}
	batchMsg := followup()
	batch, ok := batchMsg.(tea.BatchMsg)
	if !ok || len(batch) < 3 {
		t.Fatalf("start completion did not schedule buffered submission: %T", batchMsg)
	}
	firstBuffered := runBatchCommand(batch[len(batch)-1])
	if len(firstBuffered) != 1 {
		t.Fatalf("first buffered submit returned %d messages", len(firstBuffered))
	}
	if submitted, ok := firstBuffered[0].(conversationSubmittedMsg); !ok || submitted.err != nil {
		t.Fatalf("first buffered prompt submit failed: %#v", firstBuffered[0])
	}
	_, next := model.Update(firstBuffered[0])
	batchMsg = next()
	batch, ok = batchMsg.(tea.BatchMsg)
	if !ok || len(batch) < 2 {
		t.Fatalf("first acknowledgment did not schedule the next buffered submission: %T", batchMsg)
	}
	secondBuffered := runBatchCommand(batch[len(batch)-1])
	if len(secondBuffered) != 1 {
		t.Fatalf("second buffered submit returned %d messages", len(secondBuffered))
	}
	if submitted, ok := secondBuffered[0].(conversationSubmittedMsg); !ok || submitted.err != nil {
		t.Fatalf("second buffered prompt submit failed: %#v", secondBuffered[0])
	}
	for i, want := range []string{"second", "third"} {
		select {
		case submitted := <-api.submissions:
			if submitted.ConversationID != "conversation-1" || submitted.Prompt != want {
				t.Fatalf("buffered prompt %d submitted out of order or to wrong conversation: %+v", i, submitted)
			}
		case <-time.After(time.Second):
			t.Fatalf("buffered prompt %q was not submitted after start completed", want)
		}
	}
}

func TestWaitConversationSubmissionTerminalIgnoresPriorTurn(t *testing.T) {
	streams, _, out, _ := cmdutil.TestIOStreams()
	factory := &cmdutil.Factory{IOStreams: streams}
	events := make(chan protocol.SessionEventNotification, 2)
	events <- protocol.SessionEventNotification{ConversationID: "conversation-1", TurnID: "prior-turn", Type: "finished", Content: "prior"}
	events <- protocol.SessionEventNotification{ConversationID: "conversation-1", TurnID: "target-turn", Type: "finished", Content: "target"}
	snapshotCalls := 0
	snapshot := func() (*protocol.ConversationSyncResult, error) {
		snapshotCalls++
		turnID, status := "", ""
		if snapshotCalls > 1 {
			turnID, status = "target-turn", string(persistence.StatusRunning)
		}
		turns := []protocol.ConversationTurnState{}
		if turnID != "" {
			turns = append(turns, protocol.ConversationTurnState{SubmissionID: "queued-submission", TurnID: turnID, Status: status})
		}
		return &protocol.ConversationSyncResult{Turns: turns}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := waitConversationSubmissionEvents(ctx, factory, "conversation-1", "queued-submission", "", persistence.SubmissionQueue, events, nil, snapshot,
		func() (*protocol.ConversationSyncResult, error) { return &protocol.ConversationSyncResult{}, nil }, func() error { return nil })
	if err != nil {
		t.Fatalf("wait for queued target turn: %v", err)
	}
	if snapshotCalls < 2 {
		t.Fatalf("did not resync identity after prior turn: snapshot calls=%d", snapshotCalls)
	}
	if !strings.Contains(out.String(), "prior") || !strings.Contains(out.String(), "target") {
		t.Fatalf("expected both prior and target turn output, got %q", out.String())
	}
}

func TestWaitConversationSubmissionTerminalReturnsWhenSyncShowsCompletion(t *testing.T) {
	streams, _, out, _ := cmdutil.TestIOStreams()
	factory := &cmdutil.Factory{IOStreams: streams}
	completed := &protocol.ConversationSyncResult{Turns: []protocol.ConversationTurnState{{
		SubmissionID: "target-submission", TurnID: "target-turn", Status: string(persistence.StatusCompleted),
	}}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := waitConversationSubmissionEvents(ctx, factory, "conversation-1", "target-submission", "", persistence.SubmissionQueue, nil, nil,
		func() (*protocol.ConversationSyncResult, error) { return completed, nil },
		func() (*protocol.ConversationSyncResult, error) {
			return &protocol.ConversationSyncResult{Transcript: []protocol.ConversationTranscriptEntry{{
				Role: "assistant", TurnID: "target-turn", Content: "restored answer",
			}}}, nil
		}, func() error { return nil })
	if err != nil {
		t.Fatalf("completed target turn from sync should return: %v", err)
	}
	if !strings.Contains(out.String(), "restored answer") {
		t.Fatalf("sync-completed target answer was not printed: %q", out.String())
	}
}

func TestWaitConversationSteerResolvesAttachedTurnFromTranscript(t *testing.T) {
	streams, _, out, _ := cmdutil.TestIOStreams()
	factory := &cmdutil.Factory{IOStreams: streams}
	snapshotCalls := 0
	snapshot := func() (*protocol.ConversationSyncResult, error) {
		snapshotCalls++
		status := string(persistence.StatusRunning)
		if snapshotCalls > 1 {
			status = string(persistence.StatusCompleted)
		}
		return &protocol.ConversationSyncResult{Turns: []protocol.ConversationTurnState{{
			SubmissionID: "original-submission", TurnID: "active-turn", Status: status,
		}}}, nil
	}
	fullSnapshotCalls := 0
	fullSnapshot := func() (*protocol.ConversationSyncResult, error) {
		fullSnapshotCalls++
		return &protocol.ConversationSyncResult{Transcript: []protocol.ConversationTranscriptEntry{
			{SubmissionID: "steer-submission", TurnID: "active-turn", Mode: string(persistence.SubmissionSteer), Role: "user"},
			{TurnID: "active-turn", Role: "assistant", Content: "answer"},
		}}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := waitConversationSubmissionEvents(ctx, factory, "conversation-1", "steer-submission", "", persistence.SubmissionSteer,
		nil, nil, snapshot, fullSnapshot, func() error { return nil })
	if err != nil {
		t.Fatalf("wait for completed turn receiving the steer: %v", err)
	}
	if fullSnapshotCalls < 2 {
		t.Fatal("steer submission was not resolved from transcript attachment")
	}
	if !strings.Contains(out.String(), "answer") {
		t.Fatalf("target turn output was not rendered: %q", out.String())
	}
}

func TestWaitConversationSubmissionDoesNotDuplicateRenderedFinalText(t *testing.T) {
	streams, _, out, _ := cmdutil.TestIOStreams()
	factory := &cmdutil.Factory{IOStreams: streams}
	events := make(chan protocol.SessionEventNotification, 1)
	events <- protocol.SessionEventNotification{ConversationID: "conversation-1", TurnID: "target-turn", Type: string(agent.EventTextComplete), Content: "final answer", Data: map[string]any{"step": 2}}
	snapshotCalls := 0
	snapshot := func() (*protocol.ConversationSyncResult, error) {
		snapshotCalls++
		status := string(persistence.StatusRunning)
		if snapshotCalls > 1 {
			status = string(persistence.StatusCompleted)
		}
		return &protocol.ConversationSyncResult{Turns: []protocol.ConversationTurnState{{
			SubmissionID: "target-submission", TurnID: "target-turn", Status: status,
		}}}, nil
	}
	fullSnapshot := func() (*protocol.ConversationSyncResult, error) {
		return &protocol.ConversationSyncResult{Transcript: []protocol.ConversationTranscriptEntry{{
			Role: "assistant", TurnID: "target-turn", StepNumber: 2, Content: "final answer",
		}}}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := waitConversationSubmissionEvents(ctx, factory, "conversation-1", "target-submission", "target-turn", persistence.SubmissionQueue,
		events, nil, snapshot, fullSnapshot, func() error { return nil })
	if err != nil {
		t.Fatalf("wait for target completion after final text: %v", err)
	}
	if got := strings.Count(out.String(), "final answer"); got != 1 {
		t.Fatalf("final answer was rendered %d times; output=%q", got, out.String())
	}
}

func TestWaitConversationSubmissionRecoversFinalAfterCommentaryTextComplete(t *testing.T) {
	streams, _, out, _ := cmdutil.TestIOStreams()
	factory := &cmdutil.Factory{IOStreams: streams}
	events := make(chan protocol.SessionEventNotification, 1)
	events <- protocol.SessionEventNotification{ConversationID: "conversation-1", TurnID: "target-turn", Type: string(agent.EventTextComplete), Content: "checking the files now", Data: map[string]any{"step": 1}}
	snapshotCalls := 0
	snapshot := func() (*protocol.ConversationSyncResult, error) {
		snapshotCalls++
		status := string(persistence.StatusRunning)
		if snapshotCalls > 1 {
			status = string(persistence.StatusCompleted)
		}
		return &protocol.ConversationSyncResult{Turns: []protocol.ConversationTurnState{{
			SubmissionID: "target-submission", TurnID: "target-turn", Status: status,
		}}}, nil
	}
	fullSnapshot := func() (*protocol.ConversationSyncResult, error) {
		return &protocol.ConversationSyncResult{Transcript: []protocol.ConversationTranscriptEntry{
			{Role: "assistant", TurnID: "target-turn", StepNumber: 1, Content: "checking the files now"},
			{Role: "assistant", TurnID: "target-turn", StepNumber: 2, Content: "the final answer"},
		}}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := waitConversationSubmissionEvents(ctx, factory, "conversation-1", "target-submission", "target-turn", persistence.SubmissionQueue,
		events, nil, snapshot, fullSnapshot, func() error { return nil })
	if err != nil {
		t.Fatalf("wait for final answer after commentary: %v", err)
	}
	if got := strings.Count(out.String(), "checking the files now"); got != 1 {
		t.Fatalf("commentary was rendered %d times; output=%q", got, out.String())
	}
	if got := strings.Count(out.String(), "the final answer"); got != 1 {
		t.Fatalf("final transcript answer was rendered %d times; output=%q", got, out.String())
	}
}

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
