package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/hoangndst/buddy-agent/internal/agent"
	"github.com/hoangndst/buddy-agent/internal/agent/provider"
	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	"github.com/hoangndst/buddy-agent/internal/config"
	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
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
