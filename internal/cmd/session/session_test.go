package session_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hoangndst/buddy-agent/internal/cmd"
	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	"github.com/hoangndst/buddy-agent/internal/config"
	"github.com/hoangndst/buddy-agent/internal/persistence"
)

func setupTestFactory(t *testing.T) (*cmdutil.Factory, persistence.Store, *bytes.Buffer) {
	outBuf := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	inBuf := &bytes.Buffer{}

	streams := &cmdutil.IOStreams{
		In:     inBuf,
		Out:    outBuf,
		ErrOut: errBuf,
	}

	dsn := fmt.Sprintf("file:memdb_cmd_%d?mode=memory&cache=shared&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", time.Now().UnixNano())
	store, err := persistence.NewSQLiteStore(dsn)
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	f := cmdutil.NewFactory()
	f.IOStreams = streams
	f.ConfigLoader = func() (*config.Config, error) {
		return &config.Config{
			WorkspaceDir: "/test/workspace",
			Model:        "gpt-4o",
			MaxSteps:     30,
		}, nil
	}
	f.Store = func() (persistence.Store, func(), error) {
		return store, func() {}, nil
	}

	return f, store, outBuf
}

func TestCmdSessionList_EmptyAndPopulated(t *testing.T) {
	f, store, outBuf := setupTestFactory(t)
	rootCmd := cmd.NewRootCmd(f)

	// 1. Empty list
	rootCmd.SetArgs([]string{"session", "list"})
	err := rootCmd.ExecuteContext(context.Background())
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if !strings.Contains(outBuf.String(), "No historical sessions found") {
		t.Errorf("expected empty notice, got: %s", outBuf.String())
	}

	// 2. Add sessions and list again
	outBuf.Reset()
	now := time.Now().UTC()
	sess := &persistence.SessionRecord{
		ID:         "sess_abc123456789",
		CreatedAt:  now,
		UpdatedAt:  now,
		Status:     persistence.StatusCompleted,
		Task:       "Refactor authentication flow",
		Model:      "gpt-4o",
		WorkingDir: "/test/workspace",
		TotalSteps: 8,
		TokenUsage: persistence.TokenUsage{TotalTokens: 1250},
	}
	_ = store.CreateSession(context.Background(), sess)

	rootCmd.SetArgs([]string{"session", "list"})
	err = rootCmd.ExecuteContext(context.Background())
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	output := outBuf.String()
	if !strings.Contains(output, "sess_abc1234") || !strings.Contains(output, "completed") || !strings.Contains(output, "Refactor authentication flow") {
		t.Errorf("unexpected list output: %s", output)
	}
}

func TestCmdSessionShow(t *testing.T) {
	f, store, outBuf := setupTestFactory(t)
	rootCmd := cmd.NewRootCmd(f)
	ctx := context.Background()

	now := time.Now().UTC()
	sess := &persistence.SessionRecord{
		ID:         "sess_show_test_99",
		CreatedAt:  now,
		UpdatedAt:  now,
		Status:     persistence.StatusCompleted,
		Task:       "Run unit tests",
		Model:      "gpt-4o",
		WorkingDir: "/test/workspace",
		TotalSteps: 1,
		TokenUsage: persistence.TokenUsage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
	}
	_ = store.CreateSession(ctx, sess)

	turn := &persistence.TurnRecord{
		SessionID:        sess.ID,
		StepNumber:       1,
		Role:             "assistant",
		Content:          "Tests passed",
		ReasoningContent: "Checking bash execution",
		ToolCalls: []persistence.ToolCallRecord{
			{
				ID:       "call_1",
				Name:     "bash",
				Args:     `{"command": "go test ./..."}`,
				Result:   "PASS\nok",
				ExitCode: 0,
			},
		},
		TokenUsage: sess.TokenUsage,
		DurationMs: 120,
		CreatedAt:  now,
	}
	_ = store.CommitTurn(ctx, turn, 1, sess.TokenUsage)

	rootCmd.SetArgs([]string{"session", "show", "sess_show_test_99"})
	err := rootCmd.ExecuteContext(ctx)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}

	output := outBuf.String()
	if !strings.Contains(output, "Session: sess_show_test_99") ||
		!strings.Contains(output, "Run unit tests") ||
		!strings.Contains(output, "Checking bash execution") ||
		!strings.Contains(output, "bash: go test ./...") {
		t.Errorf("unexpected show output: %s", output)
	}
}

func TestCmdSessionPrune(t *testing.T) {
	f, store, outBuf := setupTestFactory(t)
	rootCmd := cmd.NewRootCmd(f)
	ctx := context.Background()

	now := time.Now().UTC()
	for i := 1; i <= 5; i++ {
		sess := &persistence.SessionRecord{
			ID:         fmt.Sprintf("sess_prune_%d", i),
			CreatedAt:  now.Add(-time.Duration(100+i) * time.Hour),
			UpdatedAt:  now.Add(-time.Duration(100+i) * time.Hour),
			Status:     persistence.StatusCompleted,
			Task:       fmt.Sprintf("Old task %d", i),
			Model:      "gpt-4o",
			WorkingDir: "/test/workspace",
		}
		_ = store.CreateSession(ctx, sess)
	}

	rootCmd.SetArgs([]string{"session", "prune", "--older-than", "24h", "--keep", "2"})
	err := rootCmd.ExecuteContext(ctx)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}

	if !strings.Contains(outBuf.String(), "Pruned 3 expired sessions") {
		t.Errorf("expected 3 pruned sessions output, got: %s", outBuf.String())
	}
}
