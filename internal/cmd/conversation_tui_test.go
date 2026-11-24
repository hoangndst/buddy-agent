package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/textarea"
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
	outputReads []protocol.OutputReadParams
	diffResult  *protocol.DiffInspectResult
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
func (f *conversationAPITestDouble) ReadOutput(params protocol.OutputReadParams) (*protocol.OutputReadResult, error) {
	f.outputReads = append(f.outputReads, params)
	return &protocol.OutputReadResult{Data: "page", TotalBytes: 4}, nil
}
func (f *conversationAPITestDouble) InspectDiff(protocol.DiffInspectParams) (*protocol.DiffInspectResult, error) {
	if f.diffResult != nil {
		return f.diffResult, nil
	}
	return &protocol.DiffInspectResult{}, nil
}
func (f *conversationAPITestDouble) RespondConversationPermissionParams(protocol.PermissionRespondParams) error {
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

func TestConversationTUIRowVirtualization(t *testing.T) {
	model := &conversationTUI{
		textarea:       textarea.New(),
		width:          80,
		height:         24,
		lineCountCache: make(map[string]int),
		wrapCache:      make(map[string][]string),
	}
	// Append 30 distinct blocks
	for i := 0; i < 30; i++ {
		model.appendBlock(TranscriptBlock{
			ID:        fmt.Sprintf("block-%02d", i),
			Kind:      BlockAssistantMessage,
			Role:      "assistant",
			Header:    "Buddy",
			Body:      fmt.Sprintf("This is assistant message body #%02d with multiple lines\nline two of content\nline three of content", i),
			CreatedAt: time.Now(),
		})
	}

	// At scrollOffset 0 (bottom of conversation)
	model.scrollOffset = 0
	visible := model.computeVisibleBlocks()
	if len(visible) == 0 || len(visible) >= len(model.blocks) {
		t.Fatalf("expected visible subset of blocks, got %d of %d", len(visible), len(model.blocks))
	}

	// Verify block 0 is NOT visible at bottom
	if visible[0].ID == "block-00" {
		t.Fatalf("block-00 should be off-screen when scrolled to bottom, but was visible: %s", visible[0].ID)
	}
	// Verify the last block is visible at bottom
	lastVisible := visible[len(visible)-1]
	if lastVisible.ID != "block-29" {
		t.Fatalf("block-29 should be visible at bottom, got %s", lastVisible.ID)
	}

	view := model.View()
	rendered := fmt.Sprintf("%v", view)
	if strings.Contains(rendered, "message body #00") {
		t.Fatalf("rendered view at bottom should omit off-screen block-00, got: %s", rendered)
	}
	if !strings.Contains(rendered, "message body #29") {
		t.Fatalf("rendered view at bottom should contain on-screen block-29")
	}

	// Scroll to top
	model.scrollOffset = 1000 // will be clamped to maxScroll
	visibleTop := model.computeVisibleBlocks()
	if len(visibleTop) == 0 {
		t.Fatal("expected visible blocks at top")
	}
	if visibleTop[0].ID != "block-00" {
		t.Fatalf("block-00 should be visible when scrolled to top, got %s", visibleTop[0].ID)
	}
	viewTop := model.View()
	renderedTop := fmt.Sprintf("%v", viewTop)
	if !strings.Contains(renderedTop, "message body #00") {
		t.Fatalf("rendered view at top should contain block-00")
	}
	if strings.Contains(renderedTop, "message body #29") {
		t.Fatalf("rendered view at top should omit block-29")
	}
}

func TestConversationTUIWrapCacheInvalidation(t *testing.T) {
	model := &conversationTUI{
		textarea:       textarea.New(),
		width:          80,
		height:         24,
		lineCountCache: make(map[string]int),
		wrapCache:      make(map[string][]string),
	}
	model.appendBlock(TranscriptBlock{
		ID:        "test-block-1",
		Kind:      BlockAssistantMessage,
		Role:      "assistant",
		Header:    "Buddy",
		Body:      "Short content for testing cache invalidation",
		CreatedAt: time.Now(),
	})

	_ = model.computeVisibleBlocks()
	key80 := "test-block-1:80"
	if _, ok := model.lineCountCache[key80]; !ok {
		t.Fatalf("expected lineCountCache to contain key %q", key80)
	}

	// Trigger window resize to width 120
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	model = updated.(*conversationTUI)

	if _, ok := model.lineCountCache[key80]; ok {
		t.Fatalf("old cache key %q should be cleared after resize", key80)
	}
	key120 := "test-block-1:120"
	if _, ok := model.lineCountCache[key120]; !ok {
		t.Fatalf("new cache key %q should be populated after resize", key120)
	}
}

func TestConversationTUIStreamingDeltaCoalescingThreadSafe(t *testing.T) {
	api := &conversationAPITestDouble{events: make(chan protocol.SessionEventNotification, 10)}
	model := &conversationTUI{
		client:         api,
		conversation:   "conversation-1",
		state:          "running",
		width:          80,
		height:         24,
		lineCountCache: make(map[string]int),
		wrapCache:      make(map[string][]string),
	}

	// Deliver 100 token deltas in rapid succession
	for i := 0; i < 100; i++ {
		updated, cmd := model.Update(conversationEventMsg{
			ConversationID: "conversation-1",
			TurnID:         "turn-1",
			Type:           string(agent.EventTextDelta),
			Content:        fmt.Sprintf("token%d ", i),
		})
		model = updated.(*conversationTUI)
		if i == 0 && cmd == nil {
			t.Fatal("first delta should arm ticker command")
		}
	}

	if !model.tickerActive {
		t.Fatal("ticker should be active while streaming deltas")
	}
	if model.deltaBuffer.Len() == 0 {
		t.Fatal("deltaBuffer should have buffered deltas")
	}

	// Fire render tick
	updated, _ := model.Update(renderTickMsg(time.Now()))
	model = updated.(*conversationTUI)

	if model.deltaBuffer.Len() != 0 {
		t.Fatalf("renderTickMsg should flush deltaBuffer, remaining len=%d", model.deltaBuffer.Len())
	}
	if !strings.Contains(model.streamingText, "token0 ") || !strings.Contains(model.streamingText, "token99 ") {
		t.Fatalf("streamingText missing tokens: %q", model.streamingText)
	}

	// Fire EventTextComplete
	updated, _ = model.Update(conversationEventMsg{
		ConversationID: "conversation-1",
		TurnID:         "turn-1",
		Type:           string(agent.EventTextComplete),
		Content:        model.streamingText,
	})
	model = updated.(*conversationTUI)

	if model.tickerActive {
		t.Fatal("tickerActive should be false after text complete")
	}
	if model.streamingText != "" {
		t.Fatalf("streamingText should be cleared after text complete, got: %q", model.streamingText)
	}
	if len(model.blocks) == 0 {
		t.Fatal("expected completed transcript block")
	}
	lastBlock := model.blocks[len(model.blocks)-1]
	if lastBlock.Streaming {
		t.Fatal("completed block should not be marked streaming")
	}
	if !strings.Contains(lastBlock.Body, "token99") {
		t.Fatalf("completed block body missing content: %s", lastBlock.Body)
	}
}

func TestConversationTUIMarkdownTableTruncation(t *testing.T) {
	model := &conversationTUI{
		width:          80,
		height:         24,
		lineCountCache: make(map[string]int),
		wrapCache:      make(map[string][]string),
	}

	// Generate table with 250 rows
	var sb strings.Builder
	sb.WriteString("| Index | Description |\n|---|---|\n")
	for i := 1; i <= 250; i++ {
		sb.WriteString(fmt.Sprintf("| Row %d | Value %d |\n", i, i))
	}
	fullMarkdown := sb.String()

	block := TranscriptBlock{
		ID:        "table-block",
		Kind:      BlockAssistantMessage,
		Role:      "assistant",
		Header:    "Buddy",
		Body:      fullMarkdown,
		CreatedAt: time.Now(),
	}

	rendered := model.renderBlock(block, 80)
	if !strings.Contains(rendered, "[... 50 rows omitted; use /copy to export full table]") {
		t.Fatalf("rendered block should contain 200-row truncation badge: %s", rendered)
	}
	// Check that full markdown in memory is preserved
	if strings.Count(block.Body, "Row ") != 250 {
		t.Fatalf("block.Body in memory should retain all 250 rows, got: %d", strings.Count(block.Body, "Row "))
	}
}

func TestConversationTUIToolEventHyperlink(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	api := &conversationAPITestDouble{events: make(chan protocol.SessionEventNotification, 10)}
	model := &conversationTUI{
		client:         api,
		conversation:   "conversation-1",
		workspace:      "/test/workspace",
		width:          80,
		height:         24,
		lineCountCache: make(map[string]int),
		wrapCache:      make(map[string][]string),
	}

	updated, _ := model.Update(conversationEventMsg{
		ConversationID: "conversation-1",
		TurnID:         "turn-1",
		Type:           string(agent.EventToolCall),
		Data: map[string]any{
			"tool_name": "read_file",
			"path":      "internal/cmd/root.go",
			"line":      float64(42),
		},
	})
	model = updated.(*conversationTUI)

	if len(model.blocks) == 0 {
		t.Fatal("expected tool block created")
	}
	toolBlock := model.blocks[len(model.blocks)-1]
	if !strings.Contains(toolBlock.Header, "Running read_file") {
		t.Fatalf("toolBlock Header missing tool name: %s", toolBlock.Header)
	}
	if !strings.Contains(toolBlock.Header, "internal/cmd/root.go:42") {
		t.Fatalf("toolBlock Header missing file and line: %s", toolBlock.Header)
	}
	if !strings.Contains(toolBlock.Header, "file:///test/workspace/internal/cmd/root.go#L42") {
		t.Fatalf("toolBlock Header missing workspace-resolved file URI: %s", toolBlock.Header)
	}
}

func TestConversationTUICopyCommand(t *testing.T) {
	model := &conversationTUI{
		textarea:       textarea.New(),
		width:          80,
		height:         24,
		lineCountCache: make(map[string]int),
		wrapCache:      make(map[string][]string),
	}
	model.appendBlock(TranscriptBlock{
		ID:        "msg-1",
		Kind:      BlockAssistantMessage,
		Role:      "assistant",
		Header:    "Buddy",
		Body:      "Secret transcript content for clipboard copy",
		CreatedAt: time.Now(),
	})

	model.textarea.SetValue("/copy")
	updated, cmd := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = updated.(*conversationTUI)

	if cmd == nil {
		t.Fatal("expected OSC 52 command returned on /copy")
	}
	if len(model.blocks) < 2 {
		t.Fatal("expected notification block appended after /copy")
	}
	lastBlock := model.blocks[len(model.blocks)-1]
	if !strings.Contains(lastBlock.Body, "Copied full transcript to clipboard") {
		t.Fatalf("expected copy confirmation, got: %s", lastBlock.Body)
	}
}

func TestConversationTUIActivityFocusAndOutputPager(t *testing.T) {
	api := &conversationAPITestDouble{submissions: make(chan protocol.ConversationSubmitParams, 1)}
	model := &conversationTUI{client: api, conversation: "conv-1", textarea: textarea.New(), width: 80, height: 24}
	model.appendBlock(TranscriptBlock{ID: "activity", TurnID: "turn-1", Kind: BlockToolActivity, Header: "✓ bash", Body: "preview", Collapsed: true, Data: map[string]any{"output_ref": "out-1"}})
	updated, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyF6})
	model = updated.(*conversationTUI)
	if !model.transcriptFocused || model.selectedActivity != "activity" {
		t.Fatal("F6 did not focus the transcript activity")
	}
	updated, cmd := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = updated.(*conversationTUI)
	if cmd == nil || model.outputRef != "out-1" {
		t.Fatal("Enter did not open the selected output pager")
	}
	if _, ok := cmd().(outputPageMsg); !ok {
		t.Fatal("output request did not return a page message")
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	model = updated.(*conversationTUI)
	if model.outputRef != "" || !model.transcriptFocused {
		t.Fatal("Escape did not return to transcript focus")
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyF6})
	model = updated.(*conversationTUI)
	updated, _ = model.Update(tea.KeyPressMsg{Text: " ", Code: ' '})
	model = updated.(*conversationTUI)
	if model.textarea.Value() != " " {
		t.Fatalf("Space did not remain composer input: %q", model.textarea.Value())
	}
}

