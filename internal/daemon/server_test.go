package daemon

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/hoangndst/buddy-agent/internal/agent"
	"github.com/hoangndst/buddy-agent/internal/agent/provider"
	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
	"github.com/hoangndst/buddy-agent/internal/tools"
)

type mockProvider struct{}

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
}
