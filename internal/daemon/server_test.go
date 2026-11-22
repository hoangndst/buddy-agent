package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hoangndst/buddy-agent/internal/agent"
	"github.com/hoangndst/buddy-agent/internal/agent/provider"
	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
	"github.com/hoangndst/buddy-agent/internal/persistence"
	"github.com/hoangndst/buddy-agent/internal/tools"
)

type mockProvider struct{}

type resumeTestProvider struct{ calls chan struct{} }

func (p resumeTestProvider) Protocol() string { return "chat" }
func (p resumeTestProvider) Generate(context.Context, provider.GenerateRequest) (*provider.GenerateResponse, error) {
	select {
	case p.calls <- struct{}{}:
	default:
	}
	return &provider.GenerateResponse{Protocol: "chat", Content: "restored answer"}, nil
}

func (m *mockProvider) Protocol() string {
	return "chat"
}

func (m *mockProvider) Generate(_ context.Context, _ provider.GenerateRequest) (*provider.GenerateResponse, error) {
	return &provider.GenerateResponse{
		Content: "Task completed",
	}, nil
}

func TestServer_LifecycleAndIPC(t *testing.T) {
	tempDir := t.TempDir()
	sockPath := filepath.Join(tempDir, "daemon_test.sock")

	sockMgr := NewSocketManager(sockPath)

	// Mock engine factory that creates a test engine that emits simulated events
	factory := func(_ string, _ protocol.SessionStartParams, emitHandler agent.EventHandler) (*agent.Engine, func(), error) {
		// Use a mock engine
		eng := agent.NewEngine(
			nil,
			"test-model",
			10,
			nil,
			nil,
			emitHandler,
			agent.WithSystemPromptOverride("test prompt"),
		)
		return eng, nil, nil
	}

	sessionMgr := NewSessionManager(factory)
	server := NewServer(sockMgr, sessionMgr)

	if err := server.Start(); err != nil {
		t.Fatalf("failed to start daemon server: %v", err)
	}
	defer func() { _ = server.Stop() }()

	// Connect client via Unix socket
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("client failed to dial socket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	codec := protocol.NewStreamCodec(conn, conn)

	// 1. Test daemon.ping
	pingReq, _ := protocol.NewRequest("ping-1", protocol.MethodDaemonPing, nil)
	if err := codec.WriteMessage(pingReq); err != nil {
		t.Fatalf("failed to send ping: %v", err)
	}

	pingRes, err := codec.ReadMessage()
	if err != nil {
		t.Fatalf("failed to read ping response: %v", err)
	}
	if *pingRes.ID != "ping-1" {
		t.Fatalf("expected ID ping-1, got %v", *pingRes.ID)
	}

	// 2. Test daemon.status
	statusReq, _ := protocol.NewRequest("status-1", protocol.MethodDaemonStatus, nil)
	if err := codec.WriteMessage(statusReq); err != nil {
		t.Fatalf("failed to send status: %v", err)
	}
	statusRes, err := codec.ReadMessage()
	if err != nil {
		t.Fatalf("failed to read status response: %v", err)
	}
	var statusData protocol.DaemonStatusResult
	if err := json.Unmarshal(statusRes.Result, &statusData); err != nil {
		t.Fatalf("failed to parse status result: %v", err)
	}
	if statusData.SocketPath != sockPath {
		t.Fatalf("expected socket path %s, got %s", sockPath, statusData.SocketPath)
	}

	// 3. Test session.cancel on unknown session returns error
	cancelReq, _ := protocol.NewRequest("cancel-1", protocol.MethodSessionCancel, protocol.SessionCancelParams{
		SessionID: "sess_non_existent",
	})
	if err := codec.WriteMessage(cancelReq); err != nil {
		t.Fatalf("failed to send cancel: %v", err)
	}
	cancelRes, err := codec.ReadMessage()
	if err != nil {
		t.Fatalf("failed to read cancel response: %v", err)
	}
	if cancelRes.Error == nil || cancelRes.Error.Code != protocol.ErrCodeSessionNotFound {
		t.Fatalf("expected ErrCodeSessionNotFound, got %+v", cancelRes)
	}

	// 4. Test ID-less notification (must not panic the server)
	pingNotify, _ := protocol.NewNotification(protocol.MethodDaemonPing, nil)
	if err := codec.WriteMessage(pingNotify); err != nil {
		t.Fatalf("failed to send notification: %v", err)
	}

	// Follow-up request must succeed without connection disruption
	pingReq2, _ := protocol.NewRequest("ping-2", protocol.MethodDaemonPing, nil)
	if err := codec.WriteMessage(pingReq2); err != nil {
		t.Fatalf("failed to send second ping: %v", err)
	}
	pingRes2, err := codec.ReadMessage()
	if err != nil {
		t.Fatalf("failed to read second ping response: %v", err)
	}
	if pingRes2.ID == nil || *pingRes2.ID != "ping-2" {
		t.Fatalf("expected ID ping-2, got %+v", pingRes2)
	}
}

func TestSessionManager_BoundCompletedRetention(t *testing.T) {
	factory := func(_ string, _ protocol.SessionStartParams, emitHandler agent.EventHandler) (*agent.Engine, func(), error) {
		eng := agent.NewEngine(
			&mockProvider{},
			"test-model",
			1,
			tools.NewRegistry(1000),
			nil,
			emitHandler,
			agent.WithSystemPromptOverride("prompt"),
		)
		return eng, nil, nil
	}

	sm := NewSessionManager(factory)
	sm.SetMaxCompletedSessions(2)

	// Start and complete 3 sessions sequentially
	var workers []*SessionWorker
	for i := 0; i < 3; i++ {
		w, err := sm.StartSession(t.Context(), protocol.SessionStartParams{Task: "task"}, nil)
		if err != nil {
			t.Fatalf("StartSession failed: %v", err)
		}
		workers = append(workers, w)
		for w.IsActive() {
			time.Sleep(5 * time.Millisecond)
		}
	}

	// First session should have been evicted; last 2 should remain
	if _, ok := sm.GetSession(workers[0].SessionID()); ok {
		t.Errorf("expected session 0 (%s) to be evicted from manager retention", workers[0].SessionID())
	}
	if _, ok := sm.GetSession(workers[1].SessionID()); !ok {
		t.Errorf("expected session 1 (%s) to be retained", workers[1].SessionID())
	}
	if _, ok := sm.GetSession(workers[2].SessionID()); !ok {
		t.Errorf("expected session 2 (%s) to be retained", workers[2].SessionID())
	}
}

func TestSessionManager_StartConversationAndQueueSubmissions(t *testing.T) {
	workspace := t.TempDir()
	store, err := persistence.NewSQLiteStore(filepath.Join(workspace, "buddy.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	factory := func(sessionID string, _ protocol.SessionStartParams, emitHandler agent.EventHandler) (*agent.Engine, func(), error) {
		eng := agent.NewEngine(&mockProvider{}, "test-model", 3, tools.NewRegistry(1000), nil, emitHandler,
			agent.WithSystemPromptOverride("system"), agent.WithStore(store), agent.WithWorkspaceDir(workspace), agent.WithSessionIDOverride(sessionID))
		return eng, nil, nil
	}
	manager := NewSessionManager(factory)
	params := protocol.ConversationStartParams{StartID: "start-stable", SubmissionID: "submission-first", Prompt: "first prompt", WorkspaceDir: workspace}
	worker, started, err := manager.StartConversation(t.Context(), params, nil)
	if err != nil {
		t.Fatalf("start conversation: %v", err)
	}
	if started.ConversationID == "" || started.TurnID == "" || started.SessionID != started.ConversationID {
		t.Fatalf("invalid start result: %+v", started)
	}

	// A retry must return the same durable conversation and first turn.
	workerRetry, retry, err := manager.StartConversation(t.Context(), params, nil)
	if err != nil {
		t.Fatalf("retry conversation.start: %v", err)
	}
	if workerRetry != worker || !retry.Existing || retry.ConversationID != started.ConversationID || retry.TurnID != started.TurnID {
		t.Fatalf("conversation.start retry did not reuse the original worker/result: worker=%p retry=%+v", workerRetry, retry)
	}
	changed := params
	changed.Prompt = "changed payload"
	if _, _, err := manager.StartConversation(t.Context(), changed, nil); !errors.Is(err, persistence.ErrSubmissionConflict) {
		t.Fatalf("expected changed start payload to conflict, got %v", err)
	}

	waitForConversationTurns(t, store, started.ConversationID, 1)
	second, existing, err := worker.Submit(t.Context(), &persistence.SubmissionRecord{
		ConversationID: started.ConversationID, SubmissionID: "submission-second", Prompt: "follow-up", Mode: persistence.SubmissionQueue,
	})
	if err != nil {
		t.Fatalf("submit follow-up: %v", err)
	}
	if existing || second.Sequence != 2 {
		t.Fatalf("unexpected accepted submission: %+v existing=%v", second, existing)
	}
	waitForConversationTurns(t, store, started.ConversationID, 2)
	second, err = store.GetSubmission(t.Context(), started.ConversationID, "submission-second")
	if err != nil || second.TurnID == nil || *second.TurnID == "" {
		t.Fatalf("queued prompt was not assigned when its turn started: submission=%+v err=%v", second, err)
	}

	duplicate, existing, err := worker.Submit(t.Context(), &persistence.SubmissionRecord{
		ConversationID: started.ConversationID, SubmissionID: "submission-second", Prompt: "follow-up", Mode: persistence.SubmissionQueue,
	})
	if err != nil {
		t.Fatalf("retry conversation.submit: %v", err)
	}
	if !existing || duplicate.Sequence != second.Sequence || duplicate.TurnID == nil || *duplicate.TurnID != *second.TurnID {
		t.Fatalf("submission retry did not preserve accepted identity: %+v existing=%v", duplicate, existing)
	}

	session, err := store.GetSession(t.Context(), started.ConversationID)
	if err != nil {
		t.Fatal(err)
	}
	if session.InteractionKind != persistence.InteractionConversation || session.Depth != 0 || session.ParentSessionID != nil {
		t.Fatalf("conversation persisted with wrong interaction kind/role: %+v", session)
	}
	manager.Drain(time.Second)

	restartedManager := NewSessionManager(factory)
	restored, record, err := restartedManager.ResumeConversation(t.Context(), protocol.ConversationResumeParams{
		WorkspaceDir: workspace, ConversationID: started.ConversationID,
	}, nil)
	if err != nil {
		t.Fatalf("restore conversation after daemon restart: %v", err)
	}
	if record.ID != started.ConversationID || record.InteractionKind != persistence.InteractionConversation {
		t.Fatalf("restored wrong conversation record: %+v", record)
	}
	if _, _, err := restored.Submit(t.Context(), &persistence.SubmissionRecord{
		ConversationID: started.ConversationID, SubmissionID: "submission-after-restore", Prompt: "continue after restore", Mode: persistence.SubmissionQueue,
	}); err != nil {
		t.Fatalf("submit after restore: %v", err)
	}
	waitForConversationTurns(t, store, started.ConversationID, 3)
	restartedManager.Drain(time.Second)
}

func TestResumeConversationRequiresEphemeralCredentialAndReconfigures(t *testing.T) {
	workspace := t.TempDir()
	dbPath := filepath.Join(workspace, "buddy.db")
	store, err := persistence.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	calls := make(chan struct{}, 4)
	factory := func(sessionID string, params protocol.SessionStartParams, emitHandler agent.EventHandler) (*agent.Engine, func(), error) {
		eng := agent.NewEngine(resumeTestProvider{calls: calls}, params.Model, 3, tools.NewRegistry(1000), nil, emitHandler,
			agent.WithSystemPromptOverride("system"), agent.WithStore(store), agent.WithWorkspaceDir(workspace), agent.WithSessionIDOverride(sessionID),
			agent.WithProviderMetadata(params.BaseURL, params.APIType, 1000))
		return eng, nil, nil
	}
	secret := "ephemeral-test-secret"
	manager := NewSessionManager(factory)
	_, started, err := manager.StartConversation(t.Context(), protocol.ConversationStartParams{
		StartID: "ephemeral-start", SubmissionID: "ephemeral-first", Prompt: "first prompt", WorkspaceDir: workspace,
		Model: "test-model", BaseURL: "https://provider.example/v1", APIType: "chat", APIKey: &secret,
	}, nil)
	if err != nil {
		t.Fatalf("start conversation: %v", err)
	}
	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("initial provider request did not run")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		submission, getErr := store.GetSubmission(t.Context(), started.ConversationID, "ephemeral-first")
		if getErr == nil && submission.Status == persistence.SubmissionConsumed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	profile, err := store.GetConversationProviderProfile(t.Context(), started.ConversationID)
	if err != nil || profile == nil || profile.CredentialSource != persistence.ConversationCredentialEphemeral || profile.BaseURL != "https://provider.example/v1" || profile.APIType != "chat" {
		t.Fatalf("provider profile did not preserve safe settings/source: profile=%+v err=%v", profile, err)
	}
	if strings.Contains(fmt.Sprintf("%+v", profile), secret) {
		t.Fatal("ephemeral API key appeared in the persisted provider profile")
	}
	var hasAPIKeyColumn int
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM pragma_table_info('conversation_provider_profiles') WHERE name = 'api_key'`).Scan(&hasAPIKeyColumn); err != nil || hasAPIKeyColumn != 0 {
		t.Fatalf("provider profile schema stores an API key column: count=%d err=%v", hasAPIKeyColumn, err)
	}
	manager.Drain(time.Second)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = persistence.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	restoredManager := NewSessionManager(func(sessionID string, params protocol.SessionStartParams, emitHandler agent.EventHandler) (*agent.Engine, func(), error) {
		eng := agent.NewEngine(resumeTestProvider{calls: calls}, params.Model, 3, tools.NewRegistry(1000), nil, emitHandler,
			agent.WithSystemPromptOverride("system"), agent.WithStore(store), agent.WithWorkspaceDir(workspace), agent.WithSessionIDOverride(sessionID),
			agent.WithProviderMetadata(params.BaseURL, params.APIType, 1000))
		return eng, nil, nil
	})
	resume := protocol.ConversationResumeParams{
		WorkspaceDir: workspace, ConversationID: started.ConversationID, DBPath: dbPath,
		Model: "test-model", BaseURL: "https://provider.example/v1", APIType: "chat",
	}
	restored, _, err := restoredManager.ResumeConversation(t.Context(), resume, nil)
	if err != nil {
		t.Fatalf("restore history without ephemeral key: %v", err)
	}
	if reason := restored.ExecutionBlocked(); !strings.Contains(reason, "credentials") {
		t.Fatalf("restore did not block execution pending credentials: %q", reason)
	}
	snapshot, _, err := restored.Sync(t.Context(), protocol.ConversationSyncParams{ConversationID: started.ConversationID}, nil)
	if err != nil || snapshot.ExecutionBlocked == "" || len(snapshot.Transcript) == 0 {
		t.Fatalf("blocked restore did not expose history and reason: snapshot=%+v err=%v", snapshot, err)
	}
	resume.APIKey = &secret
	resume.ConversationID = ""
	if _, _, err := restoredManager.ResumeConversation(t.Context(), resume, nil); err != nil {
		t.Fatalf("resume latest with re-supplied ephemeral key: %v", err)
	}
	if reason := restored.ExecutionBlocked(); reason != "" {
		t.Fatalf("execution remained blocked after credentials were supplied: %q", reason)
	}
	if _, _, err := restored.Submit(t.Context(), &persistence.SubmissionRecord{
		ConversationID: started.ConversationID, SubmissionID: "after-restore", Prompt: "continue", Mode: persistence.SubmissionQueue,
	}); err != nil {
		t.Fatalf("submit after reconfiguration: %v", err)
	}
	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("provider request did not run after credentials were re-supplied")
	}
	restoredManager.Drain(time.Second)
}

func TestStartConversationRetryRestoresSavedProviderProfile(t *testing.T) {
	workspace := t.TempDir()
	dbPath := filepath.Join(workspace, "retry-profile.db")
	store, err := persistence.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	calls := make(chan struct{}, 4)
	makeFactory := func(captured chan<- protocol.SessionStartParams) EngineFactory {
		return func(sessionID string, params protocol.SessionStartParams, emitHandler agent.EventHandler) (*agent.Engine, func(), error) {
			if captured != nil {
				captured <- params
			}
			eng := agent.NewEngine(resumeTestProvider{calls: calls}, params.Model, 3, tools.NewRegistry(1000), nil, emitHandler,
				agent.WithSystemPromptOverride("system"), agent.WithStore(store), agent.WithWorkspaceDir(workspace), agent.WithSessionIDOverride(sessionID),
				agent.WithProviderMetadata(params.BaseURL, params.APIType, 1000))
			return eng, nil, nil
		}
	}
	secret := "retry-ephemeral-key"
	initial := protocol.ConversationStartParams{
		StartID: "retry-profile-start", SubmissionID: "retry-profile-submission", Prompt: "retain this conversation", WorkspaceDir: workspace,
		Model: "original-model", BaseURL: "https://original.example/v1", APIType: "chat", APIKey: &secret,
	}
	firstManager := NewSessionManager(makeFactory(nil))
	_, started, err := firstManager.StartConversation(t.Context(), initial, nil)
	if err != nil {
		t.Fatalf("start original conversation: %v", err)
	}
	select {
	case <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("original provider request did not run")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		submission, getErr := store.GetSubmission(t.Context(), started.ConversationID, initial.SubmissionID)
		if getErr == nil && submission.Status == persistence.SubmissionConsumed {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	firstManager.Drain(time.Second)

	captured := make(chan protocol.SessionStartParams, 4)
	retryManager := NewSessionManager(makeFactory(captured))
	retry := initial
	retry.APIKey = nil
	retry.Model = "incoming-wrong-model"
	retry.BaseURL = "https://incoming-wrong.example/v1"
	retry.APIType = "responses"
	worker, result, err := retryManager.StartConversation(t.Context(), retry, nil)
	if err != nil {
		t.Fatalf("retry existing conversation without its ephemeral key: %v", err)
	}
	if !result.Existing || worker.ConversationID() != started.ConversationID {
		t.Fatalf("retry did not reuse the persisted conversation: result=%+v worker=%q", result, worker.ConversationID())
	}
	if reason := worker.ExecutionBlocked(); !strings.Contains(reason, "credentials") || reason == "" {
		t.Fatalf("retry did not block execution pending original credentials: %q", reason)
	}
	var lastFactoryParams protocol.SessionStartParams
	for {
		select {
		case lastFactoryParams = <-captured:
		default:
			goto capturedParams
		}
	}
capturedParams:
	if lastFactoryParams.Model != "original-model" || lastFactoryParams.BaseURL != "https://original.example/v1" || lastFactoryParams.APIType != "chat" || lastFactoryParams.APIKey != nil {
		t.Fatalf("retry restored using incoming settings instead of persisted profile: %+v", lastFactoryParams)
	}
	snapshot, _, err := worker.Sync(t.Context(), protocol.ConversationSyncParams{ConversationID: started.ConversationID}, nil)
	if err != nil || snapshot.ExecutionBlocked == "" || len(snapshot.Transcript) == 0 {
		t.Fatalf("blocked retry did not preserve history: snapshot=%+v err=%v", snapshot, err)
	}

	retry.APIKey = &secret
	retry.Model = "original-model"
	retry.BaseURL = "https://original.example/v1"
	retry.APIType = "chat"
	if _, result, err = retryManager.StartConversation(t.Context(), retry, nil); err != nil || !result.Existing {
		t.Fatalf("retry with original ephemeral credentials: result=%+v err=%v", result, err)
	}
	if reason := worker.ExecutionBlocked(); reason != "" {
		t.Fatalf("retry with credentials left execution blocked: %q", reason)
	}
	retryManager.Drain(time.Second)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func waitForConversationTurns(t *testing.T, store persistence.Store, conversationID string, count int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		turns, err := store.ListConversationTurns(t.Context(), conversationID)
		if err == nil && len(turns) >= count {
			ready := true
			for _, turn := range turns[:count] {
				if turn.Status != persistence.StatusCompleted {
					ready = false
					break
				}
			}
			if ready {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	turns, err := store.ListConversationTurns(t.Context(), conversationID)
	t.Fatalf("conversation did not complete %d turns: turns=%+v err=%v", count, turns, err)
}

func TestServer_DoneSignalsAfterDrain(t *testing.T) {
	tempDir := t.TempDir()
	sockPath := filepath.Join(tempDir, "drain_test.sock")

	factory := func(_ string, _ protocol.SessionStartParams, emitHandler agent.EventHandler) (*agent.Engine, func(), error) {
		return agent.NewEngine(
			nil,
			"test-model",
			5,
			tools.NewRegistry(1000),
			nil,
			emitHandler,
		), nil, nil
	}

	sockMgr := NewSocketManager(sockPath)
	sm := NewSessionManager(factory)
	server := NewServer(sockMgr, sm)

	if err := server.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}

	// Verify server.Done() is open while running
	select {
	case <-server.Done():
		t.Fatal("expected server.Done() to be open while running")
	default:
	}

	doneClosed := make(chan struct{})
	go func() {
		<-server.Done()
		close(doneClosed)
	}()

	if err := server.Stop(); err != nil {
		t.Fatalf("server.Stop failed: %v", err)
	}

	select {
	case <-doneClosed:
		// Done channel closed after Stop
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for server.Done() to close")
	}

	lockPath := sockPath + ".lock"
	if _, err := os.Stat(lockPath); err != nil {
		t.Errorf("expected lock file %s to remain on disk after server.Stop, got: %v", lockPath, err)
	}
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Errorf("expected socket file %s to be unlinked after server.Stop", sockPath)
	}
}

func TestServer_OversizedFrameClosesConnectionWithoutLoop(t *testing.T) {
	tempDir := t.TempDir()
	sockPath := filepath.Join(tempDir, "oversized_test.sock")

	sockMgr := NewSocketManager(sockPath)
	sm := NewSessionManager(nil)
	server := NewServer(sockMgr, sm)

	if err := server.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer func() { _ = server.Stop() }()

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer func() { _ = conn.Close() }()

	codec := protocol.NewStreamCodec(conn, conn)
	pingReq, _ := protocol.NewRequest("ping-oversized-1", protocol.MethodDaemonPing, nil)
	if err := codec.WriteMessage(pingReq); err != nil {
		t.Fatalf("ping failed: %v", err)
	}

	pingRes, err := codec.ReadMessage()
	if err != nil {
		t.Fatalf("ping response failed: %v", err)
	}
	if *pingRes.ID != "ping-oversized-1" {
		t.Fatalf("unexpected ping response: %+v", pingRes)
	}

	// Send an oversized frame that exceeds DefaultMaxMessageSize
	oversized := make([]byte, protocol.DefaultMaxMessageSize+4096)
	for i := range oversized {
		oversized[i] = 'x'
	}
	oversized[len(oversized)-1] = '\n'

	if _, err := conn.Write(oversized); err != nil {
		t.Fatalf("failed to write oversized frame: %v", err)
	}

	// Server should close the connection upon terminal scanner error, causing client read to yield EOF
	buf := make([]byte, 128)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, readErr := conn.Read(buf)
	if readErr == nil {
		t.Fatal("expected read error/EOF after server closes connection on oversized frame")
	}

	// Verify server cleaned up the client from active clients map
	deadline := time.Now().Add(1 * time.Second)
	cleaned := false
	for time.Now().Before(deadline) {
		server.clientsMu.RLock()
		clientCount := len(server.clients)
		server.clientsMu.RUnlock()
		if clientCount == 0 {
			cleaned = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !cleaned {
		t.Fatal("server did not clean up client connection after stream error")
	}
}

func TestCreateEventNotification_OversizedToolResultBounded(t *testing.T) {
	// HTML-escaped characters expand sixfold during JSON encoding; the encoded
	// notification must remain bounded after escaping, not just before it.
	largeResult := strings.Repeat("&", 800*1024)
	evt := agent.Event{
		Type:       agent.EventToolResult,
		Step:       1,
		ToolName:   "bash",
		ToolResult: largeResult,
	}

	msg, err := createEventNotification("sess_test", 1, evt, false)
	if err != nil {
		t.Fatalf("createEventNotification failed: %v", err)
	}

	encoded, err := protocol.EncodeMessage(msg)
	if err != nil {
		t.Fatalf("EncodeMessage failed: %v", err)
	}

	if len(encoded) >= protocol.DefaultMaxMessageSize {
		t.Fatalf("encoded message size %d bytes exceeds DefaultMaxMessageSize (%d)", len(encoded), protocol.DefaultMaxMessageSize)
	}

	// Must decode cleanly through stream codec scanner
	decoded, err := protocol.DecodeMessage(encoded)
	if err != nil {
		t.Fatalf("DecodeMessage failed on bounded notification: %v", err)
	}

	var notify protocol.SessionEventNotification
	if err := json.Unmarshal(decoded.Params, &notify); err != nil {
		t.Fatalf("unmarshal params failed: %v", err)
	}

	// Verify Content was not duplicated to 800 KB
	if len(notify.Content) > 2048 {
		t.Errorf("expected Content to be a preview summary, got length %d", len(notify.Content))
	}

	// Verify tool_result was bounded
	tr, ok := notify.Data["tool_result"].(string)
	if !ok {
		t.Fatalf("missing tool_result in Data: %+v", notify.Data)
	}
	if len(tr) > maxNotificationFieldBytes+128 {
		t.Errorf("expected tool_result bounded to ~%d, got %d", maxNotificationFieldBytes, len(tr))
	}
	if !strings.Contains(tr, "[output truncated for streaming]") {
		t.Errorf("expected truncation marker in tool_result")
	}
}
