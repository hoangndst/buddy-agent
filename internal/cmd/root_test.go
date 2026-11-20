package cmd

import (
	"strings"
	"testing"

	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	"github.com/hoangndst/buddy-agent/internal/config"
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
}

func TestRootCmd_REPLExit(t *testing.T) {
	f := cmdutil.NewFactory()
	ioStreams, inBuf, outBuf, _ := cmdutil.TestIOStreams()
	f.IOStreams = ioStreams

	// Mock config
	f.ConfigLoader = func() (*config.Config, error) {
		return &config.Config{
			APIKey:          "test-key",
			Model:           "test-model",
			WorkspaceDir:    t.TempDir(),
			MaxOutputChars:  1000,
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
