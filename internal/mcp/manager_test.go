package mcp

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestManager_EmptyConfig(t *testing.T) {
	mgr := NewClientManager(nil)
	ctx := context.Background()

	errs := mgr.StartAll(ctx)
	if len(errs) != 0 {
		t.Errorf("expected 0 errors, got %d", len(errs))
	}
	if len(mgr.Tools()) != 0 {
		t.Errorf("expected 0 tools, got %d", len(mgr.Tools()))
	}
	if err := mgr.Close(); err != nil {
		t.Errorf("expected clean Close(), got %v", err)
	}
}

func TestManager_DisabledServer(t *testing.T) {
	cfg := &Config{
		MCPServers: map[string]ServerConfig{
			"disabled_sqlite": {
				Command:  "uvx",
				Args:     []string{"mcp-server-sqlite"},
				Disabled: true,
			},
		},
	}
	mgr := NewClientManager(cfg)
	ctx := context.Background()

	errs := mgr.StartAll(ctx)
	if len(errs) != 0 {
		t.Errorf("expected 0 errors for disabled server, got %d", len(errs))
	}

	states := mgr.ServerStates()
	state, ok := states["disabled_sqlite"]
	if !ok {
		t.Fatal("expected state for disabled_sqlite")
	}
	if state.Status != StatusStopped {
		t.Errorf("expected status 'stopped', got %s", state.Status)
	}
}

func TestManager_FailingServer_FailureIsolation(t *testing.T) {
	cfg := &Config{
		MCPServers: map[string]ServerConfig{
			"broken_server": {
				Command:        "non-existent-binary-buddy-12345",
				TimeoutSeconds: 2,
			},
		},
	}
	mgr := NewClientManager(cfg)
	ctx := context.Background()

	// Failure isolation: StartAll does not panic and isolates the failure
	errs := mgr.StartAll(ctx)
	if len(errs) != 1 {
		t.Fatalf("expected 1 server error in map, got %d", len(errs))
	}

	states := mgr.ServerStates()
	state, ok := states["broken_server"]
	if !ok {
		t.Fatal("expected state for broken_server")
	}
	if state.Status != StatusFailed {
		t.Errorf("expected status 'failed', got %s", state.Status)
	}
	if state.LastError == "" {
		t.Error("expected non-empty LastError")
	}
}

// TestHelperProcess implements a mock stdio MCP server for testing subprocess lifecycle.
func TestHelperProcess(_ *testing.T) {
	if os.Getenv("GO_WANT_MCP_HELPER") != "1" {
		return
	}

	server := mcp.NewServer(&mcp.Implementation{Name: "mock-server", Version: "0.1.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "ping",
		Description: "Simple ping tool",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: "pong"},
			},
		}, nil, nil
	})

	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestManager_MockStdioServer_FullLifecycle(t *testing.T) {
	// Configure manager pointing to this test binary as helper process
	cfg := &Config{
		MCPServers: map[string]ServerConfig{
			"mock_helper": {
				Command: os.Args[0],
				Args:    []string{"-test.run=TestHelperProcess", "--"},
				Env: map[string]string{
					"GO_WANT_MCP_HELPER": "1",
				},
				TimeoutSeconds: 5,
			},
		},
	}

	mgr := NewClientManager(cfg)
	ctx := context.Background()

	errs := mgr.StartAll(ctx)
	if len(errs) != 0 {
		t.Fatalf("StartAll failed: %v", errs)
	}

	states := mgr.ServerStates()
	state, ok := states["mock_helper"]
	if !ok || state.Status != StatusRunning {
		t.Fatalf("expected mock_helper running, got state: %+v", state)
	}
	if state.DiscoveredTools != 1 {
		t.Errorf("expected 1 discovered tool, got %d", state.DiscoveredTools)
	}

	// Verify tool registration
	discoveredTools := mgr.Tools()
	if len(discoveredTools) != 1 {
		t.Fatalf("expected 1 tool registered, got %d", len(discoveredTools))
	}
	tool := discoveredTools[0]
	if tool.Name() != "mcp_mock_helper_ping" {
		t.Errorf("expected tool name mcp_mock_helper_ping, got %s", tool.Name())
	}

	// Execute tool through manager CallTool
	out, err := tool.Execute(ctx, nil)
	if err != nil {
		t.Fatalf("tool execution returned error: %v", err)
	}
	if !strings.Contains(out, "pong") {
		t.Errorf("expected output containing 'pong', got %q", out)
	}

	// Verify clean teardown
	if err := mgr.Close(); err != nil {
		t.Fatalf("mgr.Close() failed: %v", err)
	}

	// Post-close CallTool returns ErrServerUnavailable
	_, err = mgr.CallTool(ctx, "mock_helper", "ping", nil)
	if err != ErrServerUnavailable {
		t.Errorf("expected ErrServerUnavailable after close, got %v", err)
	}
}

