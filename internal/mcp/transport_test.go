package mcp

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testExecutable(t *testing.T) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable failed: %v", err)
	}
	return executable
}

func TestStdioTransport_BaseEnvironment(t *testing.T) {
	t.Run("CustomBaseEnv_WithOverrides", func(t *testing.T) {
		baseEnv := []string{
			"CALLER_VAR=caller_value",
			"SHARED_VAR=original_value",
			"PATH=/custom/bin",
		}
		srv := ServerConfig{
			Command: testExecutable(t),
			Args:    []string{"hello"},
			Env: map[string]string{
				"SHARED_VAR": "server_override",
				"SERVER_VAR": "server_val",
			},
		}

		transport, err := NewStdioTransport(srv, baseEnv)
		if err != nil {
			t.Fatalf("NewStdioTransport failed: %v", err)
		}
		if transport == nil {
			t.Fatal("expected non-nil transport")
		}

		cmd := transport.Cmd()
		if cmd == nil {
			t.Fatal("expected non-nil cmd")
		}

		envMap := make(map[string]string)
		for _, e := range cmd.Env {
			k, v, ok := strings.Cut(e, "=")
			if ok {
				envMap[k] = v
			}
		}

		if envMap["CALLER_VAR"] != "caller_value" {
			t.Errorf("expected CALLER_VAR='caller_value', got %q", envMap["CALLER_VAR"])
		}
		if envMap["PATH"] != "/custom/bin" {
			t.Errorf("expected PATH='/custom/bin', got %q", envMap["PATH"])
		}
		if envMap["SHARED_VAR"] != "server_override" {
			t.Errorf("expected SHARED_VAR='server_override' (srv.Env override), got %q", envMap["SHARED_VAR"])
		}
		if envMap["SERVER_VAR"] != "server_val" {
			t.Errorf("expected SERVER_VAR='server_val', got %q", envMap["SERVER_VAR"])
		}

		// Ensure daemon's os.Environ was not inherited when explicit baseEnv is provided
		if len(envMap) != 4 {
			t.Errorf("expected exactly 4 environment variables, got %d: %+v", len(envMap), envMap)
		}
	})

	t.Run("NilBaseEnv_FallsBackToOsEnviron", func(t *testing.T) {
		t.Setenv("BUDDY_TEST_SENTINEL_VAR", "test_sentinel_val")

		srv := ServerConfig{
			Command: "echo",
			Env: map[string]string{
				"CUSTOM_OVERRIDE": "custom_val",
			},
		}

		transport, err := NewStdioTransport(srv, nil)
		if err != nil {
			t.Fatalf("NewStdioTransport failed: %v", err)
		}

		cmd := transport.Cmd()
		envMap := make(map[string]string)
		for _, e := range cmd.Env {
			k, v, ok := strings.Cut(e, "=")
			if ok {
				envMap[k] = v
			}
		}

		if envMap["BUDDY_TEST_SENTINEL_VAR"] != "test_sentinel_val" {
			t.Errorf("expected fallback to os.Environ containing sentinel var, got %q", envMap["BUDDY_TEST_SENTINEL_VAR"])
		}
		if envMap["CUSTOM_OVERRIDE"] != "custom_val" {
			t.Errorf("expected CUSTOM_OVERRIDE='custom_val', got %q", envMap["CUSTOM_OVERRIDE"])
		}
	})

	t.Run("OmittedBaseEnv_FallsBackToOsEnviron", func(t *testing.T) {
		t.Setenv("BUDDY_TEST_SENTINEL_VAR_2", "sentinel_2")

		srv := ServerConfig{
			Command: "echo",
		}

		transport, err := NewStdioTransport(srv)
		if err != nil {
			t.Fatalf("NewStdioTransport failed: %v", err)
		}

		cmd := transport.Cmd()
		found := false
		for _, e := range cmd.Env {
			if e == "BUDDY_TEST_SENTINEL_VAR_2=sentinel_2" {
				found = true
				break
			}
		}
		if !found {
			t.Error("expected fallback to os.Environ when baseEnv omitted")
		}
	})

	t.Run("EmptyBaseEnv_PreservedWithoutOsEnviron", func(t *testing.T) {
		srv := ServerConfig{
			Command: testExecutable(t),
			Env: map[string]string{
				"ONLY_KEY": "only_val",
			},
		}

		transport, err := NewStdioTransport(srv, []string{})
		if err != nil {
			t.Fatalf("NewStdioTransport failed: %v", err)
		}

		cmd := transport.Cmd()
		if len(cmd.Env) != 1 || cmd.Env[0] != "ONLY_KEY=only_val" {
			t.Errorf("expected only explicit srv.Env when empty baseEnv provided, got: %+v", cmd.Env)
		}
	})

	t.Run("MissingCommand_ReturnsError", func(t *testing.T) {
		srv := ServerConfig{Command: ""}
		_, err := NewStdioTransport(srv)
		if err == nil {
			t.Error("expected error for missing command")
		}
	})
}

