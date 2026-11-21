package mcp

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hoangndst/buddy-agent/internal/tools/subprocess"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RollingBuffer implements a thread-safe, bounded in-memory buffer
// capturing standard error output up to a maximum capacity.
type RollingBuffer struct {
	mu       sync.Mutex
	capacity int
	buf      []byte
}

func newRollingBuffer(capacity int) *RollingBuffer {
	if capacity <= 0 {
		capacity = 4096
	}
	return &RollingBuffer{capacity: capacity}
}

func (b *RollingBuffer) Write(p []byte) (n int, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.capacity {
		b.buf = b.buf[len(b.buf)-b.capacity:]
	}
	return len(p), nil
}

func (b *RollingBuffer) String() string {
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

// isExecutable verifies that the file exists, is not a directory,
// and has executable permissions.
func isExecutable(file string) bool {
	info, err := os.Stat(file)
	if err != nil {
		return false
	}
	if info.IsDir() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode().Perm()&0111 != 0
}

// lookPathIn searches for file in the directories specified by pathList.
// On success, it resolves and returns the absolute executable path.
func lookPathIn(file string, pathList string) (string, error) {
	if strings.ContainsRune(file, filepath.Separator) || strings.ContainsRune(file, '/') {
		if isExecutable(file) {
			absPath, err := filepath.Abs(file)
			if err != nil {
				return file, nil
			}
			return absPath, nil
		}
		return "", exec.ErrNotFound
	}

	dirs := filepath.SplitList(pathList)
	for _, dir := range dirs {
		if dir == "" {
			dir = "."
		}
		target := filepath.Join(dir, file)
		if runtime.GOOS == "windows" && filepath.Ext(file) == "" {
			for _, ext := range []string{".exe", ".cmd", ".bat", ".com"} {
				candidate := target + ext
				if isExecutable(candidate) {
					absPath, err := filepath.Abs(candidate)
					if err != nil {
						return candidate, nil
					}
					return absPath, nil
				}
			}
		}
		if isExecutable(target) {
			absPath, err := filepath.Abs(target)
			if err != nil {
				return target, nil
			}
			return absPath, nil
		}
	}
	return "", exec.ErrNotFound
}

func absolutizePathList(pathList, baseDir string) (string, error) {
	if baseDir == "" {
		var err error
		baseDir, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	baseDir, err := filepath.Abs(baseDir)
	if err != nil {
		return "", err
	}

	entries := filepath.SplitList(pathList)
	for i, entry := range entries {
		if entry == "" {
			entry = "."
		}
		if !filepath.IsAbs(entry) {
			entry = filepath.Join(baseDir, entry)
		}
		entries[i] = filepath.Clean(entry)
	}
	return strings.Join(entries, string(filepath.ListSeparator)), nil
}

// buildStdioCommand prepares an exec.Cmd with environment merging,
// process group supervisor preparation, and rolling stderr buffer attachment.
func buildStdioCommand(srv ServerConfig, baseEnv ...[]string) (*exec.Cmd, subprocess.Supervisor, *RollingBuffer, error) {
	if srv.Command == "" {
		return nil, nil, nil, fmt.Errorf("stdio server requires a command: %w", ErrMissingCommandOrURL)
	}

	var initialEnv []string
	if len(baseEnv) > 0 && baseEnv[0] != nil {
		initialEnv = baseEnv[0]
	} else {
		initialEnv = os.Environ()
	}

	// Merge environment: start with base environment, override with srv.Env
	envMap := make(map[string]string)
	for _, envStr := range initialEnv {
		k, v, ok := strings.Cut(envStr, "=")
		if ok {
			envMap[k] = v
		}
	}
	for k, v := range srv.Env {
		if k == "PATH" || runtime.GOOS == "windows" && strings.EqualFold(k, "PATH") {
			pathBaseDir := srv.WorkDir
			if pathBaseDir == "" {
				pathBaseDir = srv.Cwd
			}
			absolutePath, err := absolutizePathList(v, pathBaseDir)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("failed to resolve MCP server PATH: %w", err)
			}
			v = absolutePath
		}
		envMap[k] = v
	}

	resolvedPath := srv.Command
	if !strings.ContainsRune(srv.Command, filepath.Separator) && !strings.ContainsRune(srv.Command, '/') {
		pathVal, ok := envMap["PATH"]
		if !ok && runtime.GOOS == "windows" {
			for k, v := range envMap {
				if strings.EqualFold(k, "PATH") {
					pathVal = v
					ok = true
					break
				}
			}
		}
		if !ok || pathVal == "" {
			return nil, nil, nil, fmt.Errorf("cannot resolve MCP command %q: PATH is not set in the effective session environment", srv.Command)
		}
		lp, err := lookPathIn(srv.Command, pathVal)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("cannot resolve MCP command %q in the effective session PATH: %w", srv.Command, err)
		}
		resolvedPath = lp
	}

	cmd := exec.Command(resolvedPath, srv.Args...)
	if srv.WorkDir != "" {
		cmd.Dir = srv.WorkDir
	} else if srv.Cwd != "" {
		cmd.Dir = srv.Cwd
	}

	var mergedEnv []string
	for k, v := range envMap {
		mergedEnv = append(mergedEnv, fmt.Sprintf("%s=%s", k, v))
	}
	sort.Strings(mergedEnv)
	cmd.Env = mergedEnv

	sup := subprocess.NewSupervisor()
	if err := sup.PrepareCmd(cmd); err != nil {
		return nil, nil, nil, fmt.Errorf("failed to prepare command process group: %w", err)
	}

	stderrBuf := newRollingBuffer(4096)
	cmd.Stderr = stderrBuf

	return cmd, sup, stderrBuf, nil
}

