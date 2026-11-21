package cmd

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hoangndst/buddy-agent/internal/agent"
	"github.com/hoangndst/buddy-agent/internal/agent/provider"
	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	"github.com/hoangndst/buddy-agent/internal/config"
	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
	core "github.com/hoangndst/buddy-agent/internal/mcp"
	"github.com/openai/openai-go"
)

func TestRootCmd_StructureAndSubcommands(t *testing.T) {
	f := cmdutil.NewFactory()
	ioStreams, _, _, _ := cmdutil.TestIOStreams()
	f.IOStreams = ioStreams

	rootCmd := NewRootCmd(f)
	if rootCmd.Use != "buddy [task]" {
		t.Errorf("expected Use 'buddy [task]', got %q", rootCmd.Use)
	}

	// Verify subcommands attached
	subcommands := make(map[string]bool)
	for _, c := range rootCmd.Commands() {
		subcommands[c.Name()] = true
	}

	if !subcommands["mcp"] {
		t.Error("expected 'mcp' subcommand to be attached")
	}
	if !subcommands["doc"] {
		t.Error("expected 'doc' subcommand to be attached")
	}
	if !subcommands["daemon"] {
		t.Error("expected 'daemon' subcommand to be attached")
	}
	if !subcommands["attach"] {
		t.Error("expected 'attach' subcommand to be attached")
	}
}

func TestRootCmd_REPLExit(t *testing.T) {
	f := cmdutil.NewFactory()
	ioStreams, inBuf, outBuf, _ := cmdutil.TestIOStreams()
	f.IOStreams = ioStreams

	// Mock config
	f.ConfigLoader = func() (*config.Config, error) {
		return &config.Config{
			APIKey:           "test-key",
			Model:            "test-model",
			WorkspaceDir:     t.TempDir(),
			MaxOutputChars:   1000,
			MaxContextTokens: 4096,
		}, nil
	}

	// Send "exit" to REPL input stream
	inBuf.WriteString("exit\n")

	rootCmd := NewRootCmd(f)
	rootCmd.SetArgs([]string{})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("REPL execution failed: %v", err)
	}

	if !strings.Contains(outBuf.String(), "=== buddy-agent CLI ready ===") {
		t.Errorf("expected ready message, got: %s", outBuf.String())
	}
}

func TestFormatToolPreview(t *testing.T) {
	tests := []struct {
		name     string
		toolName string
		rawArgs  string
		expected string
	}{
		{
			name:     "bash tool",
			toolName: "bash",
			rawArgs:  `{"command":"ls -la"}`,
			expected: "⚙ bash: ls -la",
		},
		{
			name:     "read_file tool",
			toolName: "read_file",
			rawArgs:  `{"path":"main.go"}`,
			expected: "⚙ read_file: main.go",
		},
		{
			name:     "write_file tool",
			toolName: "write_file",
			rawArgs:  `{"path":"config.json","content":"{}"}`,
			expected: "⚙ write_file: config.json",
		},
		{
			name:     "edit_file tool",
			toolName: "edit_file",
			rawArgs:  `{"path":"internal/agent.go","target":"foo","replacement":"bar"}`,
			expected: "⚙ edit_file: internal/agent.go",
		},
		{
			name:     "save_memory tool",
			toolName: "save_memory",
			rawArgs:  `{"topic":"architecture","content":"use react loops"}`,
			expected: "⚙ save_memory: architecture",
		},
		{
			name:     "create_skill tool",
			toolName: "create_skill",
			rawArgs:  `{"name":"linting","description":"run linter"}`,
			expected: "⚙ create_skill: linting",
		},
		{
			name:     "mcp tool primary key",
			toolName: "search_docs",
			rawArgs:  `{"query":"how to build"}`,
			expected: "⚙ search_docs: how to build",
		},
		{
			name:     "generic tool unparseable fallback",
			toolName: "custom_op",
			rawArgs:  `raw-string-args`,
			expected: "⚙ custom_op(raw-string-args)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatToolPreview(tt.toolName, tt.rawArgs)
			if got != tt.expected {
				t.Errorf("formatToolPreview(%q, %q) = %q, want %q", tt.toolName, tt.rawArgs, got, tt.expected)
			}
		})
	}
}

type mockCLIProvider struct {
	turn int
}

func (m *mockCLIProvider) Protocol() string {
	return "chat"
}

func (m *mockCLIProvider) Generate(_ context.Context, _ provider.GenerateRequest) (*provider.GenerateResponse, error) {
	m.turn++
	if m.turn == 1 {
		return &provider.GenerateResponse{
			ReasoningContent: "Planning to execute directory list",
			ToolCalls: []provider.ToolCall{
				{
					ID:        "call_1",
					Name:      "bash",
					Arguments: `{"command":"echo hello"}`,
				},
			},
			Protocol: "chat",
		}, nil
	}
	return &provider.GenerateResponse{
		Content:  "Directory listing complete! Everything looks good.",
		Protocol: "chat",
	}, nil
}

