package daemon

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hoangndst/buddy-agent/internal/agent"
	"github.com/hoangndst/buddy-agent/internal/agent/provider"
	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	"github.com/hoangndst/buddy-agent/internal/config"
	"github.com/hoangndst/buddy-agent/internal/daemon"
	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
	"github.com/hoangndst/buddy-agent/internal/tools"
	"github.com/openai/openai-go"
)

func TestCmdDaemon_StatusStopped(t *testing.T) {
	tempDir := t.TempDir()
	sockPath := filepath.Join(tempDir, "non_existent.sock")

	f := cmdutil.NewFactory()
	ioStreams, _, outBuf, _ := cmdutil.TestIOStreams()
	f.IOStreams = ioStreams

	cmd := newCmdStatus(f)
	cmd.SetArgs([]string{"--socket", sockPath})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("expected status command to succeed, got error: %v", err)
	}

	if !strings.Contains(outBuf.String(), "Daemon Status: Stopped") {
		t.Errorf("expected output to indicate stopped daemon, got:\n%s", outBuf.String())
	}
}

func TestCmdDaemon_StatusAndStopActive(t *testing.T) {
	tempDir := t.TempDir()
	sockPath := filepath.Join(tempDir, "active_daemon.sock")

	sockMgr := daemon.NewSocketManager(sockPath)
	factory := func(_ string, _ protocol.SessionStartParams, emitHandler agent.EventHandler) (*agent.Engine, func(), error) {
		eng := agent.NewEngine(
			nil,
			"test-model",
			5,
			tools.NewRegistry(10000),
			nil,
			emitHandler,
			agent.WithSystemPromptOverride("mock prompt"),
		)
		return eng, nil, nil
	}

	sessionMgr := daemon.NewSessionManager(factory)
	server := daemon.NewServer(sockMgr, sessionMgr)

	if err := server.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer func() { _ = server.Stop() }()

	f := cmdutil.NewFactory()
	ioStreams, _, outBuf, _ := cmdutil.TestIOStreams()
	f.IOStreams = ioStreams

	// 1. Check status of running daemon
	statusCmd := newCmdStatus(f)
	statusCmd.SetArgs([]string{"--socket", sockPath})
	if err := statusCmd.Execute(); err != nil {
		t.Fatalf("status command failed: %v", err)
	}
	if !strings.Contains(outBuf.String(), "Daemon Status:   Active") {
		t.Errorf("expected Active daemon status, got:\n%s", outBuf.String())
	}

	// 2. Stop daemon via stop command
	outBuf.Reset()
	stopCmd := newCmdStop(f)
	stopCmd.SetArgs([]string{"--socket", sockPath})
	if err := stopCmd.Execute(); err != nil {
		t.Fatalf("stop command failed: %v", err)
	}
	if !strings.Contains(outBuf.String(), "stopped successfully") {
		t.Errorf("expected success message, got:\n%s", outBuf.String())
	}

	time.Sleep(100 * time.Millisecond)

	// 3. Status should now report stopped
	outBuf.Reset()
	statusCmd2 := newCmdStatus(f)
	statusCmd2.SetArgs([]string{"--socket", sockPath})
	_ = statusCmd2.Execute()
	if !strings.Contains(outBuf.String(), "Daemon Status: Stopped") {
		t.Errorf("expected daemon to report Stopped after stop command, got:\n%s", outBuf.String())
	}
}

