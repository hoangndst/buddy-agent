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
	"time"

	// sqlite driver registration
	_ "modernc.org/sqlite"
)

// SQLiteStore implements Store using modernc.org/sqlite with dual connection pools.
type SQLiteStore struct {
	writerDB *sql.DB // MaxOpenConns = 1 (all writes)
	readerDB *sql.DB // MaxOpenConns = 4 (concurrent reads)
	dbPath   string
	isMemory bool
	mu       sync.Mutex
}

// NewSQLiteStore initializes and connects the SQLite persistence engine.
func NewSQLiteStore(dbPath string) (*SQLiteStore, error) {
	isMemory := dbPath == ":memory:" || strings.Contains(dbPath, "mode=memory")

	if !isMemory {
		if err := CheckOrQuarantine(dbPath); err != nil {
			return nil, fmt.Errorf("quarantine check failed: %w", err)
		}

		dbDir := filepath.Dir(dbPath)
		if err := os.MkdirAll(dbDir, 0700); err != nil {
			return nil, fmt.Errorf("failed to create database directory: %w", err)
		}

		// Ensure .buddy/.gitignore exists to prevent accidental Git commits
		gitignorePath := filepath.Join(dbDir, ".gitignore")
		if _, err := os.Stat(gitignorePath); os.IsNotExist(err) {
			_ = os.WriteFile(gitignorePath, []byte("*\n"), 0644)
		}

		// Pre-create file with 0600 mode
		if _, err := os.Stat(dbPath); os.IsNotExist(err) {
			f, err := os.OpenFile(dbPath, os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				return nil, fmt.Errorf("failed to pre-create database file with 0600 mode: %w", err)
			}
			_ = f.Close()
		}
	}

	var writerDSN, readerDSN string
	if isMemory {
		writerDSN = dbPath
		readerDSN = dbPath
	} else {
		writerDSN = fmt.Sprintf(
			"file:%s?_pragma=busy_timeout(2500)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_pragma=auto_vacuum(INCREMENTAL)&_txlock=immediate",
			dbPath,
		)
		readerDSN = fmt.Sprintf(
			"file:%s?_pragma=busy_timeout(2500)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=query_only(ON)",
			dbPath,
		)
	}

	writer, err := sql.Open("sqlite", writerDSN)
	if err != nil {
		return nil, fmt.Errorf("failed to open writer connection: %w", err)
	}
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)
	writer.SetConnMaxLifetime(0)

	reader, err := sql.Open("sqlite", readerDSN)
	if err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("failed to open reader connection: %w", err)
	}
	if isMemory {
		reader.SetMaxOpenConns(1)
	} else {
		reader.SetMaxOpenConns(4)
		reader.SetMaxIdleConns(4)
	}

	store := &SQLiteStore{
		writerDB: writer,
		readerDB: reader,
		dbPath:   dbPath,
		isMemory: isMemory,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := store.Migrate(ctx); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("failed to run database migrations: %w", err)
	}

	return store, nil
}