func TestStdioTransport_Accessors(t *testing.T) {
	srv := ServerConfig{
		Command: "echo",
		Args:    []string{"test"},
	}
	transport, err := NewStdioTransport(srv)
	if err != nil {
		t.Fatalf("NewStdioTransport failed: %v", err)
	}

	if transport.Cmd() == nil {
		t.Fatal("expected non-nil Cmd")
	}
	if transport.Supervisor() == nil {
		t.Fatal("expected non-nil Supervisor")
	}
	if transport.StderrBuffer() == nil {
		t.Fatal("expected non-nil StderrBuffer")
	}
	if transport.StderrText() != "" {
		t.Errorf("expected empty initial StderrText, got %q", transport.StderrText())
	}
}

func TestStdioTransport_ResolveBareCommandInCustomPATH(t *testing.T) {
	tempDir := t.TempDir()
	binName := "custom-runner"
	targetPath := filepath.Join(tempDir, binName)
	if runtime.GOOS == "windows" {
		targetPath = filepath.Join(tempDir, binName+".bat")
		if err := os.WriteFile(targetPath, []byte("@echo off\r\necho custom\r\n"), 0755); err != nil {
			t.Fatalf("failed to create script: %v", err)
		}
	} else {
		if err := os.WriteFile(targetPath, []byte("#!/bin/sh\necho custom\n"), 0755); err != nil {
			t.Fatalf("failed to create script: %v", err)
		}
	}

	baseEnv := []string{
		"PATH=" + tempDir,
	}
	srv := ServerConfig{
		Command: binName,
	}

	transport, err := NewStdioTransport(srv, baseEnv)
	if err != nil {
		t.Fatalf("NewStdioTransport failed: %v", err)
	}

	cmd := transport.Cmd()
	if cmd == nil {
		t.Fatal("expected non-nil cmd")
	}

	expectedAbs, err := filepath.Abs(targetPath)
	if err != nil {
		t.Fatalf("filepath.Abs failed: %v", err)
	}
	if cmd.Path != expectedAbs {
		t.Errorf("expected cmd.Path %q, got %q", expectedAbs, cmd.Path)
	}
}

func TestStdioTransport_ServerRelativePATHUsesWorkDir(t *testing.T) {
	serverDir := t.TempDir()
	binDir := filepath.Join(serverDir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("failed to create server bin directory: %v", err)
	}
	binName := "server-runner"
	targetPath := filepath.Join(binDir, binName)
	if runtime.GOOS == "windows" {
		targetPath += ".bat"
		if err := os.WriteFile(targetPath, []byte("@echo off\r\n"), 0755); err != nil {
			t.Fatalf("failed to create server executable: %v", err)
		}
	} else if err := os.WriteFile(targetPath, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatalf("failed to create server executable: %v", err)
	}

	transport, err := NewStdioTransport(ServerConfig{
		Command: binName,
		WorkDir: serverDir,
		Env:     map[string]string{"PATH": "./bin"},
	}, []string{"PATH=/daemon/bin"})
	if err != nil {
		t.Fatalf("NewStdioTransport failed: %v", err)
	}

	expectedPath, err := filepath.Abs(targetPath)
	if err != nil {
		t.Fatalf("filepath.Abs failed: %v", err)
	}
	if transport.Cmd().Path != expectedPath {
		t.Errorf("expected server-relative executable %q, got %q", expectedPath, transport.Cmd().Path)
	}
	if transport.Cmd().Dir != serverDir {
		t.Errorf("expected working directory %q, got %q", serverDir, transport.Cmd().Dir)
	}
}

