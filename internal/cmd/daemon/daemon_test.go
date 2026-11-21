package daemon

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hoangndst/buddy-agent/internal/agent"
	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	"github.com/hoangndst/buddy-agent/internal/daemon"
	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
	"github.com/hoangndst/buddy-agent/internal/tools"
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