// Migrate executes pending schema migrations using PRAGMA user_version.
func (s *SQLiteStore) Migrate(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var currentVersion int
	if err := s.writerDB.QueryRowContext(ctx, "PRAGMA user_version;").Scan(&currentVersion); err != nil {
		return fmt.Errorf("failed to read PRAGMA user_version: %w", err)
	}

	if currentVersion < 1 {
		migration001 := `
		CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			status TEXT NOT NULL,
			task TEXT NOT NULL,
			model TEXT NOT NULL,
			working_dir TEXT NOT NULL,
			max_steps INTEGER NOT NULL DEFAULT 30,
			total_steps INTEGER DEFAULT 0,
			prompt_tokens INTEGER DEFAULT 0,
			completion_tokens INTEGER DEFAULT 0,
			reasoning_tokens INTEGER DEFAULT 0,
			total_tokens INTEGER DEFAULT 0,
			error_message TEXT
		);

		CREATE INDEX IF NOT EXISTS idx_sessions_working_dir ON sessions(working_dir, updated_at DESC);

		CREATE TABLE IF NOT EXISTS turns (
			session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			step_number INTEGER NOT NULL,
			role TEXT NOT NULL,
			content TEXT NOT NULL,
			reasoning_content TEXT,
			tool_calls JSON,
			prompt_tokens INTEGER DEFAULT 0,
			completion_tokens INTEGER DEFAULT 0,
			reasoning_tokens INTEGER DEFAULT 0,
			total_tokens INTEGER DEFAULT 0,
			duration_ms INTEGER NOT NULL,
			created_at DATETIME NOT NULL,
			PRIMARY KEY (session_id, step_number)
		);

		CREATE TABLE IF NOT EXISTS episodic_snapshots (
			id INTEGER PRIMARY KEY,
			session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			step_number INTEGER NOT NULL,
			distilled_narrative TEXT NOT NULL,
			state_tracker JSON NOT NULL,
			current_tokens INTEGER NOT NULL,
			created_at DATETIME NOT NULL
		);

		CREATE INDEX IF NOT EXISTS idx_snapshots_session_step ON episodic_snapshots(session_id, step_number DESC);
		`

		tx, err := s.writerDB.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("failed to begin migration transaction: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		if _, err := tx.ExecContext(ctx, migration001); err != nil {
			return fmt.Errorf("failed to execute migration 001 statements: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit migration 001: %w", err)
		}

		// Update user_version outside transaction block
		if _, err := s.writerDB.ExecContext(ctx, "PRAGMA user_version = 1;"); err != nil {
			return fmt.Errorf("failed to bump PRAGMA user_version to 1: %w", err)
		}
	}

	return nil
}

// Close flushes open WAL transactions, runs incremental vacuum, and closes database handles.
func (s *SQLiteStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var errs []error
	if !s.isMemory && s.writerDB != nil {
		_, _ = s.writerDB.Exec("PRAGMA wal_checkpoint(TRUNCATE);")
		_, _ = s.writerDB.Exec("PRAGMA incremental_vacuum(100);")
	}

	if s.readerDB != nil {
		if err := s.readerDB.Close(); err != nil {
			errs = append(errs, err)
		}
		s.readerDB = nil
	}
	if s.writerDB != nil {
		if err := s.writerDB.Close(); err != nil {
			errs = append(errs, err)
		}
		s.writerDB = nil
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}

// CreateSession persists initial session metadata before turn execution begins.
func (s *SQLiteStore) CreateSession(ctx context.Context, session *SessionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	query := `
	INSERT INTO sessions (
		id, created_at, updated_at, status, task, model, working_dir,
		max_steps, total_steps, prompt_tokens, completion_tokens, reasoning_tokens, total_tokens, error_message
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
	`

	errMsg := ""
	if session.ErrorMessage != nil {
		errMsg = *session.ErrorMessage
	}

	_, err := s.writerDB.ExecContext(
		ctx,
		query,
		session.ID,
		session.CreatedAt.UTC(),
		session.UpdatedAt.UTC(),
		string(session.Status),
		session.Task,
		session.Model,
		session.WorkingDir,
		session.MaxSteps,
		session.TotalSteps,
		session.TokenUsage.PromptTokens,
		session.TokenUsage.CompletionTokens,
		session.TokenUsage.ReasoningTokens,
		session.TokenUsage.TotalTokens,
		sql.NullString{String: errMsg, Valid: session.ErrorMessage != nil},
	)
	if err != nil {
		return fmt.Errorf("failed to create session %s: %w", session.ID, err)
	}
	return nil
}

// GetSession retrieves a session record by exact ID or unique short prefix.
func (s *SQLiteStore) GetSession(ctx context.Context, idOrPrefix string) (*SessionRecord, error) {
	trimmed := strings.TrimSpace(idOrPrefix)
	if trimmed == "" {
		return nil, ErrSessionNotFound
	}

	query := `
	SELECT id, created_at, updated_at, status, task, model, working_dir, max_steps, total_steps,
	       prompt_tokens, completion_tokens, reasoning_tokens, total_tokens, error_message
	FROM sessions
	WHERE id LIKE ? || '%'
	ORDER BY updated_at DESC;
	`

	rows, err := s.readerDB.QueryContext(ctx, query, trimmed)
	if err != nil {
		return nil, fmt.Errorf("failed to query sessions for prefix %q: %w", trimmed, err)
	}
	defer func() { _ = rows.Close() }()

	var sessions []*SessionRecord
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(sessions) == 0 {
		return nil, ErrSessionNotFound
	}

	// Check if any candidate is an exact match
	for _, sess := range sessions {
		if sess.ID == trimmed {
			return sess, nil
		}
	}

	if len(sessions) > 1 {
		candidates := make([]string, len(sessions))
		for i, sess := range sessions {
			candidates[i] = sess.ID
		}
		return nil, &AmbiguousIDError{Prefix: trimmed, Candidates: candidates}
	}

	return sessions[0], nil
}

// GetLatestResumableSession fetches the most recent interrupted, failed, or running session in workingDir.
func (s *SQLiteStore) GetLatestResumableSession(ctx context.Context, workingDir string) (*SessionRecord, error) {
	query := `
	SELECT id, created_at, updated_at, status, task, model, working_dir, max_steps, total_steps,
	       prompt_tokens, completion_tokens, reasoning_tokens, total_tokens, error_message
	FROM sessions
	WHERE working_dir = ? AND status IN ('interrupted', 'failed', 'running')
	ORDER BY updated_at DESC
	LIMIT 1;
	`

	row := s.readerDB.QueryRowContext(ctx, query, workingDir)
	sess, err := scanSessionRow(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSessionNotFound
		}
		return nil, fmt.Errorf("failed to query latest resumable session: %w", err)
	}
	return sess, nil
}

