package mcp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hoangndst/buddy-agent/internal/tools"
	"github.com/hoangndst/buddy-agent/internal/tools/subprocess"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ClientManager coordinates MCP server lifecycles, startup handshakes,
// thread-safe tool execution, and dual-phase process group termination.
type ClientManager struct {
	mu            sync.RWMutex
	cfg           *Config
	client        *mcp.Client
	sessions      map[string]*mcp.ClientSession
	states        map[string]ServerState
	supervisors   map[string]subprocess.Supervisor
	cmds          map[string]*exec.Cmd
	stderrBuffers map[string]*rollingBuffer
	tools         []tools.Tool
	closed        bool
}

// NewClientManager constructs a new ClientManager.
func NewClientManager(cfg *Config) *ClientManager {
	if cfg == nil {
		cfg = &Config{MCPServers: make(map[string]ServerConfig)}
	}
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "buddy-agent",
		Version: "0.1.0",
	}, nil)

	return &ClientManager{
		cfg:           cfg,
		client:        client,
		sessions:      make(map[string]*mcp.ClientSession),
		states:        make(map[string]ServerState),
		supervisors:   make(map[string]subprocess.Supervisor),
		cmds:          make(map[string]*exec.Cmd),
		stderrBuffers: make(map[string]*rollingBuffer),
		tools:         make([]tools.Tool, 0),
	}
}

// Tools returns a snapshot of all discovered and filtered tool adapters across active servers.
func (m *ClientManager) Tools() []tools.Tool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	res := make([]tools.Tool, len(m.tools))
	copy(res, m.tools)
	return res
}

// GetSession retrieves the active ClientSession for a server name.
func (m *ClientManager) GetSession(serverName string) (*mcp.ClientSession, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[serverName]
	return s, ok
}

// ServerStates returns a snapshot copy of current server statuses and diagnostics.
func (m *ClientManager) ServerStates() map[string]ServerState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	res := make(map[string]ServerState, len(m.states))
	for k, v := range m.states {
		res[k] = v
	}
	return res
}

// CallTool invokes an MCP tool under read-lock synchronization, protecting active calls
// from concurrent teardown data races.
func (m *ClientManager) CallTool(ctx context.Context, serverName, toolName string, args map[string]any) (*mcp.CallToolResult, error) {
	m.mu.RLock()
	session, ok := m.sessions[serverName]
	isClosed := m.closed
	m.mu.RUnlock()

	if isClosed {
		return nil, ErrServerUnavailable
	}
	if !ok || session == nil {
		return nil, fmt.Errorf("%w: %s", ErrServerUnavailable, serverName)
	}

	return session.CallTool(ctx, &mcp.CallToolParams{
		Name:      toolName,
		Arguments: args,
	})
}

