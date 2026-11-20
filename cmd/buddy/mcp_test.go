package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCLI_Cobra_AddListRemoveFlow(t *testing.T) {
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

	ctx := context.Background()

	// 1. List with no servers
	rootCmd := newRootCmd(ctx)
	rootCmd.SetArgs([]string{"mcp", "list"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("mcp list failed: %v", err)
	}

	// 2. Add stdio server with '--'
	rootCmd = newRootCmd(ctx)
	rootCmd.SetArgs([]string{"mcp", "add", "sqlite", "--", "uvx", "mcp-server-sqlite", "--db-path", "./test.db"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("mcp add stdio failed: %v", err)
	}

	// 3. Add remote SSE server with custom headers
	rootCmd = newRootCmd(ctx)
	rootCmd.SetArgs([]string{"mcp", "add", "--url", "https://api.example.com/mcp", "--header", "Authorization: Bearer token123", "remote-api"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("mcp add sse failed: %v", err)
	}

	// 4. List servers
	var outBuf bytes.Buffer
	rootCmd = newRootCmd(ctx)
	rootCmd.SetOut(&outBuf)
	rootCmd.SetArgs([]string{"mcp", "list"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("mcp list failed: %v", err)
	}

	// Verify config file existence
	cfgPath := filepath.Join(tempDir, ".buddy", "mcp.json")
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("expected config file %s to exist: %v", cfgPath, err)
	}

	// 5. Remove stdio server
	rootCmd = newRootCmd(ctx)
	rootCmd.SetArgs([]string{"mcp", "remove", "sqlite"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("mcp remove failed: %v", err)
	}

	// 6. Remove non-existent returns error
	rootCmd = newRootCmd(ctx)
	rootCmd.SetArgs([]string{"mcp", "remove", "sqlite"})
	if err := rootCmd.Execute(); err == nil {
		t.Fatal("expected error removing non-existent server, got nil")
	}

	// 7. Remove remote-api server
	rootCmd = newRootCmd(ctx)
	rootCmd.SetArgs([]string{"mcp", "remove", "remote-api"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("mcp remove remote-api failed: %v", err)
	}
}

func TestCLI_Cobra_ValidationErrors(t *testing.T) {
	ctx := context.Background()

	// Empty add
	rootCmd := newRootCmd(ctx)
	rootCmd.SetArgs([]string{"mcp", "add"})
	if err := rootCmd.Execute(); err == nil {
		t.Fatal("expected error on empty add, got nil")
	}

	// Add sse without url
	rootCmd = newRootCmd(ctx)
	rootCmd.SetArgs([]string{"mcp", "add", "my-srv"})
	if err := rootCmd.Execute(); err == nil {
		t.Fatal("expected error on missing url or command, got nil")
	}

	// Remove without args
	rootCmd = newRootCmd(ctx)
	rootCmd.SetArgs([]string{"mcp", "remove"})
	if err := rootCmd.Execute(); err == nil {
		t.Fatal("expected error on remove without server name, got nil")
	}
}