func TestBuildEngineForSession_EffectiveLocality(t *testing.T) {
	tempDir := t.TempDir()
	f := cmdutil.NewFactory()

	// Initial daemon config: configured as local
	f.ConfigLoader = func() (*config.Config, error) {
		return &config.Config{
			Model:            "local-llama",
			BaseURL:          "http://127.0.0.1:11434/v1",
			IsLocal:          true,
			WorkspaceDir:     tempDir,
			MaxOutputChars:   10000,
			MaxSteps:         10,
			MaxContextTokens: 16384,
		}, nil
	}

	var observedConfig *config.Config
	f.NewModelProvider = func(cfg *config.Config, _ *openai.Client) provider.ModelProvider {
		c := *cfg
		observedConfig = &c
		return provider.NewAutoProvider(nil, nil, cfg.IsLocal)
	}

	// Client session specifies remote base URL and APIType
	params := protocol.SessionStartParams{
		Task:         "analyze code",
		BaseURL:      "https://api.openai.com/v1",
		Model:        "gpt-4o",
		APIType:      "responses",
		WorkspaceDir: tempDir,
	}

	eng, cleanup, err := buildEngineForSession(context.Background(), f, "sess_test_locality", params, nil)
	if err != nil {
		t.Fatalf("buildEngineForSession failed: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if eng == nil {
		t.Fatal("expected non-nil engine")
	}

	if observedConfig == nil {
		t.Fatal("expected NewModelProvider to be invoked with session config")
	}
	if observedConfig.IsLocal {
		t.Errorf("expected observedConfig.IsLocal to be false for remote endpoint, got true")
	}
	if observedConfig.Model != "gpt-4o" {
		t.Errorf("expected observedConfig.Model to be 'gpt-4o', got %q", observedConfig.Model)
	}
	if observedConfig.BaseURL != "https://api.openai.com/v1" {
		t.Errorf("expected observedConfig.BaseURL to be 'https://api.openai.com/v1', got %q", observedConfig.BaseURL)
	}
	if observedConfig.APIType != "responses" {
		t.Errorf("expected observedConfig.APIType to be 'responses', got %q", observedConfig.APIType)
	}
}

func TestBuildEngineForSession_EmptyKeyEndpointSwitch(t *testing.T) {
	tempDir := t.TempDir()
	f := cmdutil.NewFactory()

	// Initial daemon config: has secret API key and remote base URL
	f.ConfigLoader = func() (*config.Config, error) {
		return &config.Config{
			Model:            "gpt-4o",
			BaseURL:          "https://api.openai.com/v1",
			APIKey:           "sk-super-secret-key",
			IsLocal:          false,
			WorkspaceDir:     tempDir,
			MaxOutputChars:   10000,
			MaxSteps:         10,
			MaxContextTokens: 64000,
		}, nil
	}

	var observedConfig *config.Config
	f.NewModelProvider = func(cfg *config.Config, _ *openai.Client) provider.ModelProvider {
		c := *cfg
		observedConfig = &c
		return provider.NewAutoProvider(nil, nil, cfg.IsLocal)
	}

	// Client session targets local endpoint with explicitly empty key
	emptyKey := ""
	params := protocol.SessionStartParams{
		Task:         "local task",
		BaseURL:      "http://127.0.0.1:11434/v1",
		APIKey:       &emptyKey,
		Model:        "qwen",
		WorkspaceDir: tempDir,
	}

	eng, cleanup, err := buildEngineForSession(context.Background(), f, "sess_test_empty_key", params, nil)
	if err != nil {
		t.Fatalf("buildEngineForSession failed: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if eng == nil {
		t.Fatal("expected non-nil engine")
	}

	if observedConfig == nil {
		t.Fatal("expected NewModelProvider to be invoked with session config")
	}
	if observedConfig.APIKey != "" {
		t.Errorf("expected empty APIKey when caller explicitly overrides with empty key, got %q", observedConfig.APIKey)
	}
}

func TestBuildEngineForSession_EmptyReasoningEffortOverride(t *testing.T) {
	tempDir := t.TempDir()
	f := cmdutil.NewFactory()

	// Initial daemon config: configured with high reasoning effort
	f.ConfigLoader = func() (*config.Config, error) {
		return &config.Config{
			Model:            "o3-mini",
			BaseURL:          "https://api.openai.com/v1",
			ReasoningEffort:  "high",
			WorkspaceDir:     tempDir,
			MaxOutputChars:   10000,
			MaxSteps:         10,
			MaxContextTokens: 64000,
		}, nil
	}

	var observedConfig *config.Config
	f.NewModelProvider = func(cfg *config.Config, _ *openai.Client) provider.ModelProvider {
		c := *cfg
		observedConfig = &c
		return provider.NewAutoProvider(nil, nil, cfg.IsLocal)
	}

	// Client session specifies explicitly empty reasoning effort
	emptyEffort := ""
	params := protocol.SessionStartParams{
		Task:            "quick task",
		ReasoningEffort: &emptyEffort,
		WorkspaceDir:    tempDir,
	}

	eng, cleanup, err := buildEngineForSession(context.Background(), f, "sess_test_empty_effort", params, nil)
	if err != nil {
		t.Fatalf("buildEngineForSession failed: %v", err)
	}
	if cleanup != nil {
		cleanup()
	}
	if eng == nil {
		t.Fatal("expected non-nil engine")
	}

	if observedConfig == nil {
		t.Fatal("expected NewModelProvider to be invoked with session config")
	}
	if observedConfig.ReasoningEffort != "" {
		t.Errorf("expected empty ReasoningEffort when caller explicitly overrides with empty string, got %q", observedConfig.ReasoningEffort)
	}
}
