package persistence

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *SQLiteStore {
	safeName := strings.ReplaceAll(t.Name(), "/", "_")
	dsn := fmt.Sprintf("file:%s_%d?mode=memory&cache=shared&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", safeName, time.Now().UnixNano())
	store, err := NewSQLiteStore(dsn)
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestMigrationRunner(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	var version int
	err := store.writerDB.QueryRowContext(ctx, "PRAGMA user_version;").Scan(&version)
	if err != nil {
		t.Fatalf("failed to query user_version: %v", err)
	}
	if version != 1 {
		t.Errorf("expected user_version 1, got %d", version)
	}
}

func TestSessionCRUD(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	sess := &SessionRecord{
		ID:         "sess_test_123456",
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
		Status:     StatusRunning,
		Task:       "Implement pure Go SQLite persistence",
		Model:      "gpt-4o",
		WorkingDir: "/test/workspace",
		MaxSteps:   30,
		TotalSteps: 0,
		TokenUsage: TokenUsage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150},
	}

	if err := store.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	// Exact match
	retrieved, err := store.GetSession(ctx, "sess_test_123456")
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}
	if retrieved.Task != sess.Task || retrieved.Status != StatusRunning {
		t.Errorf("mismatched session: %+v", retrieved)
	}

	// Short prefix match (minimum 4 chars)
	byPrefix, err := store.GetSession(ctx, "sess_test")
	if err != nil {
		t.Fatalf("GetSession by prefix failed: %v", err)
	}
	if byPrefix.ID != sess.ID {
		t.Errorf("expected %s, got %s", sess.ID, byPrefix.ID)
	}

	// Update status
	err = store.UpdateSessionStatus(ctx, sess.ID, StatusCompleted, 5, TokenUsage{
		PromptTokens:     500,
		CompletionTokens: 250,
		TotalTokens:      750,
	}, "")
	if err != nil {
		t.Fatalf("UpdateSessionStatus failed: %v", err)
	}

	updated, err := store.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetSession updated failed: %v", err)
	}
	if updated.Status != StatusCompleted || updated.TotalSteps != 5 {
		t.Errorf("expected status completed and 5 steps, got status=%s steps=%d", updated.Status, updated.TotalSteps)
	}

	// List sessions
	list, err := store.ListSessions(ctx, "/test/workspace", 10)
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("expected 1 session, got %d", len(list))
	}
}

func TestAmbiguousShortID(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	sess1 := &SessionRecord{
		ID:         "sess_alpha_1",
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
		Status:     StatusRunning,
		Task:       "Task 1",
		Model:      "gpt-4o",
		WorkingDir: "/test/workspace",
		MaxSteps:   30,
	}
	sess2 := &SessionRecord{
		ID:         "sess_alpha_2",
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
		Status:     StatusRunning,
		Task:       "Task 2",
		Model:      "gpt-4o",
		WorkingDir: "/test/workspace",
		MaxSteps:   30,
	}

	_ = store.CreateSession(ctx, sess1)
	_ = store.CreateSession(ctx, sess2)

	_, err := store.GetSession(ctx, "sess_alpha")
	if err == nil {
		t.Fatal("expected AmbiguousIDError, got nil")
	}
	var ambErr *AmbiguousIDError
	if !strings.Contains(err.Error(), "ambiguous short session ID") {
		t.Errorf("expected ambiguous error string, got %v", err)
	}
	_ = ambErr
}

func TestTurnCommitAndJSONRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	sess := &SessionRecord{
		ID:         "sess_turns_roundtrip",
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
		Status:     StatusRunning,
		Task:       "Test roundtrip",
		Model:      "gpt-4o",
		WorkingDir: "/test/workspace",
		MaxSteps:   30,
	}
	if err := store.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	turn1 := &TurnRecord{
		SessionID:        sess.ID,
		StepNumber:       1,
		Role:             "assistant",
		Content:          "Checking files",
		ReasoningContent: "Let me check directory contents first",
		ToolCalls: []ToolCallRecord{
			{
				ID:       "call_1",
				Name:     "bash",
				Args:     `{"command": "ls -la"}`,
				Result:   "total 0\n-rw-r--r-- 1 user user 0 file.txt",
				ExitCode: 0,
			},
		},
		TokenUsage: TokenUsage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120},
		DurationMs: 150,
		CreatedAt:  time.Now().UTC(),
	}

	err := store.CommitTurn(ctx, turn1, 1, turn1.TokenUsage)
	if err != nil {
		t.Fatalf("CommitTurn failed: %v", err)
	}

	turns, err := store.GetAllTurns(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetAllTurns failed: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(turns))
	}

	rt := turns[0]
	if rt.StepNumber != 1 || rt.Content != "Checking files" || rt.ReasoningContent != "Let me check directory contents first" {
		t.Errorf("mismatched turn fields: %+v", rt)
	}
	if len(rt.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(rt.ToolCalls))
	}
	tc := rt.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "bash" || tc.ExitCode != 0 {
		t.Errorf("mismatched tool call: %+v", tc)
	}
}

func TestSecretRedaction(t *testing.T) {
	raw := "Connecting with Bearer sk-ant-api03-1234567890abcdef and ghp_0123456789abcdefghijklmnopqrstuv and sk-1234567890abcdefghijklmnopqrstuvwxyz"
	redacted := RedactSecrets(raw)

	if strings.Contains(redacted, "ghp_0123456789") {
		t.Errorf("expected GitHub PAT to be redacted: %s", redacted)
	}
	if strings.Contains(redacted, "sk-1234567890") {
		t.Errorf("expected OpenAI key to be redacted: %s", redacted)
	}
}

