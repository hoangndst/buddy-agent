package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	if version != 4 {
		t.Errorf("expected user_version 4, got %d", version)
	}
}

func TestMigration003BackfillsExistingSessionsAsTasks(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprintf("from_v%d", version), func(t *testing.T) {
			dsn := fmt.Sprintf("file:migration_%d_%d?mode=memory&cache=shared&_pragma=foreign_keys(ON)", version, time.Now().UnixNano())
			db, err := sql.Open("sqlite", dsn)
			if err != nil {
				t.Fatalf("failed to open legacy database: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			db.SetMaxOpenConns(1)
			ctx := context.Background()
			legacySchema := `
				CREATE TABLE sessions (id TEXT PRIMARY KEY, working_dir TEXT, updated_at DATETIME);
				CREATE TABLE turns (session_id TEXT, step_number INTEGER);
				CREATE TABLE episodic_snapshots (session_id TEXT, step_number INTEGER);
			`
			if version >= 2 {
				legacySchema = `
					CREATE TABLE sessions (id TEXT PRIMARY KEY, working_dir TEXT, updated_at DATETIME, parent_session_id TEXT, role TEXT, depth INTEGER);
					CREATE INDEX idx_sessions_parent ON sessions(parent_session_id);
					CREATE INDEX idx_sessions_depth ON sessions(depth);
					CREATE TABLE turns (session_id TEXT, step_number INTEGER);
					CREATE TABLE episodic_snapshots (session_id TEXT, step_number INTEGER);
				`
			}
			if _, err := db.ExecContext(ctx, legacySchema); err != nil {
				t.Fatalf("failed to create v%d schema: %v", version, err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO sessions(id, working_dir) VALUES ('legacy-task', '/legacy')`); err != nil {
				t.Fatalf("failed to seed v%d session: %v", version, err)
			}
			if _, err := db.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, version)); err != nil {
				t.Fatalf("failed to set v%d schema version: %v", version, err)
			}

			store := &SQLiteStore{writerDB: db, readerDB: db, isMemory: true}
			if err := store.Migrate(ctx); err != nil {
				t.Fatalf("migration from v%d failed: %v", version, err)
			}
			var kind string
			if err := db.QueryRowContext(ctx, `SELECT interaction_kind FROM sessions WHERE id = 'legacy-task'`).Scan(&kind); err != nil {
				t.Fatalf("failed to read backfilled interaction kind: %v", err)
			}
			if kind != string(InteractionTask) {
				t.Fatalf("expected legacy row kind %q, got %q", InteractionTask, kind)
			}
			var gotVersion int
			if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&gotVersion); err != nil || gotVersion != 4 {
				t.Fatalf("expected schema version 4, got %d (err=%v)", gotVersion, err)
			}
		})
	}
}

func TestApplySubmissionPersistsTurnBoundaryAtomically(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	conversationID := "conv_apply_boundary"
	firstTurnID := "turn-first"
	_, err := store.CreateConversation(ctx, &ConversationStartRequest{
		Session:         SessionRecord{ID: conversationID, WorkingDir: "/apply/workspace", Model: "test"},
		StartID:         "start-apply",
		FirstSubmission: SubmissionRecord{SubmissionID: "sub-first", Prompt: "root prompt", TurnID: &firstTurnID},
	})
	if err != nil {
		t.Fatal(err)
	}
	queued, err := store.AdmitSubmission(ctx, &SubmissionRecord{
		ConversationID: conversationID, SubmissionID: "sub-next", Prompt: "follow-up", Mode: SubmissionQueue,
	})
	if err != nil {
		t.Fatal(err)
	}
	if queued.TurnID != nil || queued.Status != SubmissionQueued {
		t.Fatalf("admission assigned execution identity too early: %+v", queued)
	}

	applied, err := store.ApplySubmission(ctx, conversationID, queued.SubmissionID, "turn-next", 7)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Status != SubmissionAttached || applied.TurnID == nil || *applied.TurnID != "turn-next" || applied.AppliedAfterStep == nil || *applied.AppliedAfterStep != 7 {
		t.Fatalf("submission boundary not persisted: %+v", applied)
	}
	if applied.AppliedHistoryRevision <= applied.HistoryRevision {
		t.Fatalf("apply revision should follow admission revision: %+v", applied)
	}
	retry, err := store.ApplySubmission(ctx, conversationID, queued.SubmissionID, "turn-next", 7)
	if err != nil {
		t.Fatal(err)
	}
	if retry.AppliedHistoryRevision != applied.AppliedHistoryRevision {
		t.Fatalf("idempotent apply changed revision: before=%d after=%d", applied.AppliedHistoryRevision, retry.AppliedHistoryRevision)
	}
	revision, err := store.GetHistoryRevision(ctx, conversationID)
	if err != nil {
		t.Fatal(err)
	}
	if revision != applied.AppliedHistoryRevision {
		t.Fatalf("session and submission revisions diverged: got=%d applied=%d", revision, applied.AppliedHistoryRevision)
	}
}

func TestConversationStartSubmissionIdempotencyAndHistoryRevision(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	turnID := "turn-1"
	prompt := "start a conversation"
	fingerprint := SubmissionFingerprint(prompt, SubmissionQueue)
	request := &ConversationStartRequest{
		Session: SessionRecord{
			ID:         "conv_persist_01",
			WorkingDir: "/test/conversation-workspace",
			Model:      "test-model",
		},
		StartID:            "start-01",
		PayloadFingerprint: fingerprint,
		FirstSubmission: SubmissionRecord{
			SubmissionID:       "sub-01",
			Prompt:             prompt,
			Mode:               SubmissionQueue,
			PayloadFingerprint: fingerprint,
			TurnID:             &turnID,
		},
	}

	created, err := store.CreateConversation(ctx, request)
	if err != nil {
		t.Fatalf("CreateConversation failed: %v", err)
	}
	if created.ConversationID != request.Session.ID || created.TurnID != turnID || created.Existing {
		t.Fatalf("unexpected create result: %+v", created)
	}

	retried, err := store.CreateConversation(ctx, request)
	if err != nil {
		t.Fatalf("idempotent CreateConversation retry failed: %v", err)
	}
	if !retried.Existing || retried.ConversationID != created.ConversationID || retried.TurnID != turnID {
		t.Fatalf("retry did not return original conversation: %+v", retried)
	}

	changedStart := *request
	changedStart.FirstSubmission.Prompt = "different prompt"
	if _, err := store.CreateConversation(ctx, &changedStart); !errors.Is(err, ErrSubmissionConflict) {
		t.Fatalf("expected changed start payload conflict, got %v", err)
	}

	revision, err := store.GetHistoryRevision(ctx, created.ConversationID)
	if err != nil || revision != 1 {
		t.Fatalf("expected initial history revision 1, got revision=%d err=%v", revision, err)
	}

	second := &SubmissionRecord{
		ConversationID: created.ConversationID,
		SubmissionID:   "sub-02",
		Prompt:         "follow-up prompt",
		Mode:           SubmissionQueue,
	}
	accepted, err := store.AdmitSubmission(ctx, second)
	if err != nil {
		t.Fatalf("AdmitSubmission failed: %v", err)
	}
	if accepted.Sequence != 2 || accepted.HistoryRevision != 2 || accepted.Status != SubmissionQueued {
		t.Fatalf("unexpected accepted submission: %+v", accepted)
	}
	duplicate, err := store.AdmitSubmission(ctx, &SubmissionRecord{
		ConversationID: created.ConversationID,
		SubmissionID:   "sub-02",
		Prompt:         "follow-up prompt",
		Mode:           SubmissionQueue,
	})
	if err != nil || duplicate.Sequence != 2 {
		t.Fatalf("same-payload retry should return original submission: record=%+v err=%v", duplicate, err)
	}
	if _, err := store.AdmitSubmission(ctx, &SubmissionRecord{
		ConversationID: created.ConversationID,
		SubmissionID:   "sub-02",
		Prompt:         "changed prompt",
		Mode:           SubmissionQueue,
	}); !errors.Is(err, ErrSubmissionConflict) {
		t.Fatalf("expected changed submission payload conflict, got %v", err)
	}

	if err := store.UpdateSubmissionStatus(ctx, created.ConversationID, "sub-02", SubmissionAttached, "turn-2"); err != nil {
		t.Fatalf("failed to attach submission to turn: %v", err)
	}
	if err := store.UpdateSubmissionStatus(ctx, created.ConversationID, "sub-02", SubmissionConsumed, "turn-2"); err != nil {
		t.Fatalf("failed to consume submission: %v", err)
	}
	if err := store.UpdateSubmissionStatus(ctx, created.ConversationID, "sub-02", SubmissionQueued, ""); err == nil {
		t.Fatal("expected invalid submission status transition to fail")
	}

	throughOne, err := store.ListSubmissionsThroughRevision(ctx, created.ConversationID, 1)
	if err != nil || len(throughOne) != 1 {
		t.Fatalf("revision 1 should expose only the first prompt: count=%d err=%v", len(throughOne), err)
	}
	throughTwo, err := store.ListSubmissionsThroughRevision(ctx, created.ConversationID, 2)
	if err != nil || len(throughTwo) != 2 {
		t.Fatalf("revision 2 should expose both prompts: count=%d err=%v", len(throughTwo), err)
	}

	listed, err := store.ListConversations(ctx, request.Session.WorkingDir, 10)
	if err != nil || len(listed) != 1 || listed[0].InteractionKind != InteractionConversation {
		t.Fatalf("expected only the M9 conversation in the conversation list: rows=%+v err=%v", listed, err)
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

func TestSecretRedaction_SessionAndSnapshot(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	apiKey := "sk-proj-abc1234567890123456789012345678901234567890"
	sess := &SessionRecord{
		ID:         "sess_secret_test",
		CreatedAt:  time.Now().UTC(),
		UpdatedAt:  time.Now().UTC(),
		Status:     StatusRunning,
		Task:       fmt.Sprintf("Run deploy with key %s", apiKey),
		Model:      "gpt-4o",
		WorkingDir: "/test/workspace",
	}

	err := store.CreateSession(ctx, sess)
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	loadedSess, err := store.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}
	if strings.Contains(loadedSess.Task, apiKey) {
		t.Errorf("expected task to be redacted, got: %s", loadedSess.Task)
	}
	if !strings.Contains(loadedSess.Task, "sk-[REDACTED]") {
		t.Errorf("expected sk-[REDACTED] in task, got: %s", loadedSess.Task)
	}

	snap := &SnapshotRecord{
		SessionID:          sess.ID,
		StepNumber:         1,
		DistilledNarrative: fmt.Sprintf("Narrative with secret %s", apiKey),
		StateTrackerJSON:   fmt.Sprintf(`{"root_goal":"deploy with %s"}`, apiKey),
		CurrentTokens:      100,
		CreatedAt:          time.Now().UTC(),
	}
	err = store.SaveSnapshot(ctx, snap)
	if err != nil {
		t.Fatalf("SaveSnapshot failed: %v", err)
	}

	loadedSnap, err := store.GetLatestSnapshot(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetLatestSnapshot failed: %v", err)
	}
	if strings.Contains(loadedSnap.DistilledNarrative, apiKey) {
		t.Errorf("expected narrative to be redacted, got: %s", loadedSnap.DistilledNarrative)
	}
	if strings.Contains(loadedSnap.StateTrackerJSON, apiKey) {
		t.Errorf("expected state tracker json to be redacted, got: %s", loadedSnap.StateTrackerJSON)
	}
}

func TestMemoryStoreSharedBetweenPools(t *testing.T) {
	store, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("failed to open :memory: store: %v", err)
	}
	defer func() { _ = store.Close() }()

	ctx := context.Background()
	now := time.Now().UTC()
	sess := &SessionRecord{
		ID:         "sess_pure_mem",
		CreatedAt:  now,
		UpdatedAt:  now,
		Status:     StatusRunning,
		Task:       "Test in-memory pool sharing",
		Model:      "gpt-4o",
		WorkingDir: "/test/workspace",
	}

	err = store.CreateSession(ctx, sess)
	if err != nil {
		t.Fatalf("CreateSession failed on :memory: store: %v", err)
	}

	loaded, err := store.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetSession failed on readerDB for :memory: store: %v", err)
	}
	if loaded.Task != sess.Task {
		t.Errorf("expected task %q, got %q", sess.Task, loaded.Task)
	}
}

func TestSecretRedaction_URIAndJSONBoundaries(t *testing.T) {
	// 1. Connection URI password redaction
	uri := "postgres://admin:secret123@db.example.com:5432/buddy"
	redactedURI := RedactSecrets(uri)
	expectedURI := "postgres://admin:[REDACTED]@db.example.com:5432/buddy"
	if redactedURI != expectedURI {
		t.Errorf("expected %q, got %q", expectedURI, redactedURI)
	}

	// 2. Unrelated URL and email address must not be matched across boundaries
	sentence := "See https://example.com and send reports to dev@example.com"
	redactedSentence := RedactSecrets(sentence)
	if redactedSentence != sentence {
		t.Errorf("expected sentence with URL and email to be unchanged, got %q", redactedSentence)
	}

	// 3. RedactJSON with root_goal URL and email error must preserve valid JSON and redact URI secrets
	rawJSON := `{"root_goal":"https://example.com","recent_errors":["contact: dev@example.com","failed postgres://admin:secret_pass@db:5432/prod"]}`
	redactedJSON := RedactJSON(rawJSON)

	var parsed struct {
		RootGoal     string   `json:"root_goal"`
		RecentErrors []string `json:"recent_errors"`
	}
	if err := json.Unmarshal([]byte(redactedJSON), &parsed); err != nil {
		t.Fatalf("RedactJSON produced invalid JSON: %v, output: %s", err, redactedJSON)
	}
	if parsed.RootGoal != "https://example.com" {
		t.Errorf("expected root_goal unchanged, got %q", parsed.RootGoal)
	}
	if len(parsed.RecentErrors) != 2 {
		t.Fatalf("expected 2 recent errors, got %d", len(parsed.RecentErrors))
	}
	if parsed.RecentErrors[0] != "contact: dev@example.com" {
		t.Errorf("expected email intact, got %q", parsed.RecentErrors[0])
	}
	if !strings.Contains(parsed.RecentErrors[1], "postgres://admin:[REDACTED]@db:5432/prod") {
		t.Errorf("expected DB password to be redacted in recent errors, got %q", parsed.RecentErrors[1])
	}

	// 4. Test SaveSnapshot preserves tracker JSON validity through storage
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	sess := &SessionRecord{
		ID:         "sess_json_snap",
		CreatedAt:  now,
		UpdatedAt:  now,
		Status:     StatusRunning,
		Task:       "Test JSON snapshot safety",
		Model:      "gpt-4o",
		WorkingDir: "/test/workspace",
	}
	if err := store.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	snap := &SnapshotRecord{
		SessionID:          sess.ID,
		StepNumber:         1,
		DistilledNarrative: "Narrative with https://example.com and dev@example.com",
		StateTrackerJSON:   rawJSON,
		CurrentTokens:      100,
		CreatedAt:          now,
	}
	if err := store.SaveSnapshot(ctx, snap); err != nil {
		t.Fatalf("SaveSnapshot failed: %v", err)
	}

	loadedSnap, err := store.GetLatestSnapshot(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetLatestSnapshot failed: %v", err)
	}

	var loadedData struct {
		RootGoal     string   `json:"root_goal"`
		RecentErrors []string `json:"recent_errors"`
	}
	if err := json.Unmarshal([]byte(loadedSnap.StateTrackerJSON), &loadedData); err != nil {
		t.Fatalf("loaded snapshot contains invalid tracker JSON: %v, raw: %s", err, loadedSnap.StateTrackerJSON)
	}
	if loadedData.RootGoal != "https://example.com" {
		t.Errorf("expected loaded root_goal %q, got %q", "https://example.com", loadedData.RootGoal)
	}
	if loadedData.RecentErrors[0] != "contact: dev@example.com" {
		t.Errorf("expected loaded email intact, got %q", loadedData.RecentErrors[0])
	}
}

func TestHierarchicalSessionsAndTriggerCascade(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// 1. Create parent session (depth = 0)
	parentSess := &SessionRecord{
		ID:         "sess_parent_001",
		CreatedAt:  now,
		UpdatedAt:  now,
		Status:     StatusRunning,
		Task:       "Parent task",
		Model:      "gpt-4o",
		WorkingDir: "/test/workspace",
		MaxSteps:   30,
		Role:       "root",
		Depth:      0,
	}
	if err := store.CreateSession(ctx, parentSess); err != nil {
		t.Fatalf("CreateSession (parent) failed: %v", err)
	}

	// 2. Create child session (depth = 1)
	childSess := &SessionRecord{
		ID:              "sess_child_001",
		CreatedAt:       now.Add(1 * time.Second),
		UpdatedAt:       now.Add(1 * time.Second),
		Status:          StatusRunning,
		Task:            "Child sub-task",
		Model:           "gpt-4o",
		WorkingDir:      "/test/workspace",
		MaxSteps:        10,
		ParentSessionID: &parentSess.ID,
		Role:            "researcher",
		Depth:           1,
	}
	if err := store.CreateSession(ctx, childSess); err != nil {
		t.Fatalf("CreateSession (child) failed: %v", err)
	}

	// 3. Create a turn in the child session
	childTurn := &TurnRecord{
		SessionID:  childSess.ID,
		StepNumber: 1,
		Role:       "assistant",
		Content:    "Child research content",
		CreatedAt:  now.Add(2 * time.Second),
	}
	if err := store.CommitTurn(ctx, childTurn, 1, TokenUsage{TotalTokens: 150}); err != nil {
		t.Fatalf("CommitTurn (child) failed: %v", err)
	}

	// 4. Verify GetChildSessions
	children, err := store.GetChildSessions(ctx, parentSess.ID)
	if err != nil {
		t.Fatalf("GetChildSessions failed: %v", err)
	}
	if len(children) != 1 {
		t.Fatalf("expected 1 child session, got %d", len(children))
	}
	if children[0].ID != childSess.ID || children[0].Role != "researcher" || children[0].Depth != 1 {
		t.Errorf("unexpected child session fields: %+v", children[0])
	}

	// 5. Verify ListSessions defaults to depth = 0 (excludes child)
	rootList, err := store.ListSessions(ctx, "/test/workspace", 10)
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}
	if len(rootList) != 1 || rootList[0].ID != parentSess.ID {
		t.Errorf("expected 1 root session in ListSessions, got: %d", len(rootList))
	}

	// 6. Verify ListAllSessions includes both
	allList, err := store.ListAllSessions(ctx, "/test/workspace", 10)
	if err != nil {
		t.Fatalf("ListAllSessions failed: %v", err)
	}
	if len(allList) != 2 {
		t.Errorf("expected 2 sessions in ListAllSessions, got: %d", len(allList))
	}

	// 7. Test cascading delete of child sessions via SQLite trigger
	// Delete parent session directly
	_, err = store.writerDB.ExecContext(ctx, "DELETE FROM sessions WHERE id = ?;", parentSess.ID)
	if err != nil {
		t.Fatalf("failed to delete parent session: %v", err)
	}

	// Verify parent is deleted
	if _, err := store.GetSession(ctx, parentSess.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("expected parent session not found, got %v", err)
	}

	// Verify child session was cascade-deleted by trigger trg_cascade_delete_sessions
	if _, err := store.GetSession(ctx, childSess.ID); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("expected child session to be cascade-deleted, got %v", err)
	}

	// Verify child turns were cascade-deleted via turns foreign key
	childTurns, err := store.GetAllTurns(ctx, childSess.ID)
	if err != nil {
		t.Fatalf("GetAllTurns failed: %v", err)
	}
	if len(childTurns) != 0 {
		t.Errorf("expected 0 child turns after cascade delete, got %d", len(childTurns))
	}
}

func TestIncrementSessionTokens(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	sess := &SessionRecord{
		ID:         "sess_increment_tokens",
		CreatedAt:  now,
		UpdatedAt:  now,
		Status:     StatusRunning,
		Task:       "Token increment test",
		Model:      "gpt-4o",
		WorkingDir: "/test/workspace",
		TokenUsage: TokenUsage{
			PromptTokens:     100,
			CompletionTokens: 50,
			TotalTokens:      150,
		},
	}
	if err := store.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	// Increment tokens
	delta := TokenUsage{
		PromptTokens:     200,
		CompletionTokens: 100,
		ReasoningTokens:  20,
		TotalTokens:      320,
	}
	if err := store.IncrementSessionTokens(ctx, sess.ID, delta); err != nil {
		t.Fatalf("IncrementSessionTokens failed: %v", err)
	}

	loaded, err := store.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetSession failed: %v", err)
	}

	if loaded.TokenUsage.PromptTokens != 300 {
		t.Errorf("expected PromptTokens 300, got %d", loaded.TokenUsage.PromptTokens)
	}
	if loaded.TokenUsage.CompletionTokens != 150 {
		t.Errorf("expected CompletionTokens 150, got %d", loaded.TokenUsage.CompletionTokens)
	}
	if loaded.TokenUsage.ReasoningTokens != 20 {
		t.Errorf("expected ReasoningTokens 20, got %d", loaded.TokenUsage.ReasoningTokens)
	}
	if loaded.TokenUsage.TotalTokens != 470 {
		t.Errorf("expected TotalTokens 470, got %d", loaded.TokenUsage.TotalTokens)
	}
}
