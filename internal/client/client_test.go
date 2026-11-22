package client

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/hoangndst/buddy-agent/internal/agent"
	"github.com/hoangndst/buddy-agent/internal/agent/provider"
	"github.com/hoangndst/buddy-agent/internal/daemon"
	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
	"github.com/hoangndst/buddy-agent/internal/persistence"
	"github.com/hoangndst/buddy-agent/internal/tools"
)

type mockProvider struct{}

func (m *mockProvider) Protocol() string {
	return "chat"
}

func TestClient_ConversationStartAndSubmit(t *testing.T) {
	workspace := t.TempDir()
	store, err := persistence.NewSQLiteStore(filepath.Join(workspace, "buddy.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	catalog, err := persistence.NewWorkspaceCatalog(filepath.Join(workspace, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	sockPath := filepath.Join(workspace, "conversation.sock")
	factory := func(sessionID string, _ protocol.SessionStartParams, emitHandler agent.EventHandler) (*agent.Engine, func(), error) {
		eng := agent.NewEngine(&mockProvider{}, "test-model", 3, tools.NewRegistry(1000), nil, emitHandler,
			agent.WithSystemPromptOverride("system"), agent.WithStore(store), agent.WithWorkspaceDir(workspace), agent.WithSessionIDOverride(sessionID))
		return eng, nil, nil
	}
	manager := daemon.NewSessionManager(factory)
	manager.SetWorkspaceCatalog(catalog)
	server := daemon.NewServer(daemon.NewSocketManager(sockPath), manager)
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Stop() }()
	client, err := Connect(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	started, err := client.StartConversation(protocol.ConversationStartParams{
		StartID: "stable-start", SubmissionID: "first-submission", Prompt: "first prompt", WorkspaceDir: workspace,
	})
	if err != nil {
		t.Fatalf("start conversation: %v", err)
	}
	capabilities, err := client.Capabilities()
	if err != nil {
		t.Fatalf("daemon capabilities: %v", err)
	}
	if !containsString(capabilities.Methods, protocol.MethodConversationSync) {
		t.Fatalf("daemon did not advertise conversation.sync: %+v", capabilities)
	}
	workspaces, err := client.ListConversationWorkspaces(10)
	if err != nil {
		t.Fatalf("list conversation workspaces: %v", err)
	}
	if len(workspaces.Workspaces) != 1 || workspaces.Workspaces[0].WorkspaceDir != workspace {
		t.Fatalf("unexpected workspaces: %+v", workspaces.Workspaces)
	}
	conversations, err := client.ListConversations(protocol.ConversationListParams{WorkspaceDir: workspace, Limit: 10})
	if err != nil {
		t.Fatalf("list conversations: %v", err)
	}
	if len(conversations.Conversations) != 1 || conversations.Conversations[0].DBPath == "" {
		t.Fatalf("unexpected conversations: %+v", conversations.Conversations)
	}
	queued, err := client.SubmitConversation(protocol.ConversationSubmitParams{
		ConversationID: started.ConversationID, SubmissionID: "second-submission", Prompt: "follow-up", Mode: "queue",
	})
	if err != nil {
		t.Fatalf("submit follow-up: %v", err)
	}
	if queued.Sequence != 2 {
		t.Fatalf("invalid queued submission result: %+v", queued)
	}

	terminalTurns := map[string]bool{}
	timer := time.NewTimer(4 * time.Second)
	defer timer.Stop()
	for len(terminalTurns) < 2 {
		select {
		case evt, ok := <-client.ConversationEvents():
			if !ok {
				t.Fatal("conversation event stream closed")
			}
			if evt.ConversationID != started.ConversationID || evt.SessionID != started.ConversationID || evt.WorkerEpoch == "" || evt.Sequence == 0 {
				t.Fatalf("conversation event missing compatibility/cursor fields: %+v", evt)
			}
			if evt.Data["terminal"] == true {
				if evt.HistoryRevision == 0 {
					t.Fatalf("terminal event missing committed history revision: %+v", evt)
				}
				terminalTurns[evt.TurnID] = true
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for both turn terminals; received %v", terminalTurns)
		}
	}

	firstPage, err := client.SyncConversation(protocol.ConversationSyncParams{ConversationID: started.ConversationID, PageSize: 2})
	if err != nil {
		t.Fatalf("sync conversation: %v", err)
	}
	if firstPage.HistoryRevision == 0 || firstPage.WorkerEpoch == "" || firstPage.EventWatermark == 0 || firstPage.NextPageToken == "" {
		t.Fatalf("sync response missing revision/cursor/page state: %+v", firstPage)
	}
	if firstPage.OutputIncomplete {
		t.Fatalf("completed durable conversation unexpectedly needs output recovery: %+v", firstPage)
	}
	secondPage, err := client.SyncConversation(protocol.ConversationSyncParams{
		ConversationID: started.ConversationID, HistoryRevision: firstPage.HistoryRevision,
		WorkerEpoch: firstPage.WorkerEpoch, AfterSequence: firstPage.EventWatermark,
		PageToken: firstPage.NextPageToken, PageSize: 2,
	})
	if err != nil {
		t.Fatalf("fetch pinned transcript page: %v", err)
	}
	if len(firstPage.Transcript) != 2 || len(secondPage.Transcript) < 1 || secondPage.HistoryRevision != firstPage.HistoryRevision {
		t.Fatalf("revision-pinned transcript pagination failed: first=%+v second=%+v", firstPage, secondPage)
	}
	resumed, err := client.ResumeConversation(protocol.ConversationResumeParams{WorkspaceDir: workspace, ConversationID: started.ConversationID, DBPath: conversations.Conversations[0].DBPath})
	if err != nil {
		t.Fatalf("resume conversation: %v", err)
	}
	if resumed.ConversationID != started.ConversationID || resumed.HistoryRevision != firstPage.HistoryRevision {
		t.Fatalf("resume did not preserve conversation identity/revision: %+v", resumed)
	}
}

func containsString(items []string, target string) bool {
	for _, item := range items {
		if item == target {
			return true
		}
	}
	return false
}

func (m *mockProvider) Generate(_ context.Context, _ provider.GenerateRequest) (*provider.GenerateResponse, error) {
	// Simulate delay so session stays active during test
	time.Sleep(300 * time.Millisecond)
	return &provider.GenerateResponse{
		Content: "Task completed",
	}, nil
}

func TestClient_DaemonLifecycleAndStreaming(t *testing.T) {
	tempDir := t.TempDir()
	sockPath := filepath.Join(tempDir, "client_test.sock")

	sockMgr := daemon.NewSocketManager(sockPath)

	factory := func(_ string, _ protocol.SessionStartParams, emitHandler agent.EventHandler) (*agent.Engine, func(), error) {
		eng := agent.NewEngine(
			nil,
			"test-model",
			5,
			nil,
			nil,
			emitHandler,
			agent.WithSystemPromptOverride("mock prompt"),
		)
		return eng, nil, nil
	}

	sessionMgr := daemon.NewSessionManager(factory)
	server := daemon.NewServer(sockMgr, sessionMgr)

	if err := server.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer func() { _ = server.Stop() }()

	// 1. Connect Client
	c, err := Connect(sockPath)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	defer func() { _ = c.Close() }()

	// 2. Ping
	if err := c.Ping(); err != nil {
		t.Fatalf("ping failed: %v", err)
	}

	// 3. Status
	status, err := c.Status()
	if err != nil {
		t.Fatalf("status failed: %v", err)
	}
	if status.SocketPath != sockPath {
		t.Fatalf("expected socket path %s, got %s", sockPath, status.SocketPath)
	}

	// 4. Cancel nonexistent session returns error
	err = c.CancelSession("sess_non_existent")
	if err == nil {
		t.Fatalf("expected error on canceling non-existent session")
	}

	// 5. Test StopDaemon
	if err := c.StopDaemon(); err != nil {
		t.Fatalf("stop daemon failed: %v", err)
	}

	// Wait briefly for server to shutdown
	time.Sleep(100 * time.Millisecond)

	// Attempting ping after stop should fail
	if err := c.Ping(); err == nil {
		t.Errorf("expected ping to fail after daemon stopped")
	}
}

func TestClient_SessionStreamingAndReattach(t *testing.T) {
	tempDir := t.TempDir()
	sockPath := filepath.Join(tempDir, "stream_test.sock")

	sockMgr := daemon.NewSocketManager(sockPath)

	var workerEmitter agent.EventHandler
	factory := func(_ string, _ protocol.SessionStartParams, emitHandler agent.EventHandler) (*agent.Engine, func(), error) {
		workerEmitter = emitHandler
		eng := agent.NewEngine(
			&mockProvider{},
			"test-model",
			5,
			tools.NewRegistry(10000),
			nil,
			emitHandler,
			agent.WithSystemPromptOverride("mock prompt"),
		)
		return eng, nil, nil
	}

	sessionMgr := daemon.NewSessionManager(factory)
	server := daemon.NewServer(sockMgr, sessionMgr)

	if err := server.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer func() { _ = server.Stop() }()

	// Connect Client 1
	c1, err := Connect(sockPath)
	if err != nil {
		t.Fatalf("c1 failed to connect: %v", err)
	}
	defer func() { _ = c1.Close() }()

	// Start Session
	sessionID, err := c1.StartSession(protocol.SessionStartParams{
		Task: "analyze codebase",
	})
	if err != nil {
		t.Fatalf("c1 failed to start session: %v", err)
	}
	if sessionID == "" {
		t.Fatalf("expected non-empty session ID")
	}

	// Verify initial step_start event on c1
	select {
	case evt := <-c1.Events():
		if evt.Type != "step_start" {
			t.Fatalf("expected initial step_start event, got: %+v", evt)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for initial step_start on c1")
	}

	// Emit custom events from daemon
	workerEmitter(agent.Event{Type: agent.EventReasoning, Content: "analyzing project structure"})
	workerEmitter(agent.Event{Type: agent.EventToolCall, ToolName: "list_directory"})
	workerEmitter(agent.Event{Type: agent.EventToolResult, ToolName: "list_directory", ToolResult: "file1.go\nfile2.go"})
	workerEmitter(agent.Event{Type: agent.EventError, Content: "subagent child step error"})

	// Verify c1 receives reasoning event
	select {
	case evt := <-c1.Events():
		if evt.Content != "analyzing project structure" {
			t.Fatalf("unexpected event 1: %+v", evt)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for event 1 on c1")
	}

	select {
	case evt := <-c1.Events():
		if evt.Data["tool_name"] != "list_directory" {
			t.Fatalf("unexpected event 2: %+v", evt)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for event 2 on c1")
	}

	select {
	case evt := <-c1.Events():
		if evt.Type != "tool_result" || evt.Content != "file1.go\nfile2.go" || evt.Data["tool_result"] != "file1.go\nfile2.go" {
			t.Fatalf("unexpected event 3 (tool_result): %+v", evt)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for event 3 on c1")
	}

	select {
	case evt := <-c1.Events():
		if evt.Type != "error" || evt.Content != "subagent child step error" || evt.Data["terminal"] == true {
			t.Fatalf("unexpected non-terminal error event: %+v", evt)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for intermediate error on c1")
	}

	// Connect Client 2 (Observer) and attach to existing session
	c2, err := Connect(sockPath)
	if err != nil {
		t.Fatalf("c2 failed to connect: %v", err)
	}
	defer func() { _ = c2.Close() }()

	attachRes, err := c2.AttachSession(protocol.SessionAttachParams{
		SessionID:   sessionID,
		LastEventID: 0,
		TakeControl: false,
	})
	if err != nil {
		t.Fatalf("c2 failed to attach: %v", err)
	}
	if attachRes.SessionID != sessionID || attachRes.Role != "observer" {
		t.Fatalf("unexpected attach result: %+v", attachRes)
	}

	// Verify c2 receives replayed events (step_start, then reasoning, then tool_call, then tool_result)
	select {
	case evt := <-c2.Events():
		if evt.Type != "step_start" {
			t.Fatalf("c2 replay event 1 mismatch: expected step_start, got %+v", evt)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for replayed step_start on c2")
	}

	select {
	case evt := <-c2.Events():
		if evt.Content != "analyzing project structure" {
			t.Fatalf("c2 replay event 2 mismatch: expected reasoning, got %+v", evt)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for replayed reasoning on c2")
	}

	select {
	case evt := <-c2.Events():
		if evt.Type != "tool_call" {
			t.Fatalf("c2 replay event 3 mismatch: expected tool_call, got %+v", evt)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for replayed tool_call on c2")
	}

	select {
	case evt := <-c2.Events():
		if evt.Type != "tool_result" || evt.Content != "file1.go\nfile2.go" {
			t.Fatalf("c2 replay event 4 mismatch: expected tool_result, got %+v", evt)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for replayed tool_result on c2")
	}

	// Verify non-controller (c2, which joined with takeControl: false) cannot cancel session
	if err := c2.CancelSession(sessionID); err == nil {
		t.Fatalf("expected permission error when non-controller attempts to cancel session")
	}

	// Cancel Session from c1 (the primary controller)
	if err := c1.CancelSession(sessionID); err != nil {
		t.Fatalf("c1 failed to cancel session: %v", err)
	}
}

func TestClient_PermissionRequestAndResponse(t *testing.T) {
	tempDir := t.TempDir()
	sockPath := filepath.Join(tempDir, "perm_test.sock")

	sockMgr := daemon.NewSocketManager(sockPath)

	var activeWorker *daemon.SessionWorker
	factory := func(_ string, _ protocol.SessionStartParams, emitHandler agent.EventHandler) (*agent.Engine, func(), error) {
		eng := agent.NewEngine(
			&mockProvider{},
			"test-model",
			5,
			tools.NewRegistry(10000),
			nil,
			emitHandler,
			agent.WithSystemPromptOverride("mock prompt"),
		)
		return eng, nil, nil
	}

	sessionMgr := daemon.NewSessionManager(factory)
	server := daemon.NewServer(sockMgr, sessionMgr)

	if err := server.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer func() { _ = server.Stop() }()

	c1, err := Connect(sockPath)
	if err != nil {
		t.Fatalf("connect failed: %v", err)
	}
	defer func() { _ = c1.Close() }()

	sessionID, err := c1.StartSession(protocol.SessionStartParams{
		Task: "perform system check",
	})
	if err != nil {
		t.Fatalf("start session failed: %v", err)
	}

	worker, ok := sessionMgr.GetSession(sessionID)
	if !ok {
		t.Fatalf("expected worker to be registered")
	}
	activeWorker = worker

	// Trigger permission request in background
	permResultCh := make(chan bool, 1)
	permErrCh := make(chan error, 1)
	go func() {
		allowed, err := activeWorker.RequestPermission("bash", map[string]any{"command": "rm -rf ./tmp"})
		permResultCh <- allowed
		permErrCh <- err
	}()

	// Client receives permission request
	select {
	case req := <-c1.Permissions():
		if req.Tool != "bash" || req.SessionID != sessionID {
			t.Fatalf("unexpected permission request: %+v", req)
		}
		// Client approves permission
		if err := c1.RespondPermission(req.RequestID, "allow"); err != nil {
			t.Fatalf("failed to respond to permission: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for permission request on c1")
	}

	// Verify worker unblocked with allowed=true
	select {
	case allowed := <-permResultCh:
		if !allowed {
			t.Fatalf("expected permission to be allowed, got false")
		}
		if err := <-permErrCh; err != nil {
			t.Fatalf("unexpected permission error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for worker permission return")
	}
}
