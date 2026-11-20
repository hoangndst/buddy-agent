package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	"github.com/spf13/cobra"
)

func TestMCP_AddListRemoveFlow(t *testing.T) {
	tempDir := t.TempDir()
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get wd: %v", err)
	}
	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("failed to chdir: %v", err)
	}
	defer func() {
		_ = os.Chdir(origWd)
	}()

	f := cmdutil.NewFactory()
	ioStreams, _, outBuf, _ := cmdutil.TestIOStreams()
	f.IOStreams = ioStreams

	// 1. List with no servers
	rootCmd := &cobra.Command{Use: "buddy"}
	rootCmd.AddCommand(NewCmdMCP(f))
	rootCmd.SetArgs([]string{"mcp", "list"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("mcp list failed: %v", err)
	}
	if !strings.Contains(outBuf.String(), "No MCP servers configured") {
		t.Errorf("expected empty message, got: %s", outBuf.String())
	}

	// 2. Add stdio server
	outBuf.Reset()
	rootCmd = &cobra.Command{Use: "buddy"}
	rootCmd.AddCommand(NewCmdMCP(f))
	rootCmd.SetArgs([]string{"mcp", "add", "sqlite", "--", "uvx", "mcp-server-sqlite", "--db-path", "./test.db"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("mcp add stdio failed: %v", err)
	}
	if !strings.Contains(outBuf.String(), "Added MCP stdio server \"sqlite\"") {
		t.Errorf("expected added message, got: %s", outBuf.String())
	}

	// 3. Add remote SSE server with custom headers
	outBuf.Reset()
	rootCmd = &cobra.Command{Use: "buddy"}
	rootCmd.AddCommand(NewCmdMCP(f))
	rootCmd.SetArgs([]string{"mcp", "add", "--url", "https://api.example.com/mcp", "--header", "Authorization: Bearer token123", "remote-api"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("mcp add sse failed: %v", err)
	}
	if !strings.Contains(outBuf.String(), "Added MCP remote SSE server \"remote-api\"") {
		t.Errorf("expected added message, got: %s", outBuf.String())
	}

	// 4. List servers
	outBuf.Reset()
	rootCmd = &cobra.Command{Use: "buddy"}
	rootCmd.AddCommand(NewCmdMCP(f))
	rootCmd.SetArgs([]string{"mcp", "list"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("mcp list failed: %v", err)
	}
	if !strings.Contains(outBuf.String(), "sqlite") || !strings.Contains(outBuf.String(), "remote-api") {
		t.Errorf("expected servers in list output, got: %s", outBuf.String())
	}

	// Verify config file existence
	cfgPath := filepath.Join(tempDir, ".buddy", "mcp.json")
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("expected config file %s to exist: %v", cfgPath, err)
	}

	// 5. Remove stdio server
	outBuf.Reset()
	rootCmd = &cobra.Command{Use: "buddy"}
	rootCmd.AddCommand(NewCmdMCP(f))
	rootCmd.SetArgs([]string{"mcp", "remove", "sqlite"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("mcp remove failed: %v", err)
	}
	if !strings.Contains(outBuf.String(), "Removed MCP server \"sqlite\"") {
		t.Errorf("expected removed message, got: %s", outBuf.String())
	}

	// 6. Remove non-existent returns error
	rootCmd = &cobra.Command{Use: "buddy"}
	rootCmd.AddCommand(NewCmdMCP(f))
	rootCmd.SetArgs([]string{"mcp", "remove", "sqlite"})
	if err := rootCmd.Execute(); err == nil {
		t.Fatal("expected error removing non-existent server, got nil")
	}

	// 7. Remove remote-api server
	rootCmd = &cobra.Command{Use: "buddy"}
	rootCmd.AddCommand(NewCmdMCP(f))
	rootCmd.SetArgs([]string{"mcp", "remove", "remote-api"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("mcp remove remote-api failed: %v", err)
	}
}

func TestMCP_ValidationErrors(t *testing.T) {
	f := cmdutil.NewFactory()
	ioStreams, _, _, _ := cmdutil.TestIOStreams()
	f.IOStreams = ioStreams

	// Empty add
	rootCmd := &cobra.Command{Use: "buddy"}
	rootCmd.AddCommand(NewCmdMCP(f))
	rootCmd.SetArgs([]string{"mcp", "add"})
	if err := rootCmd.Execute(); err == nil {
		t.Fatal("expected error on empty add, got nil")
	}

	// Add sse without url
	rootCmd = &cobra.Command{Use: "buddy"}
	rootCmd.AddCommand(NewCmdMCP(f))
	rootCmd.SetArgs([]string{"mcp", "add", "my-srv"})
	if err := rootCmd.Execute(); err == nil {
		t.Fatal("expected error on missing url or command, got nil")
	}

	// Remove without args
	rootCmd = &cobra.Command{Use: "buddy"}
	rootCmd.AddCommand(NewCmdMCP(f))
	rootCmd.SetArgs([]string{"mcp", "remove"})
	if err := rootCmd.Execute(); err == nil {
		t.Fatal("expected error on remove without server name, got nil")
	}
}

func TestMCP_MockGlobalConfig(t *testing.T) {
	tempHome := t.TempDir()
	tempProject := t.TempDir()

	f := cmdutil.NewFactory()
	f.UserHome = func() (string, error) { return tempHome, nil }
	f.WorkDir = func() (string, error) { return tempProject, nil }
	ioStreams, _, outBuf, _ := cmdutil.TestIOStreams()
	f.IOStreams = ioStreams

	rootCmd := &cobra.Command{Use: "buddy"}
	rootCmd.AddCommand(NewCmdMCP(f))
	rootCmd.SetArgs([]string{"mcp", "add", "global-tool", "--global", "--", "my-binary"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("mcp add --global failed: %v", err)
	}
	if !strings.Contains(outBuf.String(), "global config") {
		t.Errorf("expected global config confirmation, got: %s", outBuf.String())
	}

	globalCfgPath := filepath.Join(tempHome, ".buddy", "mcp.json")
	if _, err := os.Stat(globalCfgPath); err != nil {
		t.Errorf("expected global config file to exist at %s", globalCfgPath)
	}
}
