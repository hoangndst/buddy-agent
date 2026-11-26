package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/hoangndst/buddy-agent/internal/agent"
	"github.com/hoangndst/buddy-agent/internal/cmdutil"

	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
	"github.com/hoangndst/buddy-agent/internal/mcp"
	"github.com/hoangndst/buddy-agent/internal/persistence"
	"github.com/hoangndst/buddy-agent/internal/tui"
)

type conversationAPITestDouble struct {
	startGate           chan struct{}
	startCalls          int
	submissions         chan protocol.ConversationSubmitParams
	events              chan protocol.SessionEventNotification
	outputReads         []protocol.OutputReadParams
	permissionResponses []protocol.PermissionRespondParams
	interrupts          []protocol.TurnInterruptParams
	conversationLists   []protocol.ConversationListParams
	resumeRequests      []protocol.ConversationResumeParams
	conversationResult  *protocol.ConversationListResult
	diffResult          *protocol.DiffInspectResult
	syncResult          *protocol.ConversationSyncResult
}

func testTUIModel(width, height int) *tui.Model {
	model := tui.NewModel(tui.Header{})
	model.Width, model.Height = width, height
	model.Composer.Resize(width, height)
	return model
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
func (f *conversationAPITestDouble) ResumeConversation(params protocol.ConversationResumeParams) (*protocol.ConversationResumeResult, error) {
	f.resumeRequests = append(f.resumeRequests, params)
	return &protocol.ConversationResumeResult{ConversationID: params.ConversationID}, nil
}
func (f *conversationAPITestDouble) SyncConversation(protocol.ConversationSyncParams) (*protocol.ConversationSyncResult, error) {
	if f.syncResult != nil {
		return f.syncResult, nil
	}
	return &protocol.ConversationSyncResult{}, nil
}
func (f *conversationAPITestDouble) ListConversationWorkspaces(int) (*protocol.ConversationListWorkspacesResult, error) {
	return nil, nil
}
func (f *conversationAPITestDouble) ListConversations(params protocol.ConversationListParams) (*protocol.ConversationListResult, error) {
	f.conversationLists = append(f.conversationLists, params)
	if f.conversationResult != nil {
		return f.conversationResult, nil
	}
	return &protocol.ConversationListResult{}, nil
}
func (f *conversationAPITestDouble) SubmitConversation(params protocol.ConversationSubmitParams) (*protocol.ConversationSubmitResult, error) {
	f.submissions <- params
	return &protocol.ConversationSubmitResult{ConversationID: params.ConversationID, SubmissionID: params.SubmissionID}, nil
}
func (f *conversationAPITestDouble) InterruptTurn(params protocol.TurnInterruptParams) (*protocol.TurnInterruptResult, error) {
	f.interrupts = append(f.interrupts, params)
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
func (f *conversationAPITestDouble) RespondConversationPermissionParams(params protocol.PermissionRespondParams) error {
	f.permissionResponses = append(f.permissionResponses, params)
	return nil
}

func setTestOutputInspection(m *conversationTUI, ref string, page *tui.OutputPage) {
	m.ensureUIModel()
	m.inspectionGeneration = 1
	_, _ = m.ui.Update(tui.Event{Kind: tui.EventInspectionStart, InspectionKind: tui.InspectionOutput, InspectionGeneration: 1, InspectionRef: ref})
	_, _ = m.ui.Update(tui.Event{Kind: tui.EventOutputPage, InspectionGeneration: 1, OutputPage: page})
}

func setTestDiffInspection(m *conversationTUI, result *protocol.DiffInspectResult) {
	m.ensureUIModel()
	m.inspectionGeneration = 1
	_, _ = m.ui.Update(tui.Event{Kind: tui.EventInspectionStart, InspectionKind: tui.InspectionDiff, InspectionGeneration: 1})
	_, _ = m.ui.Update(tui.Event{Kind: tui.EventDiffResult, InspectionGeneration: 1, DiffView: toTUIDiffView(result)})
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

func TestConversationActionMessagesReachExistingRuntimeCalls(t *testing.T) {
	api := &conversationAPITestDouble{
		submissions: make(chan protocol.ConversationSubmitParams, 1),
		syncResult:  &protocol.ConversationSyncResult{ConversationID: "conv-1", ActiveTurnID: "turn-1"},
	}
	model := &conversationTUI{client: api, conversation: "conv-1", ui: testTUIModel(80, 24)}
	updated, submit := model.Update(tui.ActionMsg{Action: tui.Action{Kind: tui.ActionSubmitPrompt, Prompt: "queue me", Mode: string(persistence.SubmissionQueue)}})
	model = updated.(*conversationTUI)
	if submit == nil {
		t.Fatal("submit action did not reach the runtime adapter")
	}
	if _, ok := submit().(conversationSubmittedMsg); !ok {
		t.Fatal("submit action did not call the existing conversation API")
	}
	params := <-api.submissions
	if params.ConversationID != "conv-1" || params.Prompt != "queue me" || params.Mode != string(persistence.SubmissionQueue) {
		t.Fatalf("submit action mapped incorrectly: %+v", params)
	}
	_, interrupt := model.Update(tui.ActionMsg{Action: tui.Action{Kind: tui.ActionInterruptTurn}})
	if interrupt == nil {
		t.Fatal("interrupt action did not reach the runtime adapter")
	}
	if _, ok := interrupt().(conversationSyncMsg); !ok {
		t.Fatal("interrupt action did not finish through the existing API path")
	}
	if len(api.interrupts) != 1 || api.interrupts[0].ConversationID != "conv-1" || api.interrupts[0].TurnID != "turn-1" {
		t.Fatalf("interrupt action mapped incorrectly: %+v", api.interrupts)
	}
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

func TestLocalCommandsDoNotInvalidateEvictionRecovery(t *testing.T) {
	for _, command := range []string{"/help", "/clear", "/copy"} {
		t.Run(command, func(t *testing.T) {
			model := &conversationTUI{
				client: &conversationAPITestDouble{}, conversation: "conversation-1", workerEpoch: "epoch-1",
				resumeGeneration: 4, state: "restoring", ui: testTUIModel(80, 24),
			}
			model.handlePromptInput(command)
			if model.resumeGeneration != 4 {
				t.Fatalf("local command invalidated recovery: generation=%d", model.resumeGeneration)
			}
			updated, watch := model.Update(conversationResumedMsg{
				resumeGeneration: 4, sourceConversationID: "conversation-1", sourceWorkerEpoch: "epoch-1",
				result: &protocol.ConversationSyncResult{ConversationID: "conversation-1", WorkerEpoch: "epoch-1", State: "idle"},
			})
			model = updated.(*conversationTUI)
			if model.state != "idle" {
				t.Fatalf("eviction recovery response was discarded after %s: state=%q", command, model.state)
			}
			if watch == nil {
				t.Fatalf("accepted recovery did not restart event watching after %s", command)
			}
		})
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

		ui: testTUIModel(80, 24),
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
	model.ui.Transcript.ScrollOffset = 0
	visible := model.computeVisibleBlocks()
	if len(visible) == 0 || len(visible) >= len(model.ui.Transcript.Blocks) {
		t.Fatalf("expected visible subset of blocks, got %d of %d", len(visible), len(model.ui.Transcript.Blocks))
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
	model.ui.Transcript.ScrollOffset = 1000 // will be clamped to maxScroll
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

func TestConversationTUIWelcomeResizesToNarrowFallback(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	standard := &conversationTUI{ui: testTUIModel(80, 24), workspace: "/workspace/buddy", state: "ready"}
	updated, _ := standard.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	standard = updated.(*conversationTUI)
	standardView := ansi.Strip(standard.View().Content)
	if !strings.Contains(standardView, "╭─ Buddy Agent") || !strings.Contains(standardView, "Tips for getting started") || !strings.Contains(standardView, "╰") {
		t.Fatalf("standard welcome panel was clipped: %q", standardView)
	}
	rows := strings.Split(standardView, "\n")
	if len(rows) > 24 {
		t.Fatalf("standard welcome has %d rows for a 24-row terminal", len(rows))
	}
	if !strings.HasPrefix(rows[0], "╭─ Buddy Agent") {
		t.Fatalf("welcome panel should start at the top without a duplicate header: %q", rows[:min(3, len(rows))])
	}
	standard.appendBlock(TranscriptBlock{ID: "user-prompt", Kind: BlockUserMessage, Body: "Start a conversation"})
	transcriptView := ansi.Strip(standard.View().Content)
	if strings.Contains(transcriptView, "Welcome back!") || strings.Contains(transcriptView, "Tips for getting started") {
		t.Fatalf("welcome panel remained after transcript content appeared: %q", transcriptView)
	}

	model := &conversationTUI{ui: testTUIModel(160, 40), workspace: "/workspace/buddy", gitBranch: "main", state: "ready", startParams: protocol.ConversationStartParams{Model: "GPT-5.4"}}
	wide := ansi.Strip(model.View().Content)
	if !strings.Contains(wide, "▀") || !strings.Contains(wide, "GPT-5.4") || !strings.Contains(wide, "main") {
		t.Fatalf("wide welcome missing mascot or project metadata: %q", wide)
	}

	updated, _ = model.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	model = updated.(*conversationTUI)
	narrow := ansi.Strip(model.View().Content)
	if strings.Contains(narrow, "▀") {
		t.Fatalf("narrow welcome should hide the mascot: %q", narrow)
	}
	if lines := strings.Split(narrow, "\n"); len(lines) > 12 {
		t.Fatalf("narrow welcome has %d rows for a 12-row terminal", len(lines))
	}
	for _, line := range strings.Split(narrow, "\n") {
		if width := ansi.StringWidth(line); width > 40 {
			t.Fatalf("narrow welcome line has width %d: %q", width, line)
		}
	}
}

func TestConversationTUIResizeReflowsTranscript(t *testing.T) {
	model := &conversationTUI{ui: testTUIModel(80, 24)}
	model.appendBlock(TranscriptBlock{ID: "test-block-1", Kind: BlockAssistantMessage, Body: "Short content for testing transcript resize and reflow."})
	before := strings.Join(model.computeVisibleLines(), "\n")
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	model = updated.(*conversationTUI)
	after := strings.Join(model.computeVisibleLines(), "\n")
	if !strings.Contains(before, "Short content") || !strings.Contains(after, "Short content") {
		t.Fatalf("resize lost transcript content\nbefore: %q\nafter: %q", before, after)
	}
}

func TestConversationTUIStreamingDeltaCoalescingThreadSafe(t *testing.T) {
	api := &conversationAPITestDouble{events: make(chan protocol.SessionEventNotification, 10)}
	model := &conversationTUI{
		client:       api,
		conversation: "conversation-1",
		state:        "running",
		ui:           testTUIModel(80, 24),
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

	if !model.ui.TickerActive() {
		t.Fatal("ticker should be active while streaming deltas")
	}
	if !model.ui.HasBufferedDelta() {
		t.Fatal("deltaBuffer should have buffered deltas")
	}

	// Fire render tick
	updated, _ := model.Update(tui.FlushMsg{})
	model = updated.(*conversationTUI)

	if model.ui.HasBufferedDelta() {
		t.Fatalf("renderTickMsg should flush deltaBuffer, remaining buffered=%t", model.ui.HasBufferedDelta())
	}
	if !strings.Contains(model.ui.StreamingText(), "token0 ") || !strings.Contains(model.ui.StreamingText(), "token99 ") {
		t.Fatalf("streamingText missing tokens: %q", model.ui.StreamingText())
	}

	// Fire EventTextComplete
	updated, _ = model.Update(conversationEventMsg{
		ConversationID: "conversation-1",
		TurnID:         "turn-1",
		Type:           string(agent.EventTextComplete),
		Content:        model.ui.StreamingText(),
	})
	model = updated.(*conversationTUI)

	if model.ui.TickerActive() {
		t.Fatal("tickerActive should be false after text complete")
	}
	if model.ui.StreamingText() != "" {
		t.Fatalf("streamingText should be cleared after text complete, got: %q", model.ui.StreamingText())
	}
	if len(model.ui.Transcript.Blocks) == 0 {
		t.Fatal("expected completed transcript block")
	}
	lastBlock := model.ui.Transcript.Blocks[len(model.ui.Transcript.Blocks)-1]
	if lastBlock.Streaming {
		t.Fatal("completed block should not be marked streaming")
	}
	if !strings.Contains(lastBlock.Body, "token99") {
		t.Fatalf("completed block body missing content: %s", lastBlock.Body)
	}
}

func TestConversationTUIMarkdownTableRenderingKeepsSource(t *testing.T) {
	model := &conversationTUI{
		ui: testTUIModel(80, 24),
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

	rendered, _ := model.renderBlock(block, 80)
	rendered = ansi.Strip(rendered)
	if !strings.Contains(rendered, "Row 1") || !strings.Contains(rendered, "Row 250") {
		t.Fatalf("rendered Markdown table should retain all rows: %s", rendered)
	}
	// Check that full markdown in memory is preserved
	if strings.Count(block.Body, "Row ") != 250 {
		t.Fatalf("block.Body in memory should retain all 250 rows, got: %d", strings.Count(block.Body, "Row "))
	}
}

func TestConversationTUIToolEventHyperlink(t *testing.T) {
	t.Setenv("TERM", "xterm-256color")
	t.Setenv("NO_COLOR", "")
	api := &conversationAPITestDouble{events: make(chan protocol.SessionEventNotification, 10)}
	model := &conversationTUI{
		client:       api,
		conversation: "conversation-1",
		workspace:    "/test/workspace",
		ui:           testTUIModel(80, 24),
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

	if len(model.ui.Transcript.Blocks) == 0 {
		t.Fatal("expected tool block created")
	}
	toolBlock := model.ui.Transcript.Blocks[len(model.ui.Transcript.Blocks)-1]
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

func TestConversationTUIMouseDragCopiesTranscriptSelection(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	model := &conversationTUI{ui: testTUIModel(80, 8)}
	model.ui.SetScreenContent(tui.ScreenContent{Header: []string{"header"}, Footer: []string{"footer"}})
	model.appendBlock(TranscriptBlock{Kind: BlockUserMessage, Body: "copy this text"})
	if _, cmd := model.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 2, Y: 1}); cmd != nil {
		t.Fatal("mouse press unexpectedly ran a command")
	}
	if _, cmd := model.Update(tea.MouseMotionMsg{Button: tea.MouseLeft, X: 5, Y: 1}); cmd != nil {
		t.Fatal("mouse drag unexpectedly ran a command")
	}
	_, cmd := model.Update(tea.MouseReleaseMsg{Button: tea.MouseLeft, X: 5, Y: 1})
	if cmd == nil || fmt.Sprint(cmd()) != "copy" {
		t.Fatalf("mouse release clipboard command = %v", cmd)
	}
	for _, mod := range []tea.KeyMod{tea.ModCtrl, tea.ModSuper} {
		_, cmd := model.Update(tea.KeyPressMsg{Code: 'c', Mod: mod})
		if cmd == nil || fmt.Sprint(cmd()) != "copy" {
			t.Fatalf("copy shortcut with modifier %v did not copy the selection", mod)
		}
	}
}

func TestConversationTUIClickExpandsActivityDisclosure(t *testing.T) {
	model := &conversationTUI{ui: testTUIModel(80, 8)}
	model.ui.SetScreenContent(tui.ScreenContent{Header: []string{"header"}, Footer: []string{"footer"}})
	model.appendBlock(TranscriptBlock{ID: "group", Kind: BlockActivityGroup, Collapsed: true})
	model.appendBlock(TranscriptBlock{ID: "call", GroupID: "group", Kind: BlockToolActivity, Header: "read_file", Collapsed: true})
	model.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: 4, Y: 1})
	_, cmd := model.Update(tea.MouseReleaseMsg{Button: tea.MouseLeft, X: 4, Y: 1})
	if cmd != nil || model.ui.Transcript.Blocks[0].Collapsed {
		t.Fatalf("mouse click did not expand activity disclosure: cmd=%v group=%+v", cmd, model.ui.Transcript.Blocks[0])
	}
}

func TestConversationTUIClickOpensRenderedMarkdownLinkOnRelease(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	model := &conversationTUI{ui: testTUIModel(80, 8)}
	model.ui.SetScreenContent(tui.ScreenContent{Header: []string{"header"}, Footer: []string{"footer"}})
	model.appendBlock(TranscriptBlock{Kind: BlockAssistantMessage, Body: "[Open](https://example.com/path)"})
	line := ansi.Strip(model.ui.Transcript.VisibleLines(6, 80)[0])
	column := strings.Index(line, "Open")
	model.Update(tea.MouseClickMsg{Button: tea.MouseLeft, X: column, Y: 1})
	_, cmd := model.Update(tea.MouseReleaseMsg{Button: tea.MouseLeft, X: column, Y: 1})
	if cmd == nil || model.ui.HyperlinkAt(1, column) != "https://example.com/path" {
		t.Fatal("mouse release did not dispatch the rendered Markdown link")
	}
}

type terminalOutputFunc func([]byte) (int, error)

func (f terminalOutputFunc) Write(p []byte) (int, error) { return f(p) }

func TestConversationTUICopyCommandInAlternateScreen(t *testing.T) {
	model := &conversationTUI{

		ui: testTUIModel(80, 24),
	}
	model.appendBlock(TranscriptBlock{Kind: BlockUserMessage, Body: "Hello"})
	model.appendBlock(TranscriptBlock{Kind: BlockAssistantMessage, Body: "Hi there 👋"})
	model.ui.Composer.Editor.SetValue("/copy")

	altScreen, clipboard := make(chan struct{}), make(chan struct{})
	enteredAltScreen := sync.OnceFunc(func() { close(altScreen) })
	var clipboardOnce sync.Once
	var clipboardOutput string
	expected := ansi.SetClipboard(ansi.SystemClipboard, "Hello\n\nHi there 👋")
	output := terminalOutputFunc(func(p []byte) (int, error) {
		data := string(p)
		if strings.Contains(data, ansi.SetModeAltScreenSaveCursor) {
			enteredAltScreen()
		}
		if strings.Contains(data, expected) {
			clipboardOnce.Do(func() {
				clipboardOutput = data
				close(clipboard)
			})
		}
		return len(p), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	program := tea.NewProgram(model, tea.WithContext(ctx), tea.WithInput(nil), tea.WithOutput(output),
		tea.WithWindowSize(80, 24), tea.WithoutSignalHandler())
	go func() {
		select {
		case <-altScreen:
			program.Send(tea.KeyPressMsg{Code: tea.KeyEnter})
		case <-ctx.Done():
			return
		}
		select {
		case <-clipboard:
			program.Quit()
		case <-ctx.Done():
		}
	}()
	if _, err := program.Run(); err != nil {
		t.Fatalf("expected /copy to write OSC 52 while the alternate screen was active: %v", err)
	}
	<-clipboard
	if strings.Contains(clipboardOutput, ansi.InsertLine(1)) {
		t.Fatalf("clipboard write inserted a terminal line: %q", clipboardOutput)
	}
	if len(model.ui.Transcript.Blocks) != 3 {
		t.Fatalf("expected copy status block, got %d transcript blocks", len(model.ui.Transcript.Blocks))
	}
	lastBlock := model.ui.Transcript.Blocks[len(model.ui.Transcript.Blocks)-1]
	if lastBlock.Body != "Requested clipboard copy of full transcript." {
		t.Fatalf("unexpected clipboard status: %q", lastBlock.Body)
	}
}

func TestCopyLatestResponseAndRejectUnsafeLinkSchemes(t *testing.T) {
	model := &conversationTUI{ui: testTUIModel(80, 24)}
	model.appendBlock(TranscriptBlock{Kind: BlockAssistantMessage, Body: "older answer"})
	model.appendBlock(TranscriptBlock{Kind: BlockAssistantMessage, Body: "latest answer"})
	_, cmd := model.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModAlt})
	if cmd == nil || fmt.Sprint(cmd()) != "latest answer" {
		t.Fatal("Alt+C did not copy the latest assistant answer")
	}
	msg, ok := openExternalLink("javascript:alert(1)")().(linkOpenedMsg)
	if !ok || msg.err == nil {
		t.Fatal("unsafe link scheme was not rejected")
	}
}

func TestConversationTUIActivityFocusAndOutputPager(t *testing.T) {
	api := &conversationAPITestDouble{submissions: make(chan protocol.ConversationSubmitParams, 1)}
	model := &conversationTUI{client: api, conversation: "conv-1", ui: testTUIModel(80, 24)}
	model.appendBlock(TranscriptBlock{ID: "activity", TurnID: "turn-1", Kind: BlockToolActivity, Header: "✓ bash", Body: "preview", Collapsed: true, Activity: &tui.Activity{Tool: "bash", OutputRef: "out-1"}})
	updated, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyF6})
	model = updated.(*conversationTUI)
	if !model.ui.TranscriptFocused || model.ui.SelectedActivity != "activity" {
		t.Fatal("F6 did not focus the transcript activity")
	}
	updated, cmd := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = updated.(*conversationTUI)
	if cmd == nil {
		t.Fatal("Enter did not open the selected output pager")
	}
	updated, pageCmd := model.Update(cmd())
	model = updated.(*conversationTUI)
	if model.ui.InspectionRef() != "out-1" || pageCmd == nil {
		t.Fatal("output action was not handled by the runtime adapter")
	}
	if _, ok := pageCmd().(outputPageMsg); !ok {
		t.Fatal("output request did not return a page message")
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	model = updated.(*conversationTUI)
	if model.ui.HasInspection() || !model.ui.TranscriptFocused {
		t.Fatal("Escape did not return to transcript focus")
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyF6})
	model = updated.(*conversationTUI)
	updated, _ = model.Update(tea.KeyPressMsg{Text: " ", Code: ' '})
	model = updated.(*conversationTUI)
	if model.ui.Composer.Editor.Value() != " " {
		t.Fatalf("Space did not remain composer input: %q", model.ui.Composer.Editor.Value())
	}
}

func TestConversationTUIKeepsCompletedOutputAcrossSyncs(t *testing.T) {
	firstPrompt := protocol.ConversationTranscriptEntry{Role: "user", TurnID: "turn-1", Content: "first prompt"}
	answer := protocol.ConversationTranscriptEntry{Role: "assistant", TurnID: "turn-1", Content: "first answer"}
	api := &conversationAPITestDouble{submissions: make(chan protocol.ConversationSubmitParams, 1)}
	m := &conversationTUI{client: api, ui: testTUIModel(80, 24)}
	m.loadSync(&protocol.ConversationSyncResult{ConversationID: "conv-1", WorkerEpoch: "worker-1", Transcript: []protocol.ConversationTranscriptEntry{firstPrompt}})
	activity := map[string]any{"turn_id": "turn-1", "step": float64(1), "tool_call_id": "call-1"}
	m.Update(conversationEventMsg{ConversationID: "conv-1", TurnID: "turn-1", Sequence: 1, Type: string(agent.EventToolCall),
		Data: map[string]any{"tool_name": "bash", "activity_id": activity}})
	m.Update(conversationEventMsg{ConversationID: "conv-1", TurnID: "turn-1", Sequence: 2, Type: string(agent.EventToolResult), Content: "preview",
		Data: map[string]any{"tool_name": "bash", "activity_id": activity, "output_ref": "out-1"}})
	activityID := m.ui.Transcript.Blocks[2].ID
	openOutput := func(stage string, wantBlocks int) {
		t.Helper()
		if len(m.ui.Transcript.Blocks) != wantBlocks || m.ui.Transcript.Blocks[2].ID != activityID || !m.ui.Transcript.Blocks[2].Completed || m.ui.Transcript.Blocks[3].Body != "first answer" {
			t.Fatalf("%s sync did not reconcile the activity with its originating turn: %+v", stage, m.ui.Transcript.Blocks)
		}
		m.Update(tea.KeyPressMsg{Code: tea.KeyF6})
		m.Update(tea.KeyPressMsg{Code: tea.KeyEnter}) // Expand the tool group.
		m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		_, open := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		if m.ui.SelectedActivity != activityID || open == nil {
			t.Fatalf("%s sync lost the selectable output activity", stage)
		}
		updated, page := m.Update(open())
		m = updated.(*conversationTUI)
		if page == nil {
			t.Fatalf("%s output action did not request a page", stage)
		}
		m.Update(page())
		if m.ui.InspectionRef() != "out-1" || m.ui.CurrentOutputPage() == nil || m.ui.CurrentOutputPage().Data != "page" {
			t.Fatalf("%s sync did not preserve the output pager: ref=%q page=%+v", stage, m.ui.InspectionRef(), m.ui.CurrentOutputPage())
		}
		m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
		m.Update(tea.KeyPressMsg{Code: tea.KeyF6})
	}
	api.syncResult = &protocol.ConversationSyncResult{ConversationID: "conv-1", WorkerEpoch: "worker-1", EventWatermark: 3,
		Transcript: []protocol.ConversationTranscriptEntry{firstPrompt, answer}}
	_, sync := m.Update(conversationEventMsg{ConversationID: "conv-1", TurnID: "turn-1", Sequence: 3, Type: string(agent.EventFinished)})
	m.Update(sync())
	openOutput("completion", 4)

	nextPrompt := protocol.ConversationTranscriptEntry{Role: "user", TurnID: "turn-2", Content: "second prompt"}
	api.syncResult = &protocol.ConversationSyncResult{ConversationID: "conv-1", WorkerEpoch: "worker-1", EventWatermark: 4,
		Transcript: []protocol.ConversationTranscriptEntry{firstPrompt, answer, nextPrompt}}
	_, sync = m.Update(m.submit(nextPrompt.Content, persistence.SubmissionQueue)())
	m.Update(sync())
	openOutput("next prompt", 5)
}

func TestConversationTUIDropsOutputWhenConversationOrWorkerChanges(t *testing.T) {
	for _, next := range []protocol.ConversationSyncResult{
		{ConversationID: "conv-1", WorkerEpoch: "worker-2"},
		{ConversationID: "conv-2", WorkerEpoch: "worker-1"},
	} {
		old := &protocol.ConversationSyncResult{ConversationID: "conv-1", WorkerEpoch: "worker-1"}
		m := &conversationTUI{client: &conversationAPITestDouble{syncResult: old}}
		m.loadSync(old)
		m.appendBlock(TranscriptBlock{ID: "activity", Kind: BlockToolActivity, Activity: &tui.Activity{OutputRef: "old-ref"}})
		m.ui.SelectedActivity, m.outputLoading = "activity", true
		setTestOutputInspection(m, "old-ref", nil)
		lateSync := m.syncConversation()
		m.loadSync(&next)
		m.Update(lateSync())
		m.Update(outputPageMsg{generation: m.inspectionGeneration - 1, ref: "old-ref", err: errors.New("stale output error")})
		if m.conversation != next.ConversationID || m.workerEpoch != next.WorkerEpoch || len(m.ui.Transcript.Blocks) != 0 ||
			m.ui.SelectedActivity != "" || m.ui.HasInspection() || m.outputLoading || m.errorText != "" {
			t.Fatalf("old output state leaked after switching to %s/%s", next.ConversationID, next.WorkerEpoch)
		}
	}
}

func TestConversationTUIOutputPagerShowsExactByteRange(t *testing.T) {
	model := &conversationTUI{ui: testTUIModel(80, 24), outputPageStarts: []int64{131072}}
	setTestOutputInspection(model, "out", &tui.OutputPage{Data: "page", Offset: 131072, TotalBytes: 262144})
	if view := model.View().Content; !strings.Contains(view, "Bytes 131073–131076 of 262144") {
		t.Fatalf("pager did not show exact byte range: %q", view)
	}
}

func TestConversationTUIInspectionNoticesFitViewport(t *testing.T) {
	files := make([]protocol.DiffFileEntry, 24)
	for i := range files {
		files[i] = protocol.DiffFileEntry{Path: fmt.Sprintf("file-%02d.txt", i), Diff: strings.Repeat("+changed line\n", 24), Truncated: true}
	}
	files[22].Path = strings.Repeat("long-path/", 12) + "file-22.txt"
	for _, name := range []string{"output", "narrow output", "diff list", "diff file"} {
		t.Run(name, func(t *testing.T) {
			m := &conversationTUI{ui: testTUIModel(80, 24), conversation: "conversation-1"}
			var want []string
			if name == "output" || name == "narrow output" {
				if name == "narrow output" {
					m.ui.Width = 35
					m.errorText = "Could not retrieve a previous output page."
					want = append(want, m.errorText)
				}
				data := strings.Repeat("output line\n", 24)
				setTestOutputInspection(m, "output-1", &tui.OutputPage{Data: data, TotalBytes: int64(len(data)), Truncated: true})
				if m.errorText != "" {
					m.ui.Update(tui.Event{Kind: tui.EventInspectionFailure, InspectionGeneration: m.inspectionGeneration, InspectionError: m.errorText})
				}

				want = append(want, "output line", "Output capped", "End of available output", "Esc returns to activity")
			} else {
				m.ui.Width = 58
				setTestDiffInspection(m, &protocol.DiffInspectResult{
					Files: files, Stats: protocol.DiffStats{FilesChanged: len(files)},
					Warning: "Some Git changes could not be read safely\nCheck the worktree before continuing.", OmittedPaths: 3, Truncated: true,
				})
				want = []string{"could not be read safely", "Check the worktree", "3 paths omitted", "Diff output was capped"}
				if name == "diff file" {
					m.ui.InspectionView(m.ui.Height) // initialize layout before opening the selected file
					_, _ = m.ui.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
					want = append(want, "+changed line", "Esc file list")
				} else {
					m.ui.InspectionView(m.ui.Height)
					for range len(files) - 1 {
						_, _ = m.ui.Update(tea.KeyPressMsg{Code: tea.KeyDown})
					}
					want = append(want, "› file-23.txt", "Esc close")
				}
			}
			view := m.View().Content
			if rows := strings.Count(strings.TrimSuffix(view, "\n"), "\n") + 1; rows > m.ui.Height {
				t.Errorf("inspection uses %d rows for a %d-row terminal", rows, m.ui.Height)
			}
			for line := range strings.SplitSeq(view, "\n") {
				if width := ansi.StringWidth(line); width > m.ui.Width {
					t.Errorf("inspection line width %d exceeds terminal width %d: %q", width, m.ui.Width, line)
				}
			}
			text := strings.Join(strings.Fields(ansi.Strip(view)), " ")
			for _, fragment := range want {
				if !strings.Contains(text, fragment) {
					t.Errorf("inspection lost %q in %q", fragment, view)
				}
			}
		})
	}
}

func TestConversationTUIInspectionPansLongLines(t *testing.T) {
	start := strings.Repeat("界", 5)
	line := strings.Repeat("界", 12) + "TAIL"
	for _, tc := range []struct {
		name string
		open func(*conversationTUI) string
	}{
		{"output", func(m *conversationTUI) string {
			setTestOutputInspection(m, "output-1", &tui.OutputPage{Data: line, TotalBytes: int64(len(line))})
			return m.View().Content
		}},
		{"diff", func(m *conversationTUI) string {
			setTestDiffInspection(m, &protocol.DiffInspectResult{Files: []protocol.DiffFileEntry{{Path: "file.go", Diff: line}}})
			m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			return m.View().Content
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &conversationTUI{ui: testTUIModel(10, 24)}
			if view := tc.open(m); strings.Contains(view, "TAIL") || !strings.Contains(view, "\n"+start+"\n") {
				t.Fatalf("initial viewport did not clip the long line: %q", view)
			}
			for range 6 {
				m.Update(tea.KeyPressMsg{Code: tea.KeyRight})
			}
			if view := m.View().Content; !strings.Contains(view, "\n界界界TAIL\n") {
				t.Fatalf("right pan did not reveal the line ending: %q", view)
			}
			for range 6 {
				m.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
			}
			if view := m.View().Content; !strings.Contains(view, "\n"+start+"\n") {
				t.Fatalf("left pan did not return to the line beginning: %q", view)
			}
		})
	}
}

func TestConversationTUIDiffNavigationAndDismissal(t *testing.T) {
	model := &conversationTUI{ui: testTUIModel(80, 24)}
	setTestDiffInspection(model, &protocol.DiffInspectResult{Files: []protocol.DiffFileEntry{{Path: "one.go"}, {Path: "two.go", Diff: "header\n@@ -1 +1 @@\n-a\n+b\n@@ -5 +5 @@\n-c\n+d"}}})
	updated, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	model = updated.(*conversationTUI)
	if view := model.View().Content; !strings.Contains(view, "› two.go") {
		t.Fatalf("Down did not select next changed file: %q", view)
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = updated.(*conversationTUI)
	if view := model.View().Content; !strings.Contains(view, "@@ -1 +1 @@") {
		t.Fatalf("Enter did not open selected diff: %q", view)
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	model = updated.(*conversationTUI)
	if view := model.View().Content; !strings.Contains(view, "two.go") {
		t.Fatalf("open diff disappeared during navigation: %q", view)
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	model = updated.(*conversationTUI)
	if view := model.View().Content; !strings.Contains(view, "@@ -5 +5 @@") {
		t.Fatalf("PageDown did not navigate to the next hunk: %q", view)
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	model = updated.(*conversationTUI)
	if view := model.View().Content; !strings.Contains(view, "› two.go") {
		t.Fatalf("Escape did not return to file list: %q", view)
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	model = updated.(*conversationTUI)
	if model.ui.HasInspection() {
		t.Fatal("Escape did not close diff viewer")
	}
}

func TestConversationTUIIgnoresDiffNavigationWhileLoading(t *testing.T) {
	model := &conversationTUI{}
	model.inspectDiff()
	for _, key := range []tea.KeyPressMsg{
		{Code: tea.KeyUp}, {Code: 'k', Text: "k"}, {Code: tea.KeyDown}, {Code: 'j', Text: "j"},
		{Code: tea.KeyEnter}, {Code: tea.KeyPgUp}, {Code: tea.KeyPgDown},
	} {
		_, cmd := model.Update(key)
		if cmd != nil || !strings.Contains(model.View().Content, "Inspecting Git changes") || model.ui.Composer.Editor.Value() != "" {
			t.Fatalf("%q changed the UI while the diff was loading", key.String())
		}
	}
	generation := model.inspectionGeneration
	model.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	model.Update(diffInspectMsg{generation: generation, result: &protocol.DiffInspectResult{}})
	if model.ui.HasInspection() {
		t.Fatal("Escape did not dismiss the pending diff")
	}
}

func TestConversationTUIDiffPageUpAfterPageDownAtEnd(t *testing.T) {
	model := &conversationTUI{ui: testTUIModel(80, 24)}
	setTestDiffInspection(model, &protocol.DiffInspectResult{Files: []protocol.DiffFileEntry{{Path: "tiny.go", Diff: "@@ -1 +1 @@\n-a\n+b"}}})
	model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
	pageDownView := model.View().Content
	model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	downView := model.View().Content
	model.Update(tea.KeyPressMsg{Code: tea.KeyPgUp})
	if pageDownView == "" || downView == "" || model.View().Content == "" {
		t.Fatal("diff navigation produced an empty view")
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
	model := &conversationTUI{ui: testTUIModel(80, 24)}
	setTestDiffInspection(model, &protocol.DiffInspectResult{Files: []protocol.DiffFileEntry{{Path: malicious, Diff: malicious}}})
	view := model.View().Content
	if strings.Contains(view, "\x1b]52") || strings.Contains(view, "\x1b[2J") {
		t.Fatalf("diff view retained terminal control sequences: %q", view)
	}
}

func TestConversationTUIGroupsSubagentActivities(t *testing.T) {
	model := &conversationTUI{ui: testTUIModel(80, 24)}
	model.ensureUIModel()
	parent := TranscriptBlock{ID: "parent", Kind: BlockToolActivity, Header: "✓ invoke_subagent", Collapsed: true, Activity: &tui.Activity{Tool: "invoke_subagent", AgentName: "worker"}}
	child := TranscriptBlock{ID: "child", ParentID: "parent", Kind: BlockSubagentActivity, Header: "✓ read_file", Body: "file contents", Collapsed: true, Completed: true, Activity: &tui.Activity{Tool: "read_file", OutputRef: "child-output"}}
	model.ui.Transcript.Blocks = []TranscriptBlock{parent, child}
	collapsed, _ := model.renderBlock(parent, 80)
	if !strings.Contains(collapsed, "[Subagent: worker (1 step completed)]") || strings.Contains(collapsed, "read_file") {
		t.Fatalf("collapsed parent did not summarize child activity: %q", collapsed)
	}
	parent.Collapsed = false
	expanded, _ := model.renderBlock(parent, 80)
	if !strings.Contains(expanded, "read_file") || strings.Contains(strings.Join(model.computeVisibleLines(), "\n"), "file contents") {
		t.Fatalf("expanded grouping did not nest child activity: %q", expanded)
	}
	model.ui.TranscriptFocused = true
	model.ui.Transcript.Blocks[0].Collapsed = false
	model.ui.SelectedActivity = "parent"
	model.ui.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	if model.ui.SelectedActivity != "child" {
		t.Fatalf("expanded child was not selectable: %q", model.ui.SelectedActivity)
	}
	updated, cmd := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = updated.(*conversationTUI)
	if cmd == nil {
		t.Fatal("selected child output did not open its pager")
	}
	updated, _ = model.Update(cmd())
	model = updated.(*conversationTUI)
	if model.ui.InspectionRef() != "child-output" {
		t.Fatal("selected child output action was not applied")
	}
}

func TestConversationTUISelectionKeepsExpandedChildVisible(t *testing.T) {
	m := &conversationTUI{ui: testTUIModel(40, 21)}
	m.ui.TranscriptFocused, m.ui.SelectedActivity = true, "parent"
	m.appendBlock(TranscriptBlock{ID: "parent", Kind: BlockToolActivity, Header: "subagent"})
	for i := 1; i <= 20; i++ {
		name := fmt.Sprintf("child-%02d", i)
		m.appendBlock(TranscriptBlock{
			ID: name, ParentID: "parent", Kind: BlockSubagentActivity, Header: name,
			Body: strings.Repeat("wrapped output ", 10) + "\nsecond body line",
		})
	}
	assertVisible := func(step int) {
		t.Helper()
		want := fmt.Sprintf("child-%02d", step)
		view := strings.Join(m.computeVisibleLines(), "\n")
		if m.ui.SelectedActivity != want || !strings.Contains(view, "↳ "+want) {
			t.Fatalf("selected child %q should be visible after navigation, selected=%q view=%q", want, m.ui.SelectedActivity, view)
		}
	}
	for i := 1; i <= 20; i++ {
		m.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		assertVisible(i)
	}
	for i := 19; i >= 1; i-- {
		m.Update(tea.KeyPressMsg{Code: tea.KeyUp})
		assertVisible(i)
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
	if len(model.ui.Transcript.Blocks) != 1 || model.ui.Transcript.Blocks[0].Kind != BlockSubagentActivity || !strings.HasPrefix(model.ui.Transcript.Blocks[0].Header, "Detached child · ") {
		t.Fatalf("result-only child was not rendered as detached: %+v", model.ui.Transcript.Blocks)
	}
}

func TestConversationTUINestedActivityRefreshesCachedParent(t *testing.T) {
	m := &conversationTUI{conversation: "conv-1", ui: testTUIModel(80, 24)}
	identity := func(call string) map[string]any {
		return map[string]any{"turn_id": "turn-1", "step": float64(1), "tool_call_id": call}
	}
	emit := func(eventType agent.EventType, tool, call, content string, parent map[string]any) {
		t.Helper()
		data := map[string]any{"tool_name": tool, "activity_id": identity(call)}
		if parent != nil {
			data["parent_activity_id"] = parent
		}
		m.Update(conversationEventMsg(protocol.SessionEventNotification{
			ConversationID: "conv-1", TurnID: "turn-1", Type: string(eventType), Content: content, Data: data,
		}))
	}
	emit(agent.EventToolCall, "invoke_subagent", "parent", "", nil)
	m.ui.Transcript.Update(0, func(block *TranscriptBlock) { block.Collapsed = false }, m.ui.Width)
	_ = m.View()
	emit(agent.EventToolCall, "read_file", "child", "", identity("parent"))
	if view := m.View().Content; !strings.Contains(view, "worker (1 step)") {
		t.Fatalf("cached parent did not show the new child: %q", view)
	}
	emit(agent.EventToolResult, "read_file", "child", "read complete", identity("parent"))
	if view := m.View().Content; !strings.Contains(view, "worker (1 step completed)") {
		t.Fatalf("cached parent did not show child completion: %q", view)
	}
	emit(agent.EventToolResult, "read_file", "result-only", "second read", identity("parent"))
	if view := m.View().Content; !strings.Contains(view, "worker (2 steps completed)") {
		t.Fatalf("cached parent did not show the result-only child: %q", view)
	}
}

func TestConversationTUILongBlockClipping(t *testing.T) {
	model := &conversationTUI{

		ui: testTUIModel(80, 24),
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
	model.ui.Transcript.ScrollOffset = 0
	linesBottom := model.computeVisibleLines()
	if len(linesBottom) >= 100 {
		t.Fatalf("visible lines should be clipped to viewport height, got %d", len(linesBottom))
	}
	bottomText := ansi.Strip(strings.Join(linesBottom, "\n"))
	if !strings.Contains(bottomText, "Response line 100") {
		t.Fatalf("bottom view missing last response line: %s", bottomText)
	}
	if strings.Contains(bottomText, "Response line 1\n") {
		t.Fatalf("bottom view should not contain first response line: %s", bottomText)
	}

	// Scroll up
	model.ui.Transcript.ScrollOffset = 1000 // clamped to top
	linesTop := model.computeVisibleLines()
	topText := ansi.Strip(strings.Join(linesTop, "\n"))
	if !strings.Contains(topText, "Response line 1") {
		t.Fatalf("top view missing first response line: %s", topText)
	}
	if strings.Contains(topText, "Response line 100") {
		t.Fatalf("top view should not contain last response line: %s", topText)
	}
}

func TestConversationTUIClippedTranscriptSkipsNestedChildRows(t *testing.T) {
	m := &conversationTUI{ui: testTUIModel(80, 24)}
	m.appendBlock(TranscriptBlock{Kind: BlockAssistantMessage, Body: strings.Repeat("older response\n", m.viewportHeight())})
	m.appendBlock(TranscriptBlock{ID: "parent", Kind: BlockToolActivity, Header: "subagent", Collapsed: true})
	for range 10 {
		m.appendBlock(TranscriptBlock{ParentID: "parent", Kind: BlockSubagentActivity, Header: "child activity", Completed: true})
	}
	m.appendBlock(TranscriptBlock{Kind: BlockAssistantMessage, Body: "latest response"})
	lines := m.computeVisibleLines()
	if len(lines) > m.viewportHeight() {
		t.Fatalf("clipped transcript has %d rows, viewport allows %d", len(lines), m.viewportHeight())
	}
	view := ansi.Strip(strings.Join(lines, "\n"))
	if !strings.Contains(view, "subagent (10 steps completed)") || !strings.Contains(view, "latest response") {
		t.Fatalf("clipped transcript lost visible activity: %q", view)
	}
}

func TestConversationTUIScrollsRenderedMarkdownRows(t *testing.T) {
	for _, width := range []int{48, 80} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m := &conversationTUI{ui: testTUIModel(width, 16)}
			m.appendBlock(TranscriptBlock{Kind: BlockUserMessage, Body: "Explain the result"})
			m.appendBlock(TranscriptBlock{Kind: BlockAssistantMessage, Body: "# First heading\n\n" +
				strings.Repeat("This paragraph wraps across several terminal lines. ", 4) + "\n\n## Code\n\n" +
				"```go\nfunc example() {\n\tfmt.Println(\"hello\")\n}\n```"})
			m.appendBlock(TranscriptBlock{Kind: BlockAssistantMessage, Body: "## Results\n\n" +
				"| Name | Result |\n| --- | --- |\n| First | Passed |\n| Second | Skipped |\n\nA closing paragraph."})
			m.appendBlock(TranscriptBlock{Kind: BlockUserMessage, Body: "What comes next?"})

			var transcript []string
			for _, block := range m.ui.Transcript.Blocks {
				rendered, _ := m.renderBlock(block, width)
				transcript = append(transcript, tui.WrapLines(rendered, width)...)
				transcript = append(transcript, "")
			}
			height := m.viewportHeight()
			maxScroll := len(transcript) - height
			for offset := 0; offset <= maxScroll+1; offset++ {
				m.ui.Transcript.ScrollOffset = offset
				bottom := len(transcript) - min(offset, maxScroll)
				want := transcript[bottom-height : bottom]
				if got := m.computeVisibleLines(); !slices.Equal(got, want) {
					t.Fatalf("offset %d\n got: %q\nwant: %q", offset, got, want)
				}
			}
		})
	}
}

func TestConversationTUIMouseWheelScrollsTranscript(t *testing.T) {
	model := &conversationTUI{ui: testTUIModel(80, 24)}
	updated, _ := model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelUp})
	model = updated.(*conversationTUI)
	if model.ui.Transcript.ScrollOffset != 3 {
		t.Fatalf("wheel up scroll offset = %d, want 3", model.ui.Transcript.ScrollOffset)
	}
	updated, _ = model.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	model = updated.(*conversationTUI)
	if model.ui.Transcript.ScrollOffset != 0 {
		t.Fatalf("wheel down scroll offset = %d, want 0", model.ui.Transcript.ScrollOffset)
	}
}

func TestConversationTUIKeepsHistoryAnchorWhileAppending(t *testing.T) {
	model := &conversationTUI{ui: testTUIModel(80, 24)}
	for i := range 20 {
		model.appendBlock(TranscriptBlock{ID: fmt.Sprintf("message-%d", i), Kind: BlockAssistantMessage, Body: fmt.Sprintf("Message %d", i)})
	}
	model.ui.Transcript.ScrollOffset = 8
	before := strings.Join(model.computeVisibleLines(), "\n")
	model.appendBlock(TranscriptBlock{ID: "new-message", Kind: BlockAssistantMessage, Body: "New incoming response"})
	after := strings.Join(model.computeVisibleLines(), "\n")
	if after != before {
		t.Fatalf("new transcript content moved the inspected history (offset %d)\nbefore: %q\nafter:  %q", model.ui.Transcript.ScrollOffset, before, after)
	}
	added := len(model.getWrappedLines(model.ui.Transcript.Blocks[len(model.ui.Transcript.Blocks)-1], model.ui.Width)) + 1
	if model.ui.Transcript.ScrollOffset != 8+added {
		t.Fatalf("scroll offset after adding %d rows = %d, want %d", added, model.ui.Transcript.ScrollOffset, 8+added)
	}
}

func TestConversationTUIKeepsHistoryAnchorDuringSync(t *testing.T) {
	m := &conversationTUI{ui: testTUIModel(80, 24)}
	snapshot := &protocol.ConversationSyncResult{ConversationID: "conv-1", WorkerEpoch: "worker-1"}
	for i := range 10 {
		turnID := fmt.Sprintf("turn-%d", i)
		snapshot.Transcript = append(snapshot.Transcript,
			protocol.ConversationTranscriptEntry{TurnID: turnID, Role: "user", Content: fmt.Sprintf("Prompt %d", i)},
			protocol.ConversationTranscriptEntry{TurnID: turnID, Role: "assistant", Content: fmt.Sprintf("## Response %d\n\nAn explanation.", i)},
		)
	}
	m.loadSync(snapshot)
	m.ui.Transcript.ScrollOffset = 8
	before := m.computeVisibleLines()
	m.loadSync(snapshot)
	after := m.computeVisibleLines()
	if m.ui.Transcript.ScrollOffset != 8 || !slices.Equal(after, before) {
		t.Fatalf("snapshot moved the viewport (offset %d, want 8)\nbefore: %q\n after: %q", m.ui.Transcript.ScrollOffset, before, after)
	}
}

func TestConversationTUIKeepsHistoryAnchorWhileStreaming(t *testing.T) {
	model := &conversationTUI{ui: testTUIModel(80, 24)}
	for i := range 20 {
		model.appendBlock(TranscriptBlock{ID: fmt.Sprintf("message-%d", i), Kind: BlockAssistantMessage, Body: fmt.Sprintf("Message %d", i), Streaming: i == 19})
	}
	model.ui.Transcript.ScrollOffset = 8
	before := strings.Join(model.computeVisibleLines(), "\n")
	model.clearCache()
	model.Update(conversationEventMsg{ConversationID: "conversation-1", TurnID: "turn-1", Type: string(agent.EventTextDelta), Content: strings.Repeat(" streaming content", 30)})
	model.Update(tui.FlushMsg{})
	after := strings.Join(model.computeVisibleLines(), "\n")
	if after != before {
		t.Fatalf("stream growth moved the inspected history (offset %d)\nbefore: %q\nafter:  %q", model.ui.Transcript.ScrollOffset, before, after)
	}
}

func TestConversationTUIFormatsVisibleTranscript(t *testing.T) {
	model := &conversationTUI{ui: testTUIModel(80, 24)}
	for i := range 100 {
		model.appendBlock(TranscriptBlock{ID: fmt.Sprintf("message-%d", i), Kind: BlockAssistantMessage, Body: fmt.Sprintf("Message %d", i)})
	}
	lines := model.computeVisibleLines()
	if len(lines) == 0 || strings.Contains(strings.Join(lines, "\n"), "Message 0") {
		t.Fatalf("expected only the visible tail of the transcript, got %q", lines)
	}
}

func TestConversationTUIUnicodeWidth(t *testing.T) {
	model := &conversationTUI{ui: testTUIModel(80, 24)}
	model.ui.Width = 12
	block := TranscriptBlock{ID: "unicode", Kind: BlockAssistantMessage, Role: "assistant", Header: "Buddy", Body: "界面 🐕 Buddy"}
	rendered, _ := model.renderBlock(block, model.ui.Width)
	for _, line := range tui.WrapLines(rendered, model.ui.Width) {
		if width := ansi.StringWidth(line); width > model.ui.Width {
			t.Fatalf("unicode render width = %d, max %d: %q", width, model.ui.Width, line)
		}
	}
}

func BenchmarkConversationTUIRender10k(b *testing.B) {
	model := &conversationTUI{ui: testTUIModel(12, 24)}
	for i := range 10000 {
		model.appendBlock(TranscriptBlock{ID: fmt.Sprintf("message-%d", i), Kind: BlockAssistantMessage, Body: fmt.Sprintf("Message %d with a short readable response.", i)})
	}
	_ = model.computeVisibleLines()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = model.computeVisibleLines()
	}
}

func TestConversationTUIQuitCommand(t *testing.T) {
	for _, cmdStr := range []string{"/quit", "/exit"} {
		model := &conversationTUI{

			ui: testTUIModel(80, 24),
		}
		model.ui.Composer.Editor.SetValue(cmdStr)
		_, cmd := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		if cmd == nil {
			t.Fatalf("expected tea.Quit command on %s, got nil", cmdStr)
		}
		_, quitCmd := model.Update(cmd())
		if quitCmd == nil {
			t.Fatalf("expected tea.Quit after dispatching %s", cmdStr)
		}
		if _, ok := quitCmd().(tea.QuitMsg); !ok {
			t.Fatalf("expected QuitMsg on %s", cmdStr)
		}
	}
}

func TestConversationTUIClearCommand(t *testing.T) {
	model := &conversationTUI{

		ui: testTUIModel(80, 24),
	}
	model.ui.Transcript.ScrollOffset = 5
	model.appendBlock(TranscriptBlock{
		ID:        "block-1",
		Kind:      BlockAssistantMessage,
		Role:      "assistant",
		Header:    "Buddy",
		Body:      "Some text",
		CreatedAt: time.Now(),
	})
	model.ui.Composer.Editor.SetValue("/clear")
	updated, actionCmd := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if actionCmd == nil {
		t.Fatal("expected a submitted prompt action")
	}
	updated, _ = updated.(*conversationTUI).Update(actionCmd())
	cleared := updated.(*conversationTUI)
	if len(cleared.ui.Transcript.Blocks) != 0 {
		t.Fatalf("expected 0 blocks after /clear, got %d", len(cleared.ui.Transcript.Blocks))
	}
	if cleared.ui.Transcript.ScrollOffset != 0 {
		t.Fatalf("expected scrollOffset 0 after /clear, got %d", cleared.ui.Transcript.ScrollOffset)
	}
}

func TestConversationTUIClearKeepsStreamingBlock(t *testing.T) {
	model := &conversationTUI{
		ui:           testTUIModel(80, 24),
		conversation: "conv-1",
		state:        "running",
	}
	model.appendBlock(TranscriptBlock{ID: "old", Body: "old"})
	model.Update(conversationEventMsg{ConversationID: "conv-1", TurnID: "turn-1", Type: string(agent.EventTextDelta), Content: "partial"})
	model.ui.Composer.Editor.SetValue("/clear")
	updated, actionCmd := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if actionCmd == nil {
		t.Fatal("expected a submitted prompt action")
	}
	updated, _ = updated.(*conversationTUI).Update(actionCmd())
	cleared := updated.(*conversationTUI)
	if len(cleared.ui.Transcript.Blocks) != 1 || cleared.ui.Transcript.Blocks[0].ID != cleared.ui.ActiveBlockID() || !cleared.ui.Transcript.Blocks[0].Streaming {
		t.Fatalf("clear lost active streaming block: blocks=%+v active=%q", cleared.ui.Transcript.Blocks, cleared.ui.ActiveBlockID())
	}
}

func TestConversationTUIComposerCompletions(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "notes.md"), []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	model := &conversationTUI{workspace: workspace, ui: testTUIModel(80, 30)}
	model.ui.Composer.Commands.Register(tui.SlashCommand{Name: "/clear", Description: "Clear transcript"})
	model.ui.Composer.Commands.Register(tui.SlashCommand{Name: "/copy", Description: "Copy transcript"})
	model.ui.Composer.Editor.SetValue("/cl")
	model.ui.Composer.UpdateCompletion(model.workspace)
	if model.ui.Composer.CompletionKind() != "command" || len(model.ui.Composer.CompletionCommands()) != 1 {
		t.Fatalf("slash completion = %q, %+v", model.ui.Composer.CompletionKind(), model.ui.Composer.CompletionCommands())
	}
	updated, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	model = updated.(*conversationTUI)
	if got := model.ui.Composer.Editor.Value(); got != "/clear" {
		t.Fatalf("slash completion inserted %q", got)
	}
	model.ui.Composer.Editor.SetValue("/co")
	model.ui.Composer.UpdateCompletion(model.workspace)
	shortenedViewport := model.viewportHeight()
	if model.completionRows() == 0 {
		t.Fatal("active completion popup did not reserve viewport rows")
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	model = updated.(*conversationTUI)
	if model.ui.Composer.CompletionKind() != "" || model.ui.Composer.Editor.Value() != "/co" {
		t.Fatalf("escape changed composer draft: popup=%q draft=%q", model.ui.Composer.CompletionKind(), model.ui.Composer.Editor.Value())
	}
	if model.completionRows() != 0 || model.viewportHeight() <= shortenedViewport {
		t.Fatalf("dismissed popup still reserves transcript rows: rows=%d viewport=%d", model.completionRows(), model.viewportHeight())
	}
	model.ui.Composer.Editor.SetValue("@not")
	model.ui.Composer.UpdateCompletion(model.workspace)
	if model.ui.Composer.CompletionKind() != "file" || len(model.ui.Composer.CompletionFiles()) != 1 || model.ui.Composer.CompletionFiles()[0] != "notes.md" {
		t.Fatalf("file completion = %q, %+v", model.ui.Composer.CompletionKind(), model.ui.Composer.CompletionFiles())
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	model = updated.(*conversationTUI)
	if got := model.ui.Composer.Editor.Value(); got != "notes.md" {
		t.Fatalf("file completion inserted %q", got)
	}
}

func TestConversationTUILoadSyncSkipsEmptyAssistantToolSteps(t *testing.T) {
	model := &conversationTUI{ui: testTUIModel(80, 24)}
	model.loadSync(&protocol.ConversationSyncResult{ConversationID: "conversation-1", Transcript: []protocol.ConversationTranscriptEntry{
		{Role: "user", Content: "inspect files"},
		{Role: "assistant", Content: ""},
		{Role: "assistant", Content: " \n  "},
		{Role: "assistant", Content: "Found one file."},
	}})
	if len(model.ui.Transcript.Blocks) != 2 {
		t.Fatalf("restored visible block count = %d, want 2: %+v", len(model.ui.Transcript.Blocks), model.ui.Transcript.Blocks)
	}
	lines := ansi.Strip(strings.Join(model.computeVisibleLines(), "\n"))
	if strings.Contains(lines, "Buddy") || !strings.Contains(lines, "› inspect files") || !strings.Contains(lines, "Found one file.") {
		t.Fatalf("restored transcript contains empty assistant rows: %q", lines)
	}
}

func TestConversationTUIEmptyAssistantRendering(t *testing.T) {
	model := &conversationTUI{ui: testTUIModel(80, 24)}
	if got, _ := model.renderBlock(TranscriptBlock{Kind: BlockAssistantMessage, Role: "assistant"}, 80); got != "" {
		t.Fatalf("empty completed assistant rendered %q", got)
	}
	got, _ := model.renderBlock(TranscriptBlock{Kind: BlockAssistantMessage, Role: "assistant", Streaming: true}, 80)
	if !strings.Contains(got, "Working…") {
		t.Fatalf("empty streaming assistant rendered %q", got)
	}
	model.state = "running"
	if got := model.View().Content; !strings.Contains(got, "Working…") {
		t.Fatalf("empty running conversation rendered %q", got)
	}
}

func TestConversationTUIKeepsStreamingWhileApprovalCardIsOpen(t *testing.T) {
	model := &conversationTUI{
		conversation: "conversation-1", state: "running", ui: testTUIModel(80, 24),
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
	if !model.hasPendingApproval() || !model.ui.HasPendingApproval() || model.ui.ActiveBlockID() == "" {
		t.Fatalf("approval card blocked streaming: pending=%t activeBlock=%q", model.hasPendingApproval(), model.ui.ActiveBlockID())
	}
}

func TestConversationTUIApprovalTakesPrecedenceOverInspection(t *testing.T) {
	request := protocol.PermissionRequestNotification{
		ConversationID: "conversation-1", TurnID: "turn-1", RequestID: "request-1", Tool: "bash",
		Arguments: map[string]any{"command": "echo approval details"},
	}
	for _, overlay := range []string{"diff", "loading diff", "output"} {
		t.Run(overlay, func(t *testing.T) {
			m := &conversationTUI{conversation: request.ConversationID, ui: testTUIModel(80, 24)}
			switch overlay {
			case "diff":
				setTestDiffInspection(m, &protocol.DiffInspectResult{})
			case "loading diff":
				m.inspectDiff()
			case "output":
				setTestOutputInspection(m, "output-1", nil)
			}
			if view := m.View().Content; !strings.Contains(view, "Working tree diff") && !strings.Contains(view, "Tool output") {
				t.Fatalf("expected inspection before approval: %q", view)
			}
			m.Update(permissionRequestMsg(request))
			if overlay == "loading diff" {
				m.Update(diffInspectMsg{generation: m.inspectionGeneration, result: &protocol.DiffInspectResult{}})
			}
			if view := m.View().Content; !strings.Contains(view, "bash · turn-1") || !strings.Contains(view, "echo approval details") ||
				strings.Contains(view, "Working tree diff") || strings.Contains(view, "Tool output") {
				t.Fatalf("approval request did not take precedence: %q", view)
			}
			if _, cmd := m.Update(tea.KeyPressMsg{Code: '1', Text: "1"}); cmd == nil {
				t.Fatal("visible approval did not receive the allow shortcut")
			}
		})
	}
}

func TestConversationTUIApprovalActionBindsActiveRequestAndRejectsStaleChoice(t *testing.T) {
	api := &conversationAPITestDouble{}
	model := &conversationTUI{client: api, conversation: "conversation-1", ui: testTUIModel(80, 24)}
	first := protocol.PermissionRequestNotification{
		RequestID: "request-1", ConversationID: "conversation-1", TurnID: "turn-1", Tool: "bash",
		Arguments: map[string]any{"command": "git status"},
	}
	second := protocol.PermissionRequestNotification{
		RequestID: "request-2", ConversationID: "conversation-1", TurnID: "turn-2", Tool: "write_file",
		Arguments: map[string]any{"path": "main.go"},
	}
	model.setPendingApprovals([]protocol.PermissionRequestNotification{first, second})
	firstGeneration, secondGeneration := model.pendingApprovals[0].generation, model.pendingApprovals[1].generation
	if _, cmd := model.Update(tui.ActionMsg{Action: tui.Action{
		Kind: tui.ActionPermissionChoice, ApprovalGeneration: secondGeneration,
		ApprovalChoice: tui.ApprovalChoice{Decision: tui.ApprovalAllowOnce},
	}}); cmd == nil {
		t.Fatal("stale choice did not schedule a snapshot sync")
	}
	if len(api.permissionResponses) != 0 {
		t.Fatalf("stale choice reached permission RPC: %+v", api.permissionResponses)
	}
	_, cmd := model.Update(tui.ActionMsg{Action: tui.Action{
		Kind: tui.ActionPermissionChoice, ApprovalGeneration: firstGeneration,
		ApprovalChoice: tui.ApprovalChoice{Decision: tui.ApprovalAllowSession},
	}})
	if cmd == nil {
		t.Fatal("active approval did not schedule its response")
	}
	message := cmd()
	updated, syncCmd := model.Update(message)
	model = updated.(*conversationTUI)
	if syncCmd == nil || len(api.permissionResponses) != 1 {
		t.Fatalf("approval response did not resolve and sync: cmd=%v responses=%+v", syncCmd, api.permissionResponses)
	}
	response := api.permissionResponses[0]
	if response.RequestID != first.RequestID || response.ConversationID != first.ConversationID || response.TurnID != first.TurnID ||
		response.Decision != protocol.DecisionAllowSession || response.CommandPattern != "git status" {
		t.Fatalf("approval action was not bound to active request: %+v", response)
	}
	if len(model.pendingApprovals) != 1 || model.activeApproval().request.RequestID != second.RequestID {
		t.Fatalf("resolving first approval lost or reordered the next request: %+v", model.pendingApprovals)
	}
}

func TestConversationTUIApprovalReplacementSnapshotDropsOldGeneration(t *testing.T) {
	model := &conversationTUI{conversation: "conversation-1", ui: testTUIModel(80, 24)}
	first := protocol.PermissionRequestNotification{
		RequestID: "reused-request", ConversationID: "conversation-1", TurnID: "turn-1", Tool: "write_file",
		Arguments: map[string]any{"path": "first.go"},
	}
	replacement := protocol.PermissionRequestNotification{
		RequestID: "reused-request", ConversationID: "conversation-1", TurnID: "turn-2", Tool: "write_file",
		Arguments: map[string]any{"path": "second.go"},
	}
	model.addPendingApproval(first)
	oldGeneration := model.pendingApprovals[0].generation
	model.addPendingApproval(replacement)
	if len(model.pendingApprovals) != 1 || len(model.ui.Approvals) != 1 ||
		model.pendingApprovals[0].generation == oldGeneration || model.ui.ActiveApproval().Generation != model.pendingApprovals[0].generation ||
		model.ui.ActiveApproval().TurnID != replacement.TurnID {
		t.Fatalf("replacement left stale approval in the UI: pending=%+v ui=%+v", model.pendingApprovals, model.ui.Approvals)
	}
}

func TestConversationTUIPickerSelectionMapsUIIDsToProtocolResults(t *testing.T) {
	api := &conversationAPITestDouble{syncResult: &protocol.ConversationSyncResult{ConversationID: "second"}, conversationResult: &protocol.ConversationListResult{Conversations: []protocol.ConversationSummary{
		{ConversationID: "first", Task: "first task", DBPath: "/db/first.sqlite"},
		{ConversationID: "second", Task: "second task", DBPath: "/db/second.sqlite"},
	}}}
	model := &conversationTUI{client: api, ui: testTUIModel(80, 24), pickerGeneration: 1, workspace: "/old", databasePath: "/old.db", startParams: protocol.ConversationStartParams{WorkspaceDir: "/old"}}
	workspaces := []protocol.ConversationWorkspace{{WorkspaceDir: "/one"}, {WorkspaceDir: "/two"}}
	updated, _ := model.Update(conversationResumeListMsg{generation: 1, workspaces: workspaces})
	model = updated.(*conversationTUI)
	model.Update(tea.KeyPressMsg{Text: "down", Code: tea.KeyDown})
	_, actionCmd := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	updated, listCmd := model.Update(actionCmd())
	model = updated.(*conversationTUI)
	if listCmd == nil {
		t.Fatal("workspace selection did not request the conversation list")
	}
	updated, _ = model.Update(listCmd())
	model = updated.(*conversationTUI)
	if len(api.conversationLists) != 1 || api.conversationLists[0].WorkspaceDir != "/two" || !model.ui.HasPicker() || model.ui.Picker != tui.PickerConversation {
		t.Fatalf("workspace selection was not mapped to the requested list: calls=%+v picker=%+v", api.conversationLists, model.ui)
	}
	model.Update(tea.KeyPressMsg{Text: "down", Code: tea.KeyDown})
	_, actionCmd = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	_, resumeCmd := model.Update(actionCmd())
	if resumeCmd == nil {
		t.Fatal("conversation selection did not start resume")
	}
	resumeMsg := resumeCmd()
	if model.workspace != "/old" || model.databasePath != "/old.db" {
		t.Fatalf("workspace changed before resume succeeded: workspace=%q db=%q", model.workspace, model.databasePath)
	}
	if len(api.resumeRequests) != 1 || api.resumeRequests[0].WorkspaceDir != "/two" || api.resumeRequests[0].ConversationID != "second" || api.resumeRequests[0].DBPath != "/db/second.sqlite" {
		t.Fatalf("conversation ID did not map to its protocol result: %+v (msg=%T)", api.resumeRequests, resumeMsg)
	}
	updated, _ = model.Update(resumeMsg)
	model = updated.(*conversationTUI)
	if model.workspace != "/two" || model.databasePath != "/db/second.sqlite" || model.conversation != "second" {
		t.Fatalf("successful resume did not commit selected workspace and database: workspace=%q db=%q conversation=%q", model.workspace, model.databasePath, model.conversation)
	}
}

func TestConversationTUIPickerIgnoresCanceledAndStaleResults(t *testing.T) {
	model := &conversationTUI{
		client: &conversationAPITestDouble{}, ui: tui.NewModel(tui.Header{}), pickerGeneration: 4,
		workspaces: []protocol.ConversationWorkspace{{WorkspaceDir: "/workspace"}},
	}
	choice := tui.Choice{ID: "/workspace", Label: "/workspace"}
	model.ui.Update(tui.Event{Kind: tui.EventPickerSnapshot, PickerKind: tui.PickerWorkspace, PickerGeneration: 4, Choices: []tui.Choice{choice}})
	_, selectCmd := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	updated, listCmd := model.Update(selectCmd())
	model = updated.(*conversationTUI)
	if listCmd == nil || model.pickerGeneration != 5 {
		t.Fatal("workspace selection did not start a generation-bound conversation list")
	}
	_, cancelCmd := model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	if cancelCmd == nil {
		t.Fatal("Escape did not emit picker cancellation")
	}
	updated, _ = model.Update(cancelCmd())
	model = updated.(*conversationTUI)
	if model.pickerGeneration != 6 || model.ui.HasPicker() {
		t.Fatalf("Escape did not invalidate pending list request: generation=%d picker=%d", model.pickerGeneration, model.ui.Picker)
	}
	updated, _ = model.Update(listCmd())
	model = updated.(*conversationTUI)
	if model.ui.HasPicker() {
		t.Fatalf("canceled conversation list reopened the picker: %+v", model.ui)
	}
	model.pickerGeneration = 8
	updated, _ = model.Update(conversationResumeListMsg{generation: 7, workspaces: []protocol.ConversationWorkspace{{WorkspaceDir: "/stale"}}})
	model = updated.(*conversationTUI)
	if model.ui.HasPicker() {
		t.Fatal("stale workspace-list response opened a picker")
	}
	model.conversation, model.workspace, model.databasePath, model.resumeGeneration, model.workerEpoch = "current", "/current", "/current.db", 4, "epoch-2"
	model.ui.Update(tui.Event{Kind: tui.EventPickerSnapshot, PickerKind: tui.PickerConversation, PickerGeneration: 8, Choices: []tui.Choice{{ID: "stale", Label: "stale"}}})
	model.ui.Update(tui.Event{Kind: tui.EventPickerGeneration, PickerGeneration: 9})
	model.pickerGeneration = 9
	_, cancelCmd = model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
	updated, _ = model.Update(cancelCmd())
	model = updated.(*conversationTUI)
	updated, _ = model.Update(conversationResumedMsg{pickerGeneration: 9, resumeGeneration: 4, workspace: "/stale", databasePath: "/stale.db", result: &protocol.ConversationSyncResult{ConversationID: "stale"}})
	model = updated.(*conversationTUI)
	if model.conversation != "current" || model.workspace != "/current" || model.databasePath != "/current.db" || model.ui.HasPicker() {
		t.Fatalf("canceled resume replaced current state: conversation=%q workspace=%q db=%q picker=%d", model.conversation, model.workspace, model.databasePath, model.ui.Picker)
	}
	updated, _ = model.Update(conversationResumedMsg{resumeGeneration: 3, sourceConversationID: "current", sourceWorkerEpoch: "epoch-1", workspace: "/current", result: &protocol.ConversationSyncResult{ConversationID: "current"}})
	model = updated.(*conversationTUI)
	if model.conversation != "current" {
		t.Fatalf("stale eviction recovery replaced newer state: %q", model.conversation)
	}
}

func TestConversationTUIRoutesPasteToApprovalEditor(t *testing.T) {
	request := protocol.PermissionRequestNotification{
		RequestID: "request-1", ConversationID: "conversation-1", TurnID: "turn-1",
		Tool: "write_file", Arguments: map[string]any{"path": "old.go"},
	}
	model := &conversationTUI{
		conversation: "conversation-1",
		ui:           testTUIModel(80, 24),
	}
	model.setPendingApprovals([]protocol.PermissionRequestNotification{request})
	model.ui.Composer.Editor.SetValue("draft")
	updated, _ := model.Update(tea.KeyPressMsg{Text: "4", Code: '4'})
	model = updated.(*conversationTUI)
	updated, _ = model.Update(tea.PasteMsg{Content: `{"path":"pasted.go"}`})
	model = updated.(*conversationTUI)
	if model.ui.Composer.Editor.Value() != "draft" {
		t.Fatalf("paste changed main composer: %q", model.ui.Composer.Editor.Value())
	}
	if !model.ui.ApprovalEditing() {
		t.Fatal("approval paste ended editing mode")
	}
	updated, _ = model.Update(conversationEventMsg{
		ConversationID: "conversation-1", TurnID: "turn-1", Type: string(agent.EventTextDelta), Content: "stream continues",
	})
	model = updated.(*conversationTUI)
	if !model.ui.HasBufferedDelta() {
		t.Fatalf("editing approval blocked conversation events: %q", model.ui.StreamingText())
	}
}

func TestConversationTUICompletionStateSurvivesNonEditingMessages(t *testing.T) {
	model := &conversationTUI{ui: testTUIModel(80, 30)}
	model.ui.Composer.Commands.Register(tui.SlashCommand{Name: "/clear"})
	model.ui.Composer.Commands.Register(tui.SlashCommand{Name: "/copy"})
	model.ui.Composer.Editor.SetValue("/c")
	model.ui.Composer.UpdateCompletion(model.workspace)
	updated, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	model = updated.(*conversationTUI)
	if model.ui.Composer.CompletionCursor() != 1 {
		t.Fatalf("completion selection = %d, want 1", model.ui.Composer.CompletionCursor())
	}
	updated, _ = model.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	model = updated.(*conversationTUI)
	if model.ui.Composer.CompletionKind() != "command" || model.ui.Composer.CompletionCursor() != 1 {
		t.Fatalf("non-editing message changed completion selection: kind=%q cursor=%d", model.ui.Composer.CompletionKind(), model.ui.Composer.CompletionCursor())
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	model = updated.(*conversationTUI)
	updated, _ = model.Update(tea.WindowSizeMsg{Width: 80, Height: 30})
	model = updated.(*conversationTUI)
	if model.ui.Composer.CompletionKind() != "" || model.completionRows() != 0 {
		t.Fatalf("non-editing message reopened dismissed completion: kind=%q rows=%d", model.ui.Composer.CompletionKind(), model.completionRows())
	}
}

func TestConversationTUIHistoryNavigationRestoresDraft(t *testing.T) {
	history, err := tui.NewInputHistoryRing(filepath.Join(t.TempDir(), "history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := history.Add("older"); err != nil {
		t.Fatal(err)
	}
	if err := history.Add("newer"); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "notes.md"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	model := &conversationTUI{workspace: workspace, ui: testTUIModel(80, 24)}
	model.ui.Composer.History = history
	model.ui.Composer.Editor.SetValue("draft")
	model.ui.Composer.Editor.SetValue("@notes")
	model.ui.Composer.UpdateCompletion(model.workspace)
	model.ui.Composer.Editor.SetValue("draft")

	updated, _ := model.Update(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	model = updated.(*conversationTUI)
	if got := model.ui.Composer.Editor.Value(); got != "newer" {
		t.Fatalf("history up selected %q, want newer", got)
	}
	if model.ui.Composer.CompletionKind() != "" || model.ui.Composer.CompletionFiles() != nil {
		t.Fatalf("history navigation retained completion state: %+v", model)
	}
	for _, key := range []tea.KeyPressMsg{{Code: 'p', Mod: tea.ModCtrl}, {Code: 'n', Mod: tea.ModCtrl}, {Code: 'n', Mod: tea.ModCtrl}} {
		updated, _ := model.Update(key)
		model = updated.(*conversationTUI)
	}
	if got := model.ui.Composer.Editor.Value(); got != "draft" {
		t.Fatalf("history down restored %q, want draft", got)
	}
}

func TestConversationTUIHistoryNavigationRespectsSoftWrapRows(t *testing.T) {
	history, err := tui.NewInputHistoryRing(filepath.Join(t.TempDir(), "history.jsonl"))
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
	model := &conversationTUI{ui: testTUIModel(80, 24)}
	model.ui.Composer.History = history
	model.ui.Composer.Editor = input
	model.ui.Width = 12
	if info := model.ui.Composer.Editor.LineInfo(); info.Height < 2 {
		t.Fatalf("test draft did not wrap: %+v", info)
	}
	updated, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	model = updated.(*conversationTUI)
	if got := model.ui.Composer.Editor.Value(); got != "a long wrapped draft" || !history.AtEnd() {
		t.Fatalf("Down recalled history from a non-final visual row: draft=%q", got)
	}
	model.ui.Composer.Editor.CursorEnd()
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyUp})
	model = updated.(*conversationTUI)
	if got := model.ui.Composer.Editor.Value(); got != "a long wrapped draft" || !history.AtEnd() {
		t.Fatalf("Up recalled history from a non-first visual row: draft=%q", got)
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
	model := &conversationTUI{workspace: workspace, ui: testTUIModel(80, 24)}
	model.ui.Composer.Editor.SetValue("@")
	model.ui.Composer.UpdateCompletion(model.workspace)
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 19})
	for i := 0; i < 10; i++ {
		updated, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		model = updated.(*conversationTUI)
	}
	selected := model.ui.Composer.CompletionFiles()[model.ui.Composer.CompletionCursor()]
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
	model := &conversationTUI{workspace: workspace, ui: testTUIModel(80, 19)}
	model.ui.Composer.Editor.SetValue("@")
	model.ui.Composer.UpdateCompletion(model.workspace)
	model.Update(tea.WindowSizeMsg{Width: 80, Height: 19})
	for i := 0; i < 10; i++ {
		updated, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		model = updated.(*conversationTUI)
	}
	if got := model.completionRows(); got != 8 {
		t.Fatalf("completion rows = %d, want 8", got)
	}
	if got := model.viewportHeight(); got != 8 {
		t.Fatalf("transcript viewport height = %d, want 8 after reserving completion rows", got)
	}
	visible := 0
	for _, line := range strings.Split(model.View().Content, "\n") {
		if strings.HasPrefix(line, "  file-") || strings.HasPrefix(line, "› file-") {
			visible++
		}
	}
	if visible != 8 {
		t.Fatalf("rendered %d completion rows, want 8", visible)
	}
	if !strings.Contains(model.View().Content, "› "+model.ui.Composer.CompletionFiles()[model.ui.Composer.CompletionCursor()]) {
		t.Fatal("selected completion is not visible in the constrained popup")
	}
}

func TestConversationTUIResponsiveLayoutsStayWithinTerminal(t *testing.T) {
	for _, size := range []struct{ width, height int }{{120, 40}, {80, 24}, {58, 18}, {40, 12}, {40, 1}} {
		t.Run(fmt.Sprintf("%dx%d", size.width, size.height), func(t *testing.T) {
			input := textarea.New()
			input.ShowLineNumbers = false
			model := &conversationTUI{workspace: "/workspace/buddy-agent", conversation: "conversation-12345678901234567890", state: "idle", ui: testTUIModel(size.width, size.height)}
			updated, _ := model.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
			view := updated.(*conversationTUI).View()
			if !view.AltScreen {
				t.Fatal("interactive view did not enable alternate screen")
			}
			if size.width == 80 && size.height == 24 && !strings.Contains(ansi.Strip(view.Content), "Tips for getting started") {
				t.Fatal("empty conversation did not show its ready state")
			}
			lines := strings.Split(view.Content, "\n")
			if len(lines) > size.height {
				t.Fatalf("rendered %d rows into %d-row terminal", len(lines), size.height)
			}
			for _, line := range lines {
				if width := ansi.StringWidth(line); width > size.width {
					t.Fatalf("rendered %d columns into %d-column terminal: %q", width, size.width, line)
				}
			}
		})
	}
}

func TestConversationTUIOneRowPrioritizesComposerOrApproval(t *testing.T) {
	model := &conversationTUI{ui: testTUIModel(40, 1)}
	model.ui.Composer.Editor.Prompt = "› "
	model.ui.Composer.Editor.SetValue("keep this draft")
	updated, _ := model.Update(tea.WindowSizeMsg{Width: 40, Height: 1})
	view := updated.(*conversationTUI).View()
	if got := ansi.Strip(view.Content); !strings.Contains(got, "›") || !strings.Contains(got, "keep this draft") || strings.Contains(got, "Buddy") || strings.Contains(got, "workspace") {
		t.Fatalf("one-row conversation did not prioritize only the composer: %q", got)
	}
	model.setPendingApprovals([]protocol.PermissionRequestNotification{{
		RequestID: "request-1", ConversationID: "conversation-1", TurnID: "turn-1",
		Tool: "bash", Arguments: map[string]any{"command": "go test ./..."},
	}})
	view = model.View()
	if strings.TrimSpace(view.Content) != "1 2 3 4" {
		t.Fatalf("one-row approval view = %q, want only actionable keys", view.Content)
	}
	if model.ui.Composer.Editor.Value() != "keep this draft" {
		t.Fatalf("approval layout changed the composer draft: %q", model.ui.Composer.Editor.Value())
	}
}
