package daemon

import (
	"encoding/json"
	"net"
	"path/filepath"
	"testing"

	"github.com/hoangndst/buddy-agent/internal/agent"
	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
)

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
}