func TestToolOutputTruncationAndRuneBoundary(t *testing.T) {
	// Build a string that exceeds 64KB with multi-byte runes
	longStr := strings.Repeat("Hello 🚀 世界! ", 5000) // ~70KB
	sanitized := SanitizeToolOutput(longStr)

	if len(sanitized) > MaxStoredToolOutputBytes+200 {
		t.Errorf("expected clamped length around %d, got %d", MaxStoredToolOutputBytes, len(sanitized))
	}
	if !strings.Contains(sanitized, "[Output truncated for persistence:") {
		t.Errorf("expected truncation marker in output")
	}
}

func TestSnapshotAppendOnly(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	sess := &SessionRecord{
		ID:         "sess_snapshots",
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
		Status:     StatusRunning,
		Task:       "Snapshot test",
		Model:      "gpt-4o",
		WorkingDir: "/test/workspace",
		MaxSteps:   30,
	}
	_ = store.CreateSession(ctx, sess)

	snap1 := &SnapshotRecord{
		SessionID:          sess.ID,
		StepNumber:         5,
		DistilledNarrative: "Summary at step 5",
		StateTrackerJSON:   `{"files": ["a.go"]}`,
		CurrentTokens:      2000,
		CreatedAt:          time.Now().UTC(),
	}
	if err := store.SaveSnapshot(ctx, snap1); err != nil {
		t.Fatalf("SaveSnapshot 1 failed: %v", err)
	}

	snap2 := &SnapshotRecord{
		SessionID:          sess.ID,
		StepNumber:         15,
		DistilledNarrative: "Summary at step 15",
		StateTrackerJSON:   `{"files": ["a.go", "b.go"]}`,
		CurrentTokens:      3500,
		CreatedAt:          time.Now().UTC(),
	}
	if err := store.SaveSnapshot(ctx, snap2); err != nil {
		t.Fatalf("SaveSnapshot 2 failed: %v", err)
	}

	latest, err := store.GetLatestSnapshot(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetLatestSnapshot failed: %v", err)
	}
	if latest == nil || latest.StepNumber != 15 {
		t.Errorf("expected latest snapshot at step 15, got %+v", latest)
	}
}

func TestDualPoolConcurrency(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	sess := &SessionRecord{
		ID:         "sess_concurrent",
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
		Status:     StatusRunning,
		Task:       "Concurrency test",
		Model:      "gpt-4o",
		WorkingDir: "/test/workspace",
		MaxSteps:   50,
	}
	_ = store.CreateSession(ctx, sess)

	var wg sync.WaitGroup
	// 5 readers running concurrently
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_, _ = store.ListSessions(ctx, "/test/workspace", 10)
				time.Sleep(1 * time.Millisecond)
			}
		}()
	}

	// 1 writer writing turns
	wg.Add(1)
	go func() {
		defer wg.Done()
		for step := 1; step <= 20; step++ {
			turn := &TurnRecord{
				SessionID:  sess.ID,
				StepNumber: step,
				Role:       "assistant",
				Content:    fmt.Sprintf("Turn %d", step),
				CreatedAt:  time.Now().UTC(),
			}
			_ = store.CommitTurn(ctx, turn, step, TokenUsage{TotalTokens: int64(step * 10)})
			time.Sleep(1 * time.Millisecond)
		}
	}()

	wg.Wait()

	turns, err := store.GetAllTurns(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetAllTurns failed: %v", err)
	}
	if len(turns) != 20 {
		t.Errorf("expected 20 turns, got %d", len(turns))
	}
}

func TestPruneSessions(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	for i := 1; i <= 5; i++ {
		sess := &SessionRecord{
			ID:         fmt.Sprintf("sess_prune_%d", i),
			CreatedAt:  now.Add(-time.Duration(10-i) * 24 * time.Hour),
			UpdatedAt:  now.Add(-time.Duration(10-i) * 24 * time.Hour),
			Status:     StatusCompleted,
			Task:       fmt.Sprintf("Task %d", i),
			Model:      "gpt-4o",
			WorkingDir: "/test/workspace",
		}
		_ = store.CreateSession(ctx, sess)
	}

	// Keep max 2 sessions
	affected, err := store.PruneSessions(ctx, 24*time.Hour, 2)
	if err != nil {
		t.Fatalf("PruneSessions failed: %v", err)
	}
	if affected != 3 {
		t.Errorf("expected 3 pruned sessions, got %d", affected)
	}
}

func TestCorruptionQuarantine(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	// Write garbage bytes to simulate corrupted file
	err := os.WriteFile(dbPath, []byte("NOT A SQLITE FILE GARBAGE HEADER"), 0600)
	if err != nil {
		t.Fatalf("failed to write corrupt file: %v", err)
	}

	err = CheckOrQuarantine(dbPath)
	if err != nil {
		t.Fatalf("CheckOrQuarantine returned error: %v", err)
	}

	// The original dbPath should now be quarantined (gone from dbPath)
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Errorf("expected corrupt db to be renamed, but still exists")
	}

	entries, _ := os.ReadDir(tmpDir)
	foundCorrupt := false
	for _, e := range entries {
		if strings.Contains(e.Name(), "test.db.corrupt.") {
			foundCorrupt = true
			break
		}
	}
	if !foundCorrupt {
		t.Errorf("expected to find test.db.corrupt.<timestamp>, but found entries: %v", entries)
	}
}
