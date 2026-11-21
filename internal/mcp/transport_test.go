package mcp

import (
	"strings"
	"testing"
)

func TestStdioTransport_BaseEnvironment(t *testing.T) {
	t.Run("CustomBaseEnv_WithOverrides", func(t *testing.T) {
		baseEnv := []string{
			"CALLER_VAR=caller_value",
			"SHARED_VAR=original_value",
			"PATH=/custom/bin",
		}
		srv := ServerConfig{
			Command: "echo",
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
			Command: "echo",
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