// ListSessions returns recent sessions in the specified working directory ordered by updated_at DESC.
func (s *SQLiteStore) ListSessions(ctx context.Context, workingDir string, limit int) ([]*SessionRecord, error) {
	if limit <= 0 {
		limit = 20
	}

	query := `
	SELECT id, created_at, updated_at, status, task, model, working_dir, max_steps, total_steps,
	       prompt_tokens, completion_tokens, reasoning_tokens, total_tokens, error_message
	FROM sessions
	WHERE working_dir = ?
	ORDER BY updated_at DESC
	LIMIT ?;
	`

	rows, err := s.readerDB.QueryContext(ctx, query, workingDir, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to list sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var sessions []*SessionRecord
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, sess)
	}
	return sessions, rows.Err()
}

// UpdateSessionStatus updates session lifecycle status, completed step count, usage, and error.
func (s *SQLiteStore) UpdateSessionStatus(
	ctx context.Context,
	id string,
	status SessionStatus,
	steps int,
	usage TokenUsage,
	errMsg string,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	query := `
	UPDATE sessions
	SET status = ?, total_steps = ?, prompt_tokens = ?, completion_tokens = ?,
	    reasoning_tokens = ?, total_tokens = ?, error_message = ?, updated_at = ?
	WHERE id = ?;
	`

	_, err := s.writerDB.ExecContext(
		ctx,
		query,
		string(status),
		steps,
		usage.PromptTokens,
		usage.CompletionTokens,
		usage.ReasoningTokens,
		usage.TotalTokens,
		sql.NullString{String: errMsg, Valid: errMsg != ""},
		time.Now().UTC(),
		id,
	)
	if err != nil {
		return fmt.Errorf("failed to update session status for %s: %w", id, err)
	}
	return nil
}

// PruneSessions removes sessions older than olderThan and keeps at most keepMax recent sessions.
func (s *SQLiteStore) PruneSessions(ctx context.Context, olderThan time.Duration, keepMax int) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := time.Now().UTC().Add(-olderThan)
	query := `
	DELETE FROM sessions
	WHERE created_at < ?
	  AND id NOT IN (
	      SELECT id FROM sessions ORDER BY created_at DESC LIMIT ?
	  );
	`

	res, err := s.writerDB.ExecContext(ctx, query, cutoff, keepMax)
	if err != nil {
		return 0, fmt.Errorf("failed to prune sessions: %w", err)
	}

	affected, _ := res.RowsAffected()
	if !s.isMemory && affected > 0 {
		_, _ = s.writerDB.ExecContext(ctx, "PRAGMA incremental_vacuum(500);")
	}
	return affected, nil
}

