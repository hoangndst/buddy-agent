package cmd_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hoangndst/buddy-agent/internal/agent/provider"
	"github.com/hoangndst/buddy-agent/internal/cmd"
	"github.com/hoangndst/buddy-agent/internal/cmdutil"
	"github.com/hoangndst/buddy-agent/internal/config"
	"github.com/hoangndst/buddy-agent/internal/persistence"
	"github.com/openai/openai-go"
)

type mockProvider struct {
	content string
}

func (m *mockProvider) Generate(_ context.Context, _ provider.GenerateRequest) (*provider.GenerateResponse, error) {
	return &provider.GenerateResponse{
		Content:  m.content,
		Protocol: "chat",
		Usage: provider.TokenUsage{
			PromptTokens:     10,
			CompletionTokens: 10,
			TotalTokens:      20,
		},
	}, nil
}

func (m *mockProvider) Protocol() string {
	return "chat"
}

func TestResumeCommand(t *testing.T) {
	outBuf := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	inBuf := &bytes.Buffer{}

	streams := &cmdutil.IOStreams{
		In:     inBuf,
		Out:    outBuf,
		ErrOut: errBuf,
	}

	dsn := fmt.Sprintf("file:memdb_resume_%d?mode=memory&cache=shared&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", time.Now().UnixNano())
	store, err := persistence.NewSQLiteStore(dsn)
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	defer func() { _ = store.Close() }()

	ctx := context.Background()
	now := time.Now().UTC()
	sess := &persistence.SessionRecord{
		ID:         "sess_resume_test_123",
		CreatedAt:  now,
		UpdatedAt:  now,
		Status:     persistence.StatusInterrupted,
		Task:       "Continue fixing bug",
		Model:      "gpt-4o",
		WorkingDir: "/test/workspace",
		MaxSteps:   10,
		TotalSteps: 1,
	}
	_ = store.CreateSession(ctx, sess)

	turn := &persistence.TurnRecord{
		SessionID:  sess.ID,
		StepNumber: 1,
		Role:       "assistant",
		Content:    "Step 1 finished",
		DurationMs: 100,
		CreatedAt:  now,
	}
	_ = store.CommitTurn(ctx, turn, 1, persistence.TokenUsage{TotalTokens: 20})

	f := cmdutil.NewFactory()
	f.IOStreams = streams
	f.ConfigLoader = func() (*config.Config, error) {
		return &config.Config{
			WorkspaceDir: "/test/workspace",
			Model:        "gpt-4o",
			MaxSteps:     10,
		}, nil
	}
	f.Store = func() (persistence.Store, func(), error) {
		return store, func() {}, nil
	}
	f.NewModelProvider = func(_ *config.Config, _ *openai.Client) provider.ModelProvider {
		return &mockProvider{content: "Bug fixed in resumed step 2"}
	}

	rootCmd := cmd.NewRootCmd(f)
	rootCmd.SetArgs([]string{"resume", "sess_resume_test_123"})
	err = rootCmd.ExecuteContext(ctx)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}

	output := outBuf.String()
	if !strings.Contains(output, "Resuming session sess_resume_test_123...") ||
		!strings.Contains(output, "Bug fixed in resumed step 2") {
		t.Errorf("unexpected resume output: %s", output)
	}

	// Verify session marked completed
	resumed, err := store.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("failed to get resumed session: %v", err)
	}
	if resumed.Status != persistence.StatusCompleted {
		t.Errorf("expected status completed, got %s", resumed.Status)
	}
}