func TestConversationTUIOutputPagerShowsExactByteRange(t *testing.T) {
	model := &conversationTUI{
		outputPage:       &protocol.OutputReadResult{Data: "page", TotalBytes: 262144},
		outputPageStarts: []int64{131072},
		outputPageIndex:  0,
	}
	if view := model.renderOutputView(); !strings.Contains(view, "Bytes 131073–131076 of 262144") {
		t.Fatalf("pager did not show exact byte range: %q", view)
	}
}

func TestConversationTUIDiffNavigationAndDismissal(t *testing.T) {
	model := &conversationTUI{textarea: textarea.New(), width: 80, height: 24, diffView: &protocol.DiffInspectResult{Files: []protocol.DiffFileEntry{{Path: "one.go"}, {Path: "two.go", Diff: "header\n@@ -1 +1 @@\n-a\n+b\n@@ -5 +5 @@\n-c\n+d"}}}}
	updated, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	model = updated.(*conversationTUI)
	if model.diffSelected != 1 {
		t.Fatal("Down did not select next changed file")
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = updated.(*conversationTUI)
	if !model.diffFileOpen {
		t.Fatal("Enter did not open selected diff")
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	model = updated.(*conversationTUI)
	if model.diffScroll != 1 {
		t.Fatal("Down did not scroll within the open diff")
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	model = updated.(*conversationTUI)
	if model.diffScroll != 4 {
		t.Fatalf("PageDown did not navigate to the next hunk: %d", model.diffScroll)
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	model = updated.(*conversationTUI)
	if model.diffFileOpen || model.diffView == nil {
		t.Fatal("Escape did not return to file list")
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	model = updated.(*conversationTUI)
	if model.diffView != nil {
		t.Fatal("Escape did not close diff viewer")
	}
}

func TestActivityIdentityIncludesModelStep(t *testing.T) {
	one := map[string]any{"activity_id": map[string]any{"turn_id": "turn-1", "step": float64(2), "tool_call_id": "call-1"}}
	two := map[string]any{"activity_id": map[string]any{"turn_id": "turn-1", "step": float64(3), "tool_call_id": "call-1"}}
	if activityDataKey(one) == activityDataKey(two) {
		t.Fatal("activity keys collided across model steps")
	}
}

func TestConversationTUIEscapesUntrustedTerminalContent(t *testing.T) {
	malicious := "safe\x1b]52;c;clipboard-payload\x07\x1b[2Jtext\x00"
	clean := terminalText(malicious)
	if strings.Contains(clean, "\x1b") || strings.ContainsRune(clean, '\x00') || !strings.Contains(clean, "safetext") {
		t.Fatalf("terminal controls were not removed: %q", clean)
	}
	model := &conversationTUI{width: 80, height: 24, diffView: &protocol.DiffInspectResult{Files: []protocol.DiffFileEntry{{Path: malicious, Diff: malicious}}}}
	view := model.renderDiffView()
	if strings.Contains(view, "\x1b]52") || strings.Contains(view, "\x1b[2J") {
		t.Fatalf("diff view retained terminal control sequences: %q", view)
	}
}

func TestConversationTUIGroupsSubagentActivities(t *testing.T) {
	model := &conversationTUI{width: 80, height: 24}
	parent := TranscriptBlock{ID: "parent", Kind: BlockToolActivity, Header: "✓ invoke_subagent", Collapsed: true, Data: map[string]any{"tool_name": "invoke_subagent", "agent_name": "worker"}}
	child := TranscriptBlock{ID: "child", ParentID: "parent", Kind: BlockSubagentActivity, Header: "✓ read_file", Body: "file contents", Collapsed: true, Completed: true, Data: map[string]any{"output_ref": "child-output"}}
	model.blocks = []TranscriptBlock{parent, child}
	collapsed := model.renderBlock(parent, 80)
	if !strings.Contains(collapsed, "[Subagent: worker (1 step completed)]") || strings.Contains(collapsed, "read_file") {
		t.Fatalf("collapsed parent did not summarize child activity: %q", collapsed)
	}
	parent.Collapsed = false
	expanded := model.renderBlock(parent, 80)
	if !strings.Contains(expanded, "read_file") || strings.Contains(strings.Join(model.computeVisibleLines(), "\n"), "file contents") {
		t.Fatalf("expanded grouping did not nest child activity: %q", expanded)
	}
	model.transcriptFocused = true
	model.blocks[0].Collapsed = false
	model.selectedActivity = "parent"
	model.selectActivity(1)
	if model.selectedActivity != "child" {
		t.Fatalf("expanded child was not selectable: %q", model.selectedActivity)
	}
	updated, cmd := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = updated.(*conversationTUI)
	if cmd == nil || model.outputRef != "child-output" {
		t.Fatal("selected child output did not open its pager")
	}
}

func TestConversationTUIResultOnlyChildIsDetached(t *testing.T) {
	model := &conversationTUI{conversation: "conv-1"}
	updated, _ := model.Update(conversationEventMsg(protocol.SessionEventNotification{
		ConversationID: "conv-1",
		TurnID:         "turn-1",
		Type:           string(agent.EventToolResult),
		Content:        "read complete",
		Data: map[string]any{
			"tool_name":          "read_file",
			"activity_id":        map[string]any{"turn_id": "turn-1", "step": float64(2), "tool_call_id": "child-call"},
			"parent_activity_id": map[string]any{"turn_id": "turn-1", "step": float64(1), "tool_call_id": "parent-call"},
		},
	}))
	model = updated.(*conversationTUI)
	if len(model.blocks) != 1 || model.blocks[0].Kind != BlockSubagentActivity || !strings.HasPrefix(model.blocks[0].Header, "Detached child · ") {
		t.Fatalf("result-only child was not rendered as detached: %+v", model.blocks)
	}
}

func TestConversationTUILongBlockClipping(t *testing.T) {
	model := &conversationTUI{
		textarea:       textarea.New(),
		width:          80,
		height:         24,
		lineCountCache: make(map[string]int),
		wrapCache:      make(map[string][]string),
	}

	// 100-line single assistant response
	var longLines []string
	for i := 1; i <= 100; i++ {
		longLines = append(longLines, fmt.Sprintf("Response line %d", i))
	}
	model.appendBlock(TranscriptBlock{
		ID:        "long-response",
		Kind:      BlockAssistantMessage,
		Role:      "assistant",
		Header:    "Buddy",
		Body:      strings.Join(longLines, "\n"),
		CreatedAt: time.Now(),
	})

	// At bottom (scrollOffset = 0)
	model.scrollOffset = 0
	linesBottom := model.computeVisibleLines()
	if len(linesBottom) >= 100 {
		t.Fatalf("visible lines should be clipped to viewport height, got %d", len(linesBottom))
	}
	bottomText := strings.Join(linesBottom, "\n")
	if !strings.Contains(bottomText, "Response line 100") {
		t.Fatalf("bottom view missing last response line: %s", bottomText)
	}
	if strings.Contains(bottomText, "Response line 1\n") {
		t.Fatalf("bottom view should not contain first response line: %s", bottomText)
	}

	// Scroll up
	model.scrollOffset = 1000 // clamped to top
	linesTop := model.computeVisibleLines()
	topText := strings.Join(linesTop, "\n")
	if !strings.Contains(topText, "Response line 1") {
		t.Fatalf("top view missing first response line: %s", topText)
	}
	if strings.Contains(topText, "Response line 100") {
		t.Fatalf("top view should not contain last response line: %s", topText)
	}
}

func TestConversationTUIQuitCommand(t *testing.T) {
	for _, cmdStr := range []string{"/quit", "/exit"} {
		model := &conversationTUI{
			textarea:       textarea.New(),
			width:          80,
			height:         24,
			lineCountCache: make(map[string]int),
			wrapCache:      make(map[string][]string),
		}
		model.textarea.SetValue(cmdStr)
		_, cmd := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		if cmd == nil {
			t.Fatalf("expected tea.Quit command on %s, got nil", cmdStr)
		}
		msg := cmd()
		if _, ok := msg.(tea.QuitMsg); !ok {
			t.Fatalf("expected QuitMsg on %s, got %T", cmdStr, msg)
		}
	}
}

func TestConversationTUIClearCommand(t *testing.T) {
	model := &conversationTUI{
		textarea:       textarea.New(),
		width:          80,
		height:         24,
		scrollOffset:   5,
		lineCountCache: make(map[string]int),
		wrapCache:      make(map[string][]string),
	}
	model.appendBlock(TranscriptBlock{
		ID:        "block-1",
		Kind:      BlockAssistantMessage,
		Role:      "assistant",
		Header:    "Buddy",
		Body:      "Some text",
		CreatedAt: time.Now(),
	})
	model.textarea.SetValue("/clear")
	updated, cmd := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd != nil {
		t.Fatalf("expected nil cmd on /clear, got %v", cmd)
	}
	cleared := updated.(*conversationTUI)
	if len(cleared.blocks) != 0 {
		t.Fatalf("expected 0 blocks after /clear, got %d", len(cleared.blocks))
	}
	if len(cleared.transcript) != 0 {
		t.Fatalf("expected 0 transcript entries after /clear, got %d", len(cleared.transcript))
	}
	if cleared.scrollOffset != 0 {
		t.Fatalf("expected scrollOffset 0 after /clear, got %d", cleared.scrollOffset)
	}
}

func TestConversationTUIClearKeepsStreamingBlock(t *testing.T) {
	model := &conversationTUI{
		textarea:       textarea.New(),
		activeBlockID:  "active",
		state:          "running",
		lineCountCache: make(map[string]int),
		wrapCache:      make(map[string][]string),
	}
	model.appendBlock(TranscriptBlock{ID: "old", Body: "old"})
	model.appendBlock(TranscriptBlock{ID: "active", Body: "partial", Streaming: true})
	model.textarea.SetValue("/clear")
	updated, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	cleared := updated.(*conversationTUI)
	if len(cleared.blocks) != 1 || cleared.blocks[0].ID != "active" || cleared.activeBlockID != "active" {
		t.Fatalf("clear lost active streaming block: blocks=%+v active=%q", cleared.blocks, cleared.activeBlockID)
	}
}

func TestConversationTUIComposerCompletions(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "notes.md"), []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	model := &conversationTUI{textarea: textarea.New(), workspace: workspace, height: 30}
	model.slashCommands.Register(SlashCommand{Name: "/clear", Description: "Clear transcript"})
	model.slashCommands.Register(SlashCommand{Name: "/copy", Description: "Copy transcript"})
	model.textarea.SetValue("/cl")
	model.updateCompletion()
	if model.completionKind != "command" || len(model.commandMatches) != 1 {
		t.Fatalf("slash completion = %q, %+v", model.completionKind, model.commandMatches)
	}
	updated, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	model = updated.(*conversationTUI)
	if got := model.textarea.Value(); got != "/clear" {
		t.Fatalf("slash completion inserted %q", got)
	}
	model.textarea.SetValue("/co")
	model.updateCompletion()
	shortenedViewport := model.viewportHeight()
	if model.completionRows() == 0 {
		t.Fatal("active completion popup did not reserve viewport rows")
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	model = updated.(*conversationTUI)
	if model.completionKind != "" || model.textarea.Value() != "/co" {
		t.Fatalf("escape changed composer draft: popup=%q draft=%q", model.completionKind, model.textarea.Value())
	}
	if model.completionRows() != 0 || model.viewportHeight() <= shortenedViewport {
		t.Fatalf("dismissed popup still reserves transcript rows: rows=%d viewport=%d", model.completionRows(), model.viewportHeight())
	}
	model.textarea.SetValue("@not")
	model.updateCompletion()
	if model.completionKind != "file" || len(model.fileMatches) != 1 || model.fileMatches[0] != "notes.md" {
		t.Fatalf("file completion = %q, %+v", model.completionKind, model.fileMatches)
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	model = updated.(*conversationTUI)
	if got := model.textarea.Value(); got != "notes.md" {
		t.Fatalf("file completion inserted %q", got)
	}
}

func TestConversationTUILoadSyncSkipsEmptyAssistantToolSteps(t *testing.T) {
	model := &conversationTUI{textarea: textarea.New(), width: 80, height: 24}
	model.loadSync(&protocol.ConversationSyncResult{ConversationID: "conversation-1", Transcript: []protocol.ConversationTranscriptEntry{
		{Role: "user", Content: "inspect files"},
		{Role: "assistant", Content: ""},
		{Role: "assistant", Content: " \n  "},
		{Role: "assistant", Content: "Found one file."},
	}})
	if len(model.blocks) != 2 {
		t.Fatalf("restored visible block count = %d, want 2: %+v", len(model.blocks), model.blocks)
	}
	lines := strings.Join(model.computeVisibleLines(), "\n")
	if strings.Count(lines, "Buddy") != 1 || !strings.Contains(lines, "Found one file.") {
		t.Fatalf("restored transcript contains empty assistant rows: %q", lines)
	}
}

func TestConversationTUIKeepsStreamingWhileApprovalCardIsOpen(t *testing.T) {
	model := &conversationTUI{
		conversation: "conversation-1", state: "running", width: 80, height: 24,
		textarea: textarea.New(),
	}
	updated, _ := model.Update(permissionRequestMsg(protocol.PermissionRequestNotification{
		ConversationID: "conversation-1", TurnID: "turn-1", RequestID: "request-1", Tool: "bash",
		Arguments: map[string]any{"command": "go test ./..."},
	}))
	model = updated.(*conversationTUI)
	updated, _ = model.Update(conversationEventMsg(protocol.SessionEventNotification{
		ConversationID: "conversation-1", TurnID: "turn-1", Type: string(agent.EventTextDelta), Content: "Still working",
	}))
	model = updated.(*conversationTUI)
	if model.pendingApproval == nil || model.approvalModal == nil || model.activeBlockID == "" {
		t.Fatalf("approval card blocked streaming: approval=%+v activeBlock=%q", model.pendingApproval, model.activeBlockID)
	}
}

func TestConversationTUIRoutesPasteToApprovalEditor(t *testing.T) {
	request := &protocol.PermissionRequestNotification{RequestID: "request-1", Tool: "write_file", Arguments: map[string]any{"path": "old.go"}}
	model := &conversationTUI{
		textarea:        textarea.New(),
		pendingApproval: request,
		approvalModal:   newApprovalModalState(request.Arguments, 80, 24),
		conversation:    "conversation-1",
	}
	model.textarea.SetValue("draft")
	updated, _ := model.Update(tea.KeyPressMsg{Text: "4", Code: '4'})
	model = updated.(*conversationTUI)
	model.approvalModal.editor.CursorEnd()
	for range len(model.approvalModal.editor.Value()) {
		updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyBackspace})
		model = updated.(*conversationTUI)
	}
	updated, _ = model.Update(tea.PasteMsg{Content: `{"path":"pasted.go"}`})
	model = updated.(*conversationTUI)
	if model.textarea.Value() != "draft" {
		t.Fatalf("paste changed main composer: %q", model.textarea.Value())
	}
	if model.approvalModal.editor.Value() != `{"path":"pasted.go"}` {
		t.Fatalf("paste did not update approval editor: %q", model.approvalModal.editor.Value())
	}
	updated, _ = model.Update(conversationEventMsg{
		ConversationID: "conversation-1", TurnID: "turn-1", Type: string(agent.EventTextDelta), Content: "stream continues",
	})
	model = updated.(*conversationTUI)
	if !strings.Contains(model.deltaBuffer.String(), "stream continues") {
		t.Fatalf("editing approval blocked conversation events: %q", model.deltaBuffer.String())
	}
}

func TestConversationTUICompletionStateSurvivesNonEditingMessages(t *testing.T) {
	model := &conversationTUI{textarea: textarea.New(), width: 80, height: 30}
	model.slashCommands.Register(SlashCommand{Name: "/clear"})
	model.slashCommands.Register(SlashCommand{Name: "/copy"})
	model.textarea.SetValue("/c")
	model.updateCompletion()
	updated, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	model = updated.(*conversationTUI)
	if model.completionCursor != 1 {
		t.Fatalf("completion selection = %d, want 1", model.completionCursor)
	}
	updated, _ = model.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	model = updated.(*conversationTUI)
	if model.completionKind != "command" || model.completionCursor != 1 {
		t.Fatalf("non-editing message changed completion selection: kind=%q cursor=%d", model.completionKind, model.completionCursor)
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	model = updated.(*conversationTUI)
	updated, _ = model.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	model = updated.(*conversationTUI)
	if model.completionKind != "" || model.completionRows() != 0 {
		t.Fatalf("non-editing message reopened dismissed completion: kind=%q rows=%d", model.completionKind, model.completionRows())
	}
}

func TestConversationTUIHistoryNavigationRestoresDraft(t *testing.T) {
	history, err := NewInputHistoryRing(filepath.Join(t.TempDir(), "history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := history.Add("older"); err != nil {
		t.Fatal(err)
	}
	if err := history.Add("newer"); err != nil {
		t.Fatal(err)
	}
	model := &conversationTUI{textarea: textarea.New(), history: history, lineCountCache: make(map[string]int), wrapCache: make(map[string][]string)}
	model.textarea.SetValue("draft")
	model.completionKind = "file"
	model.fileMatches = []string{"stale"}
	model.completionStart, model.completionEnd = 0, 5
	updated, _ := model.Update(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	model = updated.(*conversationTUI)
	if got := model.textarea.Value(); got != "newer" {
		t.Fatalf("history up selected %q, want newer", got)
	}
	if model.completionKind != "" || model.fileMatches != nil || model.completionEnd != 0 {
		t.Fatalf("history navigation retained completion state: %+v", model)
	}
	for _, key := range []tea.KeyPressMsg{{Code: 'p', Mod: tea.ModCtrl}, {Code: 'n', Mod: tea.ModCtrl}, {Code: 'n', Mod: tea.ModCtrl}} {
		updated, _ := model.Update(key)
		model = updated.(*conversationTUI)
	}
	if got := model.textarea.Value(); got != "draft" {
		t.Fatalf("history down restored %q, want draft", got)
	}
}

func TestConversationTUIHistoryNavigationRespectsSoftWrapRows(t *testing.T) {
	history, err := NewInputHistoryRing(filepath.Join(t.TempDir(), "history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := history.Add("saved prompt"); err != nil {
		t.Fatal(err)
	}
	input := textarea.New()
	input.SetWidth(8)
	input.SetValue("a long wrapped draft")
	input.CursorStart()
	model := &conversationTUI{textarea: input, history: history}
	if info := model.textarea.LineInfo(); info.Height < 2 {
		t.Fatalf("test draft did not wrap: %+v", info)
	}
	updated, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	model = updated.(*conversationTUI)
	if got := model.textarea.Value(); got != "a long wrapped draft" || history.index != len(history.entries) {
		t.Fatalf("Down recalled history from a non-final visual row: draft=%q index=%d", got, history.index)
	}
	model.textarea.CursorEnd()
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	model = updated.(*conversationTUI)
	if got := model.textarea.Value(); got != "a long wrapped draft" || history.index != len(history.entries) {
		t.Fatalf("Up recalled history from a non-first visual row: draft=%q index=%d", got, history.index)
	}
}

func TestConversationTUIFileCompletionKeepsSelectionVisible(t *testing.T) {
	workspace := t.TempDir()
	for i := 0; i < 12; i++ {
		name := fmt.Sprintf("file-%02d", i)
		if err := os.WriteFile(filepath.Join(workspace, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	model := &conversationTUI{textarea: textarea.New(), workspace: workspace, width: 80, height: 24}
	model.textarea.SetValue("@")
	model.updateCompletion()
	for i := 0; i < 10; i++ {
		updated, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		model = updated.(*conversationTUI)
	}
	selected := model.fileMatches[model.completionCursor]
	if !strings.Contains(model.View().Content, "› "+selected) {
		t.Fatalf("selected completion %q is not visible", selected)
	}
}

func TestConversationTUICompletionReservesViewportRows(t *testing.T) {
	workspace := t.TempDir()
	for i := 0; i < 12; i++ {
		if err := os.WriteFile(filepath.Join(workspace, fmt.Sprintf("file-%02d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	model := &conversationTUI{textarea: textarea.New(), workspace: workspace, width: 80, height: 19}
	model.textarea.SetValue("@")
	model.updateCompletion()
	for i := 0; i < 10; i++ {
		updated, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		model = updated.(*conversationTUI)
	}
	if got := model.completionRows(); got != 4 {
		t.Fatalf("completion rows = %d, want 4", got)
	}
	if got := model.viewportHeight(); got != 4 {
		t.Fatalf("transcript viewport height = %d, want 4 after reserving completion rows", got)
	}
	visible := 0
	for _, line := range strings.Split(model.View().Content, "\n") {
		if strings.HasPrefix(line, "  file-") || strings.HasPrefix(line, "› file-") {
			visible++
		}
	}
	if visible != 4 {
		t.Fatalf("rendered %d completion rows, want 4", visible)
	}
	if !strings.Contains(model.View().Content, "› "+model.fileMatches[model.completionCursor]) {
		t.Fatal("selected completion is not visible in the constrained popup")
	}
}