// CommitTurn writes an atomic turn record and updates cumulative session metrics inside a single transaction.
func (s *SQLiteStore) CommitTurn(
	ctx context.Context,
	turn *TurnRecord,
	totalSteps int,
	sessionUsage TokenUsage,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Redact secrets and clamp tool outputs before serialization
	redactedToolCalls := make([]ToolCallRecord, len(turn.ToolCalls))
	for i, tc := range turn.ToolCalls {
		redactedToolCalls[i] = ToolCallRecord{
			ID:       tc.ID,
			Name:     tc.Name,
			Args:     RedactSecrets(tc.Args),
			Result:   SanitizeToolOutput(tc.Result),
			ExitCode: tc.ExitCode,
		}
	}

	var toolCallsJSON *string
	if len(redactedToolCalls) > 0 {
		bytes, err := json.Marshal(redactedToolCalls)
		if err != nil {
			return fmt.Errorf("failed to marshal tool calls for turn %d: %w", turn.StepNumber, err)
		}
		str := string(bytes)
		toolCallsJSON = &str
	}

	redactedContent := RedactSecrets(turn.Content)
	redactedReasoning := RedactSecrets(turn.ReasoningContent)

	tx, err := s.writerDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin turn transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	turnQuery := `
	INSERT INTO turns (
		session_id, step_number, role, content, reasoning_content, tool_calls,
		prompt_tokens, completion_tokens, reasoning_tokens, total_tokens, duration_ms, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
	`

	_, err = tx.ExecContext(
		ctx,
		turnQuery,
		turn.SessionID,
		turn.StepNumber,
		turn.Role,
		redactedContent,
		sql.NullString{String: redactedReasoning, Valid: redactedReasoning != ""},
		sql.NullString{String: derefString(toolCallsJSON), Valid: toolCallsJSON != nil},
		turn.TokenUsage.PromptTokens,
		turn.TokenUsage.CompletionTokens,
		turn.TokenUsage.ReasoningTokens,
		turn.TokenUsage.TotalTokens,
		turn.DurationMs,
		turn.CreatedAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("failed to insert turn %d: %w", turn.StepNumber, err)
	}

	sessionQuery := `
	UPDATE sessions
	SET total_steps = ?, prompt_tokens = ?, completion_tokens = ?,
	    reasoning_tokens = ?, total_tokens = ?, updated_at = ?
	WHERE id = ?;
	`

	_, err = tx.ExecContext(
		ctx,
		sessionQuery,
		totalSteps,
		sessionUsage.PromptTokens,
		sessionUsage.CompletionTokens,
		sessionUsage.ReasoningTokens,
		sessionUsage.TotalTokens,
		time.Now().UTC(),
		turn.SessionID,
	)
	if err != nil {
		return fmt.Errorf("failed to update session metrics for turn %d: %w", turn.StepNumber, err)
	}

	return tx.Commit()
}

// GetTurnsAfterStep retrieves turns for a session strictly after afterStep ordered by step_number ASC.
func (s *SQLiteStore) GetTurnsAfterStep(ctx context.Context, sessionID string, afterStep int) ([]*TurnRecord, error) {
	query := `
	SELECT session_id, step_number, role, content, reasoning_content, tool_calls,
	       prompt_tokens, completion_tokens, reasoning_tokens, total_tokens, duration_ms, created_at
	FROM turns
	WHERE session_id = ? AND step_number > ?
	ORDER BY step_number ASC;
	`

	rows, err := s.readerDB.QueryContext(ctx, query, sessionID, afterStep)
	if err != nil {
		return nil, fmt.Errorf("failed to query turns after step %d: %w", afterStep, err)
	}
	defer func() { _ = rows.Close() }()

	var turns []*TurnRecord
	for rows.Next() {
		turn, err := scanTurn(rows)
		if err != nil {
			return nil, err
		}
		turns = append(turns, turn)
	}
	return turns, rows.Err()
}