func TestStdioTransport_CustomPATH_PrecedenceOverHost(t *testing.T) {
	tempDir := t.TempDir()
	binName := "node"
	targetPath := filepath.Join(tempDir, binName)
	if runtime.GOOS == "windows" {
		targetPath = filepath.Join(tempDir, binName+".bat")
		if err := os.WriteFile(targetPath, []byte("@echo off\r\necho custom-node\r\n"), 0755); err != nil {
			t.Fatalf("failed to create script: %v", err)
		}
	} else {
		if err := os.WriteFile(targetPath, []byte("#!/bin/sh\necho custom-node\n"), 0755); err != nil {
			t.Fatalf("failed to create script: %v", err)
		}
	}

	// Place tempDir at the front of PATH so it takes precedence over host PATH
	hostPath := os.Getenv("PATH")
	customPath := tempDir + string(filepath.ListSeparator) + hostPath

	baseEnv := []string{
		"PATH=" + customPath,
	}
	srv := ServerConfig{
		Command: binName,
	}

	transport, err := NewStdioTransport(srv, baseEnv)
	if err != nil {
		t.Fatalf("NewStdioTransport failed: %v", err)
	}

	cmd := transport.Cmd()
	if cmd == nil {
		t.Fatal("expected non-nil cmd")
	}

	expectedAbs, err := filepath.Abs(targetPath)
	if err != nil {
		t.Fatalf("filepath.Abs failed: %v", err)
	}
	if cmd.Path != expectedAbs {
		t.Errorf("expected cmd.Path to resolve to custom directory %q, got %q", expectedAbs, cmd.Path)
	}
}

func TestStdioTransport_BareCommandMustResolveInEffectivePATH(t *testing.T) {
	hostExecutable, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go executable is not available on the host PATH")
	}
	command := filepath.Base(hostExecutable)
	emptyPath := t.TempDir()

	_, err = NewStdioTransport(ServerConfig{Command: command}, []string{"PATH=" + emptyPath})
	if err == nil {
		t.Fatalf("expected %q to fail when absent from the effective session PATH", command)
	}
	if !strings.Contains(err.Error(), "effective session PATH") {
		t.Fatalf("expected session PATH resolution error, got: %v", err)
	}

	_, err = NewStdioTransport(ServerConfig{Command: command}, []string{"SESSION_ONLY=value"})
	if err == nil {
		t.Fatalf("expected %q to fail when PATH is absent from the effective session environment", command)
	}
	if !strings.Contains(err.Error(), "PATH is not set") {
		t.Fatalf("expected missing PATH error, got: %v", err)
	}
}

func TestLookPathIn(t *testing.T) {
	tempDir := t.TempDir()
	dirA := filepath.Join(tempDir, "a")
	dirB := filepath.Join(tempDir, "b")
	if err := os.MkdirAll(dirA, 0755); err != nil {
		t.Fatalf("mkdir dirA: %v", err)
	}
	if err := os.MkdirAll(dirB, 0755); err != nil {
		t.Fatalf("mkdir dirB: %v", err)
	}

	toolName := "mytool"
	pathB := filepath.Join(dirB, toolName)
	if runtime.GOOS == "windows" {
		pathB = filepath.Join(dirB, toolName+".bat")
		if err := os.WriteFile(pathB, []byte("@echo off\r\n"), 0755); err != nil {
			t.Fatalf("write pathB: %v", err)
		}
	} else {
		if err := os.WriteFile(pathB, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
			t.Fatalf("write pathB: %v", err)
		}
	}

	pathList := dirA + string(filepath.ListSeparator) + dirB
	resolved, err := lookPathIn(toolName, pathList)
	if err != nil {
		t.Fatalf("lookPathIn failed: %v", err)
	}
	expectedAbs, _ := filepath.Abs(pathB)
	if resolved != expectedAbs {
		t.Errorf("expected %q, got %q", expectedAbs, resolved)
	}

	// Non-existent command returns ErrNotFound
	_, err = lookPathIn("non-existent-binary-12345", pathList)
	if !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("expected exec.ErrNotFound, got: %v", err)
	}

	// Command with path separator evaluated directly
	directResolved, err := lookPathIn(pathB, "")
	if err != nil {
		t.Fatalf("lookPathIn direct path failed: %v", err)
	}
	if directResolved != expectedAbs {
		t.Errorf("expected direct %q, got %q", expectedAbs, directResolved)
	}

	// Unix-only: test non-executable permissions skipped
	if runtime.GOOS != "windows" {
		nonExec := filepath.Join(dirA, "noexec")
		if err := os.WriteFile(nonExec, []byte("#!/bin/sh\n"), 0644); err != nil {
			t.Fatalf("write noexec: %v", err)
		}
		_, err = lookPathIn("noexec", dirA)
		if !errors.Is(err, exec.ErrNotFound) {
			t.Errorf("expected ErrNotFound for non-executable file, got: %v", err)
		}
	}
}