// SupervisedCommandTransport wraps mcp.CommandTransport to attach the process group
// supervisor immediately upon child process creation, before the JSON-RPC initialization handshake.
type SupervisedCommandTransport struct {
	inner     *mcp.CommandTransport
	cmd       *exec.Cmd
	sup       subprocess.Supervisor
	stderrBuf *RollingBuffer
}

// NewStdioTransport creates a supervised stdio command transport with optional base environment merging.
// When baseEnv is provided and non-nil, it starts with baseEnv instead of os.Environ(),
// overlaying srv.Env onto it (preserving explicit server-level overrides). If baseEnv
// is not provided or nil, it falls back to os.Environ().
func NewStdioTransport(srv ServerConfig, baseEnv ...[]string) (*SupervisedCommandTransport, error) {
	cmd, sup, stderrBuf, err := buildStdioCommand(srv, baseEnv...)
	if err != nil {
		return nil, err
	}
	return &SupervisedCommandTransport{
		inner: &mcp.CommandTransport{
			Command:           cmd,
			TerminateDuration: 100 * time.Millisecond,
		},
		cmd:       cmd,
		sup:       sup,
		stderrBuf: stderrBuf,
	}, nil
}

// Cmd returns the underlying exec.Cmd for the transport.
func (t *SupervisedCommandTransport) Cmd() *exec.Cmd {
	if t == nil {
		return nil
	}
	return t.cmd
}

// Supervisor returns the process group supervisor.
func (t *SupervisedCommandTransport) Supervisor() subprocess.Supervisor {
	if t == nil {
		return nil
	}
	return t.sup
}

// StderrBuffer returns the bounded in-memory rolling buffer capturing stderr.
func (t *SupervisedCommandTransport) StderrBuffer() *RollingBuffer {
	if t == nil {
		return nil
	}
	return t.stderrBuf
}

// StderrText returns the accumulated standard error output.
func (t *SupervisedCommandTransport) StderrText() string {
	if t == nil || t.stderrBuf == nil {
		return ""
	}
	return t.stderrBuf.String()
}

// Connect starts the child process and immediately attaches the process group supervisor
// before returning the connection for the JSON-RPC initialization handshake.
func (t *SupervisedCommandTransport) Connect(ctx context.Context) (mcp.Connection, error) {
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