// GetAllTurns returns all committed turns for a session ordered by step_number ASC.
func (s *SQLiteStore) GetAllTurns(ctx context.Context, sessionID string) ([]*TurnRecord, error) {
	return s.GetTurnsAfterStep(ctx, sessionID, 0)
}

// SaveSnapshot saves an immutable episodic state snapshot generated during context compaction.
func (s *SQLiteStore) SaveSnapshot(ctx context.Context, snapshot *SnapshotRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	query := `
	INSERT INTO episodic_snapshots (
		session_id, step_number, distilled_narrative, state_tracker, current_tokens, created_at
	) VALUES (?, ?, ?, ?, ?, ?);
	`

	res, err := s.writerDB.ExecContext(
		ctx,
		query,
		snapshot.SessionID,
		snapshot.StepNumber,
		snapshot.DistilledNarrative,
		snapshot.StateTrackerJSON,
		snapshot.CurrentTokens,
		snapshot.CreatedAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("failed to insert snapshot at step %d: %w", snapshot.StepNumber, err)
	}

	id, _ := res.LastInsertId()
	snapshot.ID = id
	return nil
}

// GetLatestSnapshot returns the most recent episodic snapshot for a session, or nil if none exists.
func (s *SQLiteStore) GetLatestSnapshot(ctx context.Context, sessionID string) (*SnapshotRecord, error) {
	query := `
	SELECT id, session_id, step_number, distilled_narrative, state_tracker, current_tokens, created_at
	FROM episodic_snapshots
	WHERE session_id = ?
	ORDER BY step_number DESC
	LIMIT 1;
	`

	row := s.readerDB.QueryRowContext(ctx, query, sessionID)
	var snap SnapshotRecord
	err := row.Scan(
		&snap.ID,
		&snap.SessionID,
		&snap.StepNumber,
		&snap.DistilledNarrative,
		&snap.StateTrackerJSON,
		&snap.CurrentTokens,
		&snap.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get latest snapshot: %w", err)
	}
	return &snap, nil
}

func scanSession(scanner interface{ Scan(...interface{}) error }) (*SessionRecord, error) {
	var (
		sess   SessionRecord
		status string
		errMsg sql.NullString
	)

	err := scanner.Scan(
		&sess.ID,
		&sess.CreatedAt,
		&sess.UpdatedAt,
		&status,
		&sess.Task,
		&sess.Model,
		&sess.WorkingDir,
		&sess.MaxSteps,
		&sess.TotalSteps,
		&sess.TokenUsage.PromptTokens,
		&sess.TokenUsage.CompletionTokens,
		&sess.TokenUsage.ReasoningTokens,
		&sess.TokenUsage.TotalTokens,
		&errMsg,
	)
	if err != nil {
		return nil, err
	}

	sess.Status = SessionStatus(status)
	if errMsg.Valid {
		sess.ErrorMessage = &errMsg.String
	}
	return &sess, nil
}

func scanSessionRow(row *sql.Row) (*SessionRecord, error) {
	return scanSession(row)
}

func scanTurn(rows *sql.Rows) (*TurnRecord, error) {
	var (
		turn      TurnRecord
		reasoning sql.NullString
		toolJSON  sql.NullString
	)

	err := rows.Scan(
		&turn.SessionID,
		&turn.StepNumber,
		&turn.Role,
		&turn.Content,
		&reasoning,
		&toolJSON,
		&turn.TokenUsage.PromptTokens,
		&turn.TokenUsage.CompletionTokens,
		&turn.TokenUsage.ReasoningTokens,
		&turn.TokenUsage.TotalTokens,
		&turn.DurationMs,
		&turn.CreatedAt,
	)
	if err != nil {
		return nil, err
	}

	if reasoning.Valid {
		turn.ReasoningContent = reasoning.String
	}
	if toolJSON.Valid && toolJSON.String != "" {
		_ = json.Unmarshal([]byte(toolJSON.String), &turn.ToolCalls)
	}
	return &turn, nil
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
