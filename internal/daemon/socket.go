package daemon

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// DefaultSocketFileName defines the standard socket filename.
const DefaultSocketFileName = "buddy.sock"

// GetDefaultSocketPath resolves the standard socket path adhering to OS hierarchy conventions.
func GetDefaultSocketPath() string {
	if env := os.Getenv("BUDDY_SOCKET_PATH"); strings.TrimSpace(env) != "" {
		return env
	}

	switch runtime.GOOS {
	case "windows":
		if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
			return filepath.Join(localAppData, "buddy", DefaultSocketFileName)
		}
	case "linux":
		if xdgRuntime := os.Getenv("XDG_RUNTIME_DIR"); xdgRuntime != "" {
			return filepath.Join(xdgRuntime, "buddy", DefaultSocketFileName)
		}
	}

	// Default fallback: ~/.buddy/buddy.sock
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".buddy", DefaultSocketFileName)
}

// SocketManager coordinates single-instance file locking, socket probing, and UDS binding.
type SocketManager struct {
	sockPath string
	lockPath string
	lockFile *os.File
	listener net.Listener
}

// NewSocketManager creates a new SocketManager for the given socket path.
func NewSocketManager(sockPath string) *SocketManager {
	if sockPath == "" {
		sockPath = GetDefaultSocketPath()
	}
	return &SocketManager{
		sockPath: sockPath,
		lockPath: sockPath + ".lock",
	}
}

// SocketPath returns the configured socket file path.
func (m *SocketManager) SocketPath() string {
	return m.sockPath
}

// Listener returns the active network listener once bound.
func (m *SocketManager) Listener() net.Listener {
	return m.listener
}

// AcquireLockAndBind guarantees single-instance daemon ownership and binds the socket.
func (m *SocketManager) AcquireLockAndBind() (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(m.sockPath), 0700); err != nil {
		return nil, fmt.Errorf("failed to create socket directory: %w", err)
	}

	// 1. Open and lock companion .lock file using non-blocking advisory file lock
	f, err := os.OpenFile(m.lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("failed to open lockfile %s: %w", m.lockPath, err)
	}
	m.lockFile = f

	if err := acquireFileLock(f); err != nil {
		_ = f.Close()
		m.lockFile = nil
		return nil, fmt.Errorf("another daemon instance is actively running (lock acquired on %s): %w", m.lockPath, err)
	}

	// 2. Probe if socket file already exists
	if _, err := os.Stat(m.sockPath); err == nil {
		conn, dialErr := net.DialTimeout("unix", m.sockPath, 100*time.Millisecond)
		if dialErr == nil {
			_ = conn.Close()
			m.releaseLock()
			return nil, fmt.Errorf("active daemon responds on socket %s; aborting startup", m.sockPath)
		}
		// Previous daemon terminated ungracefully leaving a stale socket file
		_ = os.Remove(m.sockPath)
	}

	// 3. Bind the Unix Domain Socket listener
	listener, err := net.Listen("unix", m.sockPath)
	if err != nil {
		m.releaseLock()
		return nil, fmt.Errorf("failed to bind socket listener on %s: %w", m.sockPath, err)
	}
	m.listener = listener

	// 4. Enforce strict permissions (owner read/write only)
	if runtime.GOOS != "windows" {
		_ = os.Chmod(m.sockPath, 0600)
	}

	return listener, nil
}

func (m *SocketManager) releaseLock() {
	if m.lockFile != nil {
		_ = releaseFileLock(m.lockFile)
		_ = m.lockFile.Close()
		m.lockFile = nil
		_ = os.Remove(m.lockPath)
	}
}

// Close unlinks the socket file, releases the advisory file lock, and closes the listener.
func (m *SocketManager) Close() error {
	var firstErr error
	if m.listener != nil {
		if err := m.listener.Close(); err != nil {
			firstErr = err
		}
		m.listener = nil
	}
	if m.sockPath != "" {
		_ = os.Remove(m.sockPath)
	}
	m.releaseLock()
	return firstErr
}
