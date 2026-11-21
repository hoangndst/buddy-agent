package daemon

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSocketManager_AcquireAndDoubleLock(t *testing.T) {
	tempDir := t.TempDir()
	sockPath := filepath.Join(tempDir, "daemon.sock")

	mgr1 := NewSocketManager(sockPath)
	listener1, err := mgr1.AcquireLockAndBind()
	if err != nil {
		t.Fatalf("mgr1 failed to acquire lock and bind: %v", err)
	}
	defer func() { _ = mgr1.Close() }()

	if listener1 == nil {
		t.Fatalf("expected non-nil listener")
	}

	// Verify socket file exists
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("socket file was not created: %v", err)
	}

	// Attempt secondary acquire on the same socket path (must fail fast due to flock)
	mgr2 := NewSocketManager(sockPath)
	_, err = mgr2.AcquireLockAndBind()
	if err == nil {
		t.Fatalf("expected mgr2 to fail acquiring lock, but it succeeded")
	}
	if !strings.Contains(err.Error(), "another daemon instance is actively running") {
		t.Fatalf("unexpected error message: %v", err)
	}

	// Verify client can connect to listener1
	client, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("client failed to dial active socket: %v", err)
	}
	_ = client.Close()

	// Clean close of mgr1
	if err := mgr1.Close(); err != nil {
		t.Fatalf("failed to close mgr1: %v", err)
	}

	// Now mgr2 should be able to acquire and bind cleanly
	listener2, err := mgr2.AcquireLockAndBind()
	if err != nil {
		t.Fatalf("mgr2 failed to acquire after mgr1 closed: %v", err)
	}
	defer func() { _ = mgr2.Close() }()
	if listener2 == nil {
		t.Fatalf("expected non-nil listener from mgr2")
	}
}

func TestSocketManager_StaleSocketRecovery(t *testing.T) {
	tempDir := t.TempDir()
	sockPath := filepath.Join(tempDir, "stale.sock")

	// Simulate stale socket file left behind by killed process
	if err := os.WriteFile(sockPath, []byte("stale-socket-content"), 0600); err != nil {
		t.Fatalf("failed to create fake stale socket: %v", err)
	}

	mgr := NewSocketManager(sockPath)
	listener, err := mgr.AcquireLockAndBind()
	if err != nil {
		t.Fatalf("expected manager to cleanly recover from stale socket, got: %v", err)
	}
	defer func() { _ = mgr.Close() }()

	if listener == nil {
		t.Fatalf("expected listener to be valid")
	}

	// Verify we can connect to the recovered socket
	client, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("failed to dial recovered socket: %v", err)
	}
	_ = client.Close()
}
