package mcp

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/hoangndst/buddy-agent/internal/tools/subprocess"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// rollingBuffer implements a thread-safe, bounded in-memory buffer
// capturing standard error output up to a maximum capacity.
type rollingBuffer struct {
	mu       sync.Mutex
	capacity int
	buf      []byte
}

func newRollingBuffer(capacity int) *rollingBuffer {
	if capacity <= 0 {
		capacity = 4096
	}
	return &rollingBuffer{capacity: capacity}
}

func (b *rollingBuffer) Write(p []byte) (n int, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.capacity {
		b.buf = b.buf[len(b.buf)-b.capacity:]
	}
	return len(p), nil
}

func (b *rollingBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// headerRoundTripper injects custom HTTP headers into outgoing transport requests.
type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

func (rt *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	for k, v := range rt.headers {
		r.Header.Set(k, v)
	}
	base := rt.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(r)
}

// buildStdioCommand prepares an exec.Cmd with environment merging,
// process group supervisor preparation, and rolling stderr buffer attachment.
func buildStdioCommand(srv ServerConfig) (*exec.Cmd, subprocess.Supervisor, *rollingBuffer, error) {
	if srv.Command == "" {
		return nil, nil, nil, fmt.Errorf("stdio server requires a command: %w", ErrMissingCommandOrURL)
	}

	cmd := exec.Command(srv.Command, srv.Args...)

	// Merge environment: start with current environment, override with srv.Env
	envMap := make(map[string]string)
	for _, envStr := range os.Environ() {
		for i := 0; i < len(envStr); i++ {
			if envStr[i] == '=' {
				envMap[envStr[:i]] = envStr[i+1:]
				break
			}
		}
	}
	for k, v := range srv.Env {
		envMap[k] = v
	}
	var mergedEnv []string
	for k, v := range envMap {
		mergedEnv = append(mergedEnv, fmt.Sprintf("%s=%s", k, v))
	}
	cmd.Env = mergedEnv

	sup := subprocess.NewSupervisor()
	if err := sup.PrepareCmd(cmd); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to prepare command process group: %w", err)
	}

	stderrBuf := newRollingBuffer(4096)
	cmd.Stderr = stderrBuf

	return cmd, sup, stderrBuf, nil
}

// supervisedCommandTransport wraps mcp.CommandTransport to attach the process group
// supervisor immediately upon child process creation, before the JSON-RPC initialization handshake.
type supervisedCommandTransport struct {
	inner *mcp.CommandTransport
	cmd   *exec.Cmd
	sup   subprocess.Supervisor
}

// newSupervisedCommandTransport creates a supervisedCommandTransport wrapping an SDK CommandTransport.
func newSupervisedCommandTransport(cmd *exec.Cmd, sup subprocess.Supervisor) *supervisedCommandTransport {
	return &supervisedCommandTransport{
		inner: &mcp.CommandTransport{
			Command:           cmd,
			TerminateDuration: 100 * time.Millisecond,
		},
		cmd: cmd,
		sup: sup,
	}
}

// Connect starts the child process and immediately attaches the process group supervisor
// before returning the connection for the JSON-RPC initialization handshake.
func (t *supervisedCommandTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := t.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	if t.cmd != nil && t.cmd.Process != nil && t.sup != nil {
		if attachErr := t.sup.AttachProcess(t.cmd); attachErr != nil {
			if conn != nil {
				_ = conn.Close()
			}
			_ = t.sup.Terminate(false)
			_ = t.sup.Close()
			return nil, fmt.Errorf("attaching process supervisor: %w", attachErr)
		}
	}
	return conn, nil
}

// newStreamableTransport creates an SDK StreamableClientTransport configured with custom HTTP headers.
func newStreamableTransport(srv ServerConfig) (*mcp.StreamableClientTransport, error) {
	if srv.URL == "" {
		return nil, fmt.Errorf("sse server requires a URL: %w", ErrMissingCommandOrURL)
	}

	var httpClient *http.Client
	if len(srv.Headers) > 0 {
		httpClient = &http.Client{
			Transport: &headerRoundTripper{
				base:    http.DefaultTransport,
				headers: srv.Headers,
			},
			Timeout: 30 * time.Second,
		}
	}

	return &mcp.StreamableClientTransport{
		Endpoint:   srv.URL,
		HTTPClient: httpClient,
	}, nil
}