func TestManager_ConcurrentReadWriteAccess(_ *testing.T) {
	mgr := NewClientManager(nil)
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_ = mgr.Tools()
				_ = mgr.ServerStates()
				_, _ = mgr.GetSession("any")
				_, _ = mgr.CallTool(ctx, "any", "tool", nil)
			}
		}()
	}

	time.Sleep(10 * time.Millisecond)
	_ = mgr.Close()
	wg.Wait()
}

// TestHangingHelperProcess simulates a stdio server that hangs on initialization.
func TestHangingHelperProcess(_ *testing.T) {
	if os.Getenv("GO_WANT_HANGING_MCP_HELPER") != "1" {
		return
	}
	time.Sleep(10 * time.Second)
	os.Exit(0)
}

func TestManager_SupervisedCleanupOnHandshakeFailure(t *testing.T) {
	cfg := &Config{
		MCPServers: map[string]ServerConfig{
			"hanging_helper": {
				Command: os.Args[0],
				Args:    []string{"-test.run=TestHangingHelperProcess", "--"},
				Env: map[string]string{
					"GO_WANT_HANGING_MCP_HELPER": "1",
				},
				TimeoutSeconds: 1,
			},
		},
	}

	mgr := NewClientManager(cfg)
	ctx := context.Background()

	start := time.Now()
	errs := mgr.StartAll(ctx)
	elapsed := time.Since(start)

	if len(errs) != 1 {
		t.Fatalf("expected 1 error for hanging server, got %d", len(errs))
	}
	if elapsed > 4*time.Second {
		t.Errorf("expected fast timeout and termination, took %v", elapsed)
	}

	states := mgr.ServerStates()
	if states["hanging_helper"].Status != StatusFailed {
		t.Errorf("expected failed status, got %s", states["hanging_helper"].Status)
	}

	_ = mgr.Close()
}

func TestManager_DisambiguateToolCollisions(t *testing.T) {
	mgr := NewClientManager(nil)
	adapters := []*ToolAdapter{
		NewToolAdapter("a_b", mcp.Tool{Name: "c"}, mgr, 5*time.Second),
		NewToolAdapter("a", mcp.Tool{Name: "b_c"}, mgr, 5*time.Second),
	}

	disambiguated := disambiguateToolAdapters(adapters)
	if len(disambiguated) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(disambiguated))
	}

	name1 := disambiguated[0].Name()
	name2 := disambiguated[1].Name()

	if name1 == name2 {
		t.Fatalf("collision detected! Both tools have identical name %q", name1)
	}
	if len(name1) > 64 || len(name2) > 64 {
		t.Fatalf("tool names must not exceed 64 runes: len1=%d, len2=%d", len(name1), len(name2))
	}
}

func TestClientManager_BaseEnv(t *testing.T) {
	t.Run("NilBaseEnv", func(t *testing.T) {
		mgr := NewClientManager(nil)
		if mgr.BaseEnv() != nil {
			t.Errorf("expected nil BaseEnv, got %+v", mgr.BaseEnv())
		}
	})

	t.Run("ProvidedBaseEnv", func(t *testing.T) {
		env := []string{"FOO=BAR", "BAZ=QUX"}
		mgr := NewClientManager(nil, env)
		got := mgr.BaseEnv()
		if len(got) != 2 || got[0] != "FOO=BAR" || got[1] != "BAZ=QUX" {
			t.Errorf("expected %+v, got %+v", env, got)
		}
		// Ensure copy
		got[0] = "MODIFIED"
		if mgr.BaseEnv()[0] != "FOO=BAR" {
			t.Error("BaseEnv() should return an isolated copy")
		}
	})
}