func TestRootCmd_TaskExecution_FormattingAndNoRawStepHeaders(t *testing.T) {
	f := cmdutil.NewFactory()
	ioStreams, _, outBuf, _ := cmdutil.TestIOStreams()
	f.IOStreams = ioStreams

	f.ConfigLoader = func() (*config.Config, error) {
		return &config.Config{
			APIKey:           "test-key",
			Model:            "test-model",
			WorkspaceDir:     t.TempDir(),
			MaxOutputChars:   1000,
			MaxContextTokens: 4096,
		}, nil
	}

	f.NewModelProvider = func(_ *config.Config, _ *openai.Client) provider.ModelProvider {
		return &mockCLIProvider{}
	}

	rootCmd := NewRootCmd(f)
	rootCmd.SetArgs([]string{"--standalone", "list files"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("task execution failed: %v", err)
	}

	out := outBuf.String()

	// Invariant: Raw step banners like [Step 1] must NOT be present
	if strings.Contains(out, "[Step 1]") {
		t.Errorf("output should not contain raw step banners [Step 1], got:\n%s", out)
	}

	// Reasoning visibility
	if !strings.Contains(out, "💭 Thinking:") || !strings.Contains(out, "Planning to execute directory list") {
		t.Errorf("expected styled thinking block in output, got:\n%s", out)
	}

	// Formatted tool preview
	if !strings.Contains(out, "⚙ bash: echo hello") {
		t.Errorf("expected formatted tool preview in output, got:\n%s", out)
	}

	// Conversational text complete
	if !strings.Contains(out, "Directory listing complete! Everything looks good.") {
		t.Errorf("expected final conversational text in output, got:\n%s", out)
	}

	// Done banner
	if !strings.Contains(out, "✓ Done") {
		t.Errorf("expected Done banner in output, got:\n%s", out)
	}
}

func TestRenderEvent_ToolResultDelegatedOutput(t *testing.T) {
	f := cmdutil.NewFactory()
	ioStreams, _, outBuf, _ := cmdutil.TestIOStreams()
	f.IOStreams = ioStreams

	evt := protocol.SessionEventNotification{
		Type:    string(agent.EventToolResult),
		Content: "  [subagent:researcher] ",
		Data: map[string]any{
			"tool_result": "found 4 matches in src/main.go",
		},
	}
	renderEvent(f, evt)

	out := outBuf.String()
	if !strings.Contains(out, "found 4 matches in src/main.go") {
		t.Errorf("expected output to contain delegated tool result, got:\n%s", out)
	}
	if !strings.Contains(out, "[subagent:researcher]") {
		t.Errorf("expected output to preserve subagent label, got:\n%s", out)
	}
}

func TestRenderEvent_TextComplete(t *testing.T) {
	f := cmdutil.NewFactory()
	ioStreams, _, outBuf, _ := cmdutil.TestIOStreams()
	f.IOStreams = ioStreams

	evt := protocol.SessionEventNotification{
		Type:    string(agent.EventTextComplete),
		Content: "I have prepared the changes and will now execute the build.",
	}
	renderEvent(f, evt)

	out := outBuf.String()
	if !strings.Contains(out, "I have prepared the changes and will now execute the build.") {
		t.Errorf("expected conversational text in output, got:\n%s", out)
	}
}

func TestSetupEngine_PassesCallerEnvToMCPManager(t *testing.T) {
	f := cmdutil.NewFactory()
	tempDir := t.TempDir()

	f.ConfigLoader = func() (*config.Config, error) {
		return &config.Config{
			APIKey:           "test-key",
			Model:            "test-model",
			WorkspaceDir:     tempDir,
			MaxOutputChars:   1000,
			MaxContextTokens: 4096,
		}, nil
	}

	callerEnv := []string{
		"BUDDY_CLI_TOKEN=custom_cli_token_999",
		"BUDDY_CLI_SETTING=1",
	}
	f.Environ = func() []string {
		return callerEnv
	}

	f.MCPConfigLoader = func(_, _ string) (*core.Config, error) {
		return &core.Config{
			MCPServers: map[string]core.ServerConfig{
				"test-mcp-server": {Command: "echo"},
			},
		}, nil
	}

	var observedBaseEnv []string
	f.NewClientManager = func(cfg *core.Config, baseEnv ...[]string) *core.ClientManager {
		if len(baseEnv) > 0 {
			observedBaseEnv = baseEnv[0]
		}
		return core.NewClientManager(cfg, baseEnv...)
	}

	eng, cleanup, err := setupEngine(context.Background(), f)
	if err != nil {
		t.Fatalf("setupEngine failed: %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	if eng == nil {
		t.Fatal("expected non-nil engine")
	}

	if len(observedBaseEnv) != len(callerEnv) {
		t.Fatalf("expected observedBaseEnv length %d, got %d", len(callerEnv), len(observedBaseEnv))
	}
	for i, val := range callerEnv {
		if observedBaseEnv[i] != val {
			t.Errorf("expected observedBaseEnv[%d] = %q, got %q", i, val, observedBaseEnv[i])
		}
	}
}

func TestRunTask_ForwardsParamsAndPreservesSpecialDB(t *testing.T) {
	tempDir := t.TempDir()

	tests := []struct {
		name               string
		configuredDB       string
		expectedDB         string
		configuredMaxChars int
		expectedMaxChars   int
	}{
		{
			name:               "exact memory special name preserved",
			configuredDB:       ":memory:",
			expectedDB:         ":memory:",
			configuredMaxChars: 25000,
			expectedMaxChars:   25000,
		},
		{
			name:               "file URI in-memory preserved",
			configuredDB:       "file:memdb?mode=memory&cache=shared",
			expectedDB:         "file:memdb?mode=memory&cache=shared",
			configuredMaxChars: 30000,
			expectedMaxChars:   30000,
		},
		{
			name:               "relative file path absolutized",
			configuredDB:       "test_history.db",
			expectedDB:         filepath.Join(tempDir, "test_history.db"),
			configuredMaxChars: 15000,
			expectedMaxChars:   15000,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sockDir := t.TempDir()
			sockFile := filepath.Join(sockDir, "d.sock")
			listener, err := net.Listen("unix", sockFile)
			if err != nil {
				t.Fatalf("failed to bind listener: %v", err)
			}
			defer func() { _ = listener.Close() }()

			var receivedParams protocol.SessionStartParams
			serverErrCh := make(chan error, 1)

			go func() {
				conn, err := listener.Accept()
				if err != nil {
					serverErrCh <- err
					return
				}
				defer func() { _ = conn.Close() }()

				codec := protocol.NewStreamCodec(conn, conn)
				msg, err := codec.ReadMessage()
				if err != nil {
					serverErrCh <- err
					return
				}

				if msg.Method == protocol.MethodSessionStart {
					_ = json.Unmarshal(msg.Params, &receivedParams)
					res, _ := protocol.NewResponse(*msg.ID, protocol.SessionStartResult{SessionID: "sess-test"})
					_ = codec.WriteMessage(res)

					// Send finished notification to conclude the stream
					notif, _ := protocol.NewNotification(protocol.NotifySessionEvent, protocol.SessionEventNotification{
						Type: string(agent.EventFinished),
					})
					_ = codec.WriteMessage(notif)
				}
				serverErrCh <- nil
			}()

			f := cmdutil.NewFactory()
			ioStreams, _, _, _ := cmdutil.TestIOStreams()
			f.IOStreams = ioStreams
			f.WorkDir = func() (string, error) {
				return tempDir, nil
			}
			f.Environ = func() []string {
				return []string{"PATH=./node_modules/.bin"}
			}
			f.ConfigLoader = func() (*config.Config, error) {
				return &config.Config{
					APIKey:           "key",
					Model:            "model",
					WorkspaceDir:     tempDir,
					DBPath:           tc.configuredDB,
					MaxOutputChars:   tc.configuredMaxChars,
					WorkingTailSize:  7,
					MaxContextTokens: 4096,
				}, nil
			}

			rootCmd := NewRootCmd(f)
			rootCmd.SetArgs([]string{"--socket", sockFile, "do something"})
			if err := rootCmd.Execute(); err != nil {
				t.Fatalf("rootCmd.Execute failed: %v", err)
			}

			if err := <-serverErrCh; err != nil {
				t.Fatalf("server mock error: %v", err)
			}

			if receivedParams.DBPath != tc.expectedDB {
				t.Errorf("expected DBPath %q, got %q", tc.expectedDB, receivedParams.DBPath)
			}
			if receivedParams.MaxOutputChars != tc.expectedMaxChars {
				t.Errorf("expected MaxOutputChars %d, got %d", tc.expectedMaxChars, receivedParams.MaxOutputChars)
			}
			if receivedParams.WorkingTailSize != 7 {
				t.Errorf("expected WorkingTailSize 7, got %d", receivedParams.WorkingTailSize)
			}
			wantPATH := "PATH=" + filepath.Join(tempDir, "node_modules", ".bin")
			foundPATH := false
			for _, env := range receivedParams.Env {
				if env == wantPATH {
					foundPATH = true
					break
				}
			}
			if !foundPATH {
				t.Errorf("expected caller-relative PATH to be forwarded as %q, got %v", wantPATH, receivedParams.Env)
			}
		})
	}
}
