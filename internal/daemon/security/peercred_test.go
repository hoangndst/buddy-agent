package security

import (
	"net"
	"path/filepath"
	"testing"
)

func TestVerifyPeerCredentials_SameUser(t *testing.T) {
	tempDir := t.TempDir()
	sockPath := filepath.Join(tempDir, "test.sock")

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("failed to listen on socket: %v", err)
	}
	defer func() { _ = listener.Close() }()

	errCh := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			errCh <- err
			return
		}
		defer func() { _ = conn.Close() }()
		errCh <- VerifyPeerCredentials(conn)
	}()

	client, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("failed to dial socket: %v", err)
	}
	defer func() { _ = client.Close() }()

	if err := <-errCh; err != nil {
		t.Fatalf("expected peer credentials verification to pass for same user, got: %v", err)
	}
}