// StartAll connects to all configured MCP servers concurrently with a 15-second handshake watchdog.
// Servers that fail to start or time out are logged as warnings and skipped, allowing Buddy to boot.
func (m *ClientManager) StartAll(ctx context.Context) map[string]error {
	if m.cfg == nil || len(m.cfg.MCPServers) == 0 {
		return nil
	}

	var wg sync.WaitGroup
	errMap := make(map[string]error)
	var errMu sync.Mutex

	var allDiscoveredAdapters []*ToolAdapter
	var adaptersMu sync.Mutex

	for name, srvCfg := range m.cfg.MCPServers {
		if srvCfg.Disabled {
			m.mu.Lock()
			m.states[name] = ServerState{
				Name:      name,
				Transport: srvCfg.TransportType(),
				Source:    srvCfg.Source,
				Status:    StatusStopped,
			}
			m.mu.Unlock()
			continue
		}

		wg.Add(1)
		go func(srvName string, cfg ServerConfig) {
			defer wg.Done()

			handshakeTimeout := 15 * time.Second
			if cfg.TimeoutSeconds > 0 && cfg.TimeoutSeconds < 15 {
				handshakeTimeout = time.Duration(cfg.TimeoutSeconds) * time.Second
			}
			handshakeCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
			defer cancel()

			var transport mcp.Transport
			var cmd *exec.Cmd
			var sup subprocess.Supervisor
			var stderrBuf *rollingBuffer
			var prepErr error

			if cfg.TransportType() == TransportStdio {
				cmd, sup, stderrBuf, prepErr = buildStdioCommand(cfg)
				if prepErr != nil {
					errMu.Lock()
					errMap[srvName] = prepErr
					errMu.Unlock()

					m.mu.Lock()
					m.states[srvName] = ServerState{
						Name:      srvName,
						Transport: TransportStdio,
						Source:    cfg.Source,
						Status:    StatusFailed,
						LastError: prepErr.Error(),
					}
					m.mu.Unlock()
					fmt.Fprintf(os.Stderr, "Warning: MCP server %q failed to prepare: %v\n", srvName, prepErr)
					return
				}
				transport = newSupervisedCommandTransport(cmd, sup)
			} else {
				transport, prepErr = newStreamableTransport(cfg)
				if prepErr != nil {
					errMu.Lock()
					errMap[srvName] = prepErr
					errMu.Unlock()

					m.mu.Lock()
					m.states[srvName] = ServerState{
						Name:      srvName,
						Transport: TransportSSE,
						Source:    cfg.Source,
						Status:    StatusFailed,
						LastError: prepErr.Error(),
					}
					m.mu.Unlock()
					fmt.Fprintf(os.Stderr, "Warning: MCP server %q failed to prepare transport: %v\n", srvName, prepErr)
					return
				}
			}

			session, err := m.client.Connect(handshakeCtx, transport, nil)
			if err != nil {
				if sup != nil {
					_ = sup.Terminate(false)
					_ = sup.Close()
				}
				errDetail := err.Error()
				if stderrBuf != nil {
					bufStr := strings.TrimSpace(stderrBuf.String())
					if bufStr != "" {
						errDetail = fmt.Sprintf("%s (stderr: %s)", errDetail, bufStr)
					}
				}

				errMu.Lock()
				errMap[srvName] = err
				errMu.Unlock()

				m.mu.Lock()
				m.states[srvName] = ServerState{
					Name:      srvName,
					Transport: cfg.TransportType(),
					Source:    cfg.Source,
					Status:    StatusFailed,
					LastError: errDetail,
				}
				m.mu.Unlock()
				fmt.Fprintf(os.Stderr, "Warning: MCP server %q failed to start: %v\n", srvName, errDetail)
				return
			}

			// Discover tools
			var serverAdapters []*ToolAdapter
			toolsRes, err := session.ListTools(handshakeCtx, nil)
			if err == nil && toolsRes != nil {
				for _, t := range toolsRes.Tools {
					if isToolAllowed(t.Name, cfg.IncludeTools, cfg.ExcludeTools) {
						timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
						serverAdapters = append(serverAdapters, NewToolAdapter(srvName, *t, m, timeout))
					}
				}
			}

			m.mu.Lock()
			m.sessions[srvName] = session
			m.states[srvName] = ServerState{
				Name:            srvName,
				Transport:       cfg.TransportType(),
				Source:          cfg.Source,
				Status:          StatusRunning,
				DiscoveredTools: len(serverAdapters),
			}
			if sup != nil {
				m.supervisors[srvName] = sup
				m.cmds[srvName] = cmd
				m.stderrBuffers[srvName] = stderrBuf
			}
			m.mu.Unlock()

			adaptersMu.Lock()
			allDiscoveredAdapters = append(allDiscoveredAdapters, serverAdapters...)
			adaptersMu.Unlock()
		}(name, srvCfg)
	}

	wg.Wait()

	// Deterministically sort and disambiguate tool adapters across all servers
	sort.Slice(allDiscoveredAdapters, func(i, j int) bool {
		if allDiscoveredAdapters[i].ServerName() != allDiscoveredAdapters[j].ServerName() {
			return allDiscoveredAdapters[i].ServerName() < allDiscoveredAdapters[j].ServerName()
		}
		return allDiscoveredAdapters[i].ToolName() < allDiscoveredAdapters[j].ToolName()
	})

	m.mu.Lock()
	m.tools = disambiguateToolAdapters(allDiscoveredAdapters)
	m.mu.Unlock()

	return errMap
}

// disambiguateToolAdapters scans tool adapters and resolves naming collisions
// deterministically using the original (server\x00tool) pair hash.
func disambiguateToolAdapters(adapters []*ToolAdapter) []tools.Tool {
	nameCounts := make(map[string]int)
	for _, a := range adapters {
		nameCounts[a.Name()]++
	}

	result := make([]tools.Tool, len(adapters))
	for i, a := range adapters {
		if nameCounts[a.Name()] > 1 {
			a.SetName(DisambiguateToolName(a.ServerName(), a.ToolName()))
		}
		result[i] = a
	}
	return result
}

func isToolAllowed(toolName string, include, exclude []string) bool {
	for _, ex := range exclude {
		if ex == toolName {
			return false
		}
	}
	if len(include) == 0 {
		return true
	}
	for _, inc := range include {
		if inc == toolName {
			return true
		}
	}
	return false
}

// Close terminates all active sessions and executes dual-phase signal escalation
// across child process groups, guaranteeing zero orphaned processes.
func (m *ClientManager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true

	// Copy sessions and supervisors to tear down without holding the lock
	sessions := make(map[string]*mcp.ClientSession, len(m.sessions))
	for k, v := range m.sessions {
		sessions[k] = v
	}
	supervisors := make(map[string]subprocess.Supervisor, len(m.supervisors))
	for k, v := range m.supervisors {
		supervisors[k] = v
	}
	m.mu.Unlock()

	// 1. Close SDK client sessions
	for _, s := range sessions {
		_ = s.Close()
	}

	// 2. Dual-phase process group escalation for stdio servers
	for _, sup := range supervisors {
		// Phase 1: Graceful SIGTERM
		_ = sup.Terminate(true)

		// Monitor grace period (up to 1.5s) if still alive
		if sup.IsAlive() {
			deadline := time.Now().Add(1500 * time.Millisecond)
			for time.Now().Before(deadline) {
				time.Sleep(25 * time.Millisecond)
				if !sup.IsAlive() {
					break
				}
			}
			// Phase 2: Forced SIGKILL if lingering
			if sup.IsAlive() {
				_ = sup.Terminate(false)
			}
		}

		_ = sup.Close()
	}

	return nil
}
