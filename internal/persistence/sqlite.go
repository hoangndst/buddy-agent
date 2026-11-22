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
	if dbPath == ":memory:" {
		dbPath = fmt.Sprintf("file:buddy_mem_%x?mode=memory&cache=shared", time.Now().UnixNano())
	}

	isMemory := strings.Contains(dbPath, "mode=memory") || strings.HasPrefix(dbPath, "file:mem")

	if !isMemory {
		if err := CheckOrQuarantine(dbPath); err != nil {
			return nil, fmt.Errorf("quarantine check failed: %w", err)
		}

		dbDir := filepath.Dir(dbPath)
		if err := os.MkdirAll(dbDir, 0700); err != nil {
			return nil, fmt.Errorf("failed to create database directory: %w", err)
		}

		// Ensure .buddy/.gitignore exists only within the dedicated .buddy directory
		if filepath.Base(dbDir) == ".buddy" {
			gitignorePath := filepath.Join(dbDir, ".gitignore")
			if _, err := os.Stat(gitignorePath); os.IsNotExist(err) {
				_ = os.WriteFile(gitignorePath, []byte("*\n"), 0644)
			}
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
		if !strings.Contains(dbPath, "foreign_keys") {
			if strings.Contains(dbPath, "?") {
				dbPath += "&_pragma=foreign_keys(ON)"
			} else {
				dbPath += "?_pragma=foreign_keys(ON)"
			}
		}
		if !strings.Contains(dbPath, "busy_timeout") {
			dbPath += "&_pragma=busy_timeout(5000)"
		}
		if !strings.Contains(dbPath, "cache=shared") && strings.HasPrefix(dbPath, "file:") {
			dbPath += "&cache=shared"
		}
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

	var reader *sql.DB
	if isMemory {
		reader = writer
	} else {
		var err error
		reader, err = sql.Open("sqlite", readerDSN)
		if err != nil {
			_ = writer.Close()
			return nil, fmt.Errorf("failed to open reader connection: %w", err)
		}
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

	if currentVersion < 2 {
		migration002 := `
		ALTER TABLE sessions ADD COLUMN parent_session_id TEXT;
		ALTER TABLE sessions ADD COLUMN role TEXT NOT NULL DEFAULT 'root';
		ALTER TABLE sessions ADD COLUMN depth INTEGER NOT NULL DEFAULT 0;

		CREATE INDEX IF NOT EXISTS idx_sessions_parent ON sessions(parent_session_id);
		CREATE INDEX IF NOT EXISTS idx_sessions_depth ON sessions(depth);

		CREATE TRIGGER IF NOT EXISTS trg_cascade_delete_sessions
		AFTER DELETE ON sessions
		FOR EACH ROW
		BEGIN
			DELETE FROM sessions WHERE parent_session_id = OLD.id;
		END;
		`

		tx, err := s.writerDB.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("failed to begin migration 002 transaction: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		if _, err := tx.ExecContext(ctx, migration002); err != nil {
			return fmt.Errorf("failed to execute migration 002 statements: %w", err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit migration 002: %w", err)
		}

		if _, err := s.writerDB.ExecContext(ctx, "PRAGMA user_version = 2;"); err != nil {
			return fmt.Errorf("failed to bump PRAGMA user_version to 2: %w", err)
		}
	}

	if currentVersion < 3 {
		migration003 := `
		ALTER TABLE sessions ADD COLUMN interaction_kind TEXT NOT NULL DEFAULT 'task';
		ALTER TABLE sessions ADD COLUMN history_revision INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE turns ADD COLUMN conversation_turn_id TEXT;
		ALTER TABLE turns ADD COLUMN history_revision INTEGER NOT NULL DEFAULT 0;
		ALTER TABLE episodic_snapshots ADD COLUMN history_revision INTEGER NOT NULL DEFAULT 0;

		CREATE TABLE conversation_starts (
			working_dir TEXT NOT NULL,
			start_id TEXT NOT NULL,
			payload_fingerprint TEXT NOT NULL,
			conversation_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			submission_id TEXT NOT NULL,
			created_at DATETIME NOT NULL,
			PRIMARY KEY (working_dir, start_id)
		);

		CREATE TABLE conversation_submissions (
			conversation_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			submission_id TEXT NOT NULL,
			prompt TEXT NOT NULL,
			mode TEXT NOT NULL CHECK (mode IN ('queue', 'steer')),
			payload_fingerprint TEXT NOT NULL,
			sequence INTEGER NOT NULL,
			turn_id TEXT,
			status TEXT NOT NULL,
			created_at DATETIME NOT NULL,
			history_revision INTEGER NOT NULL,
			PRIMARY KEY (conversation_id, submission_id),
			UNIQUE (conversation_id, sequence)
		);

		CREATE TABLE conversation_turns (
			conversation_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			turn_id TEXT NOT NULL,
			submission_id TEXT NOT NULL,
			status TEXT NOT NULL,
			started_at DATETIME NOT NULL,
			finished_at DATETIME,
			error_message TEXT,
			history_revision INTEGER NOT NULL,
			PRIMARY KEY (conversation_id, turn_id),
			FOREIGN KEY (conversation_id, submission_id)
				REFERENCES conversation_submissions(conversation_id, submission_id)
		);

		CREATE INDEX idx_conversation_submissions_order
			ON conversation_submissions(conversation_id, sequence);
		CREATE INDEX idx_conversation_submissions_revision
			ON conversation_submissions(conversation_id, history_revision, sequence);
		CREATE INDEX idx_conversation_turns_order
			ON conversation_turns(conversation_id, started_at);
		CREATE INDEX idx_turns_conversation_turn
			ON turns(session_id, conversation_turn_id, step_number);
		CREATE INDEX idx_turns_history_revision
			ON turns(session_id, history_revision, step_number);
		CREATE INDEX idx_conversation_starts_id
			ON conversation_starts(working_dir, created_at DESC);
		`

		tx, err := s.writerDB.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("failed to begin migration 003 transaction: %w", err)
		}
		defer func() { _ = tx.Rollback() }()

		if _, err := tx.ExecContext(ctx, migration003); err != nil {
			return fmt.Errorf("failed to execute migration 003 statements: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit migration 003: %w", err)
		}
		if _, err := s.writerDB.ExecContext(ctx, "PRAGMA user_version = 3;"); err != nil {
			return fmt.Errorf("failed to bump PRAGMA user_version to 3: %w", err)
		}
	}

	if currentVersion < 4 {
		migration004 := `
		ALTER TABLE conversation_submissions ADD COLUMN applied_after_step INTEGER;
		ALTER TABLE conversation_submissions ADD COLUMN applied_history_revision INTEGER NOT NULL DEFAULT 0;
		UPDATE conversation_submissions
		SET applied_after_step = 0, applied_history_revision = history_revision
		WHERE status IN ('attached', 'consumed');
		`
		tx, err := s.writerDB.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("failed to begin migration 004 transaction: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, migration004); err != nil {
			return fmt.Errorf("failed to execute migration 004 statements: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit migration 004: %w", err)
		}
		if _, err := s.writerDB.ExecContext(ctx, "PRAGMA user_version = 4;"); err != nil {
			return fmt.Errorf("failed to bump PRAGMA user_version to 4: %w", err)
		}
	}

	if currentVersion < 5 {
		migration005 := `
		CREATE TABLE conversation_provider_profiles (
			conversation_id TEXT PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
			base_url TEXT NOT NULL,
			api_type TEXT NOT NULL,
			model TEXT NOT NULL,
			reasoning_effort TEXT NOT NULL,
			max_steps INTEGER NOT NULL,
			max_context_tokens INTEGER NOT NULL,
			max_output_chars INTEGER NOT NULL,
			working_tail_size INTEGER NOT NULL,
			requires_mcp_config INTEGER NOT NULL DEFAULT 0,
			requires_env INTEGER NOT NULL DEFAULT 0,
			credential_source TEXT NOT NULL CHECK (credential_source IN ('daemon-default', 'ephemeral'))
		);
		`
		tx, err := s.writerDB.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("failed to begin migration 005 transaction: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		if _, err := tx.ExecContext(ctx, migration005); err != nil {
			return fmt.Errorf("failed to execute migration 005 statements: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit migration 005: %w", err)
		}
		if _, err := s.writerDB.ExecContext(ctx, "PRAGMA user_version = 5;"); err != nil {
			return fmt.Errorf("failed to bump PRAGMA user_version to 5: %w", err)
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

	if s.readerDB != nil && s.readerDB != s.writerDB {
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

// DatabasePath returns the canonical SQLite DSN/path owned by this store.
func (s *SQLiteStore) DatabasePath() string { return s.dbPath }

// CreateSession persists initial session metadata before turn execution begins.
func (s *SQLiteStore) CreateSession(ctx context.Context, session *SessionRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	query := `
	INSERT INTO sessions (
		id, created_at, updated_at, status, task, model, working_dir,
		max_steps, total_steps, prompt_tokens, completion_tokens, reasoning_tokens, total_tokens, error_message,
		parent_session_id, role, depth, interaction_kind
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
	`

	errMsg := ""
	if session.ErrorMessage != nil {
		errMsg = RedactSecrets(*session.ErrorMessage)
	}
	redactedTask := RedactSecrets(session.Task)
	session.Task = redactedTask

	role := session.Role
	if role == "" {
		role = "root"
	}
	interactionKind := session.InteractionKind
	if interactionKind == "" {
		interactionKind = InteractionTask
	}
	if interactionKind != InteractionTask && interactionKind != InteractionConversation {
		return fmt.Errorf("invalid interaction kind %q", interactionKind)
	}
	session.InteractionKind = interactionKind
	var parentID sql.NullString
	if session.ParentSessionID != nil && *session.ParentSessionID != "" {
		parentID = sql.NullString{String: *session.ParentSessionID, Valid: true}
	}

	_, err := s.writerDB.ExecContext(
		ctx,
		query,
		session.ID,
		session.CreatedAt.UTC(),
		session.UpdatedAt.UTC(),
		string(session.Status),
		redactedTask,
		session.Model,
		session.WorkingDir,
		session.MaxSteps,
		session.TotalSteps,
		session.TokenUsage.PromptTokens,
		session.TokenUsage.CompletionTokens,
		session.TokenUsage.ReasoningTokens,
		session.TokenUsage.TotalTokens,
		sql.NullString{String: errMsg, Valid: session.ErrorMessage != nil},
		parentID,
		role,
		session.Depth,
		string(interactionKind),
	)
	if err != nil {
		return fmt.Errorf("failed to create session %s: %w", session.ID, err)
	}
	return nil
}

// CreateConversation atomically creates a conversation and accepts its first prompt.
// start_id is scoped to the canonical working directory and makes creation retry-safe.
func (s *SQLiteStore) CreateConversation(ctx context.Context, request *ConversationStartRequest) (*ConversationStartResult, error) {
	if request == nil || request.StartID == "" || request.Session.ID == "" || request.Session.WorkingDir == "" || request.FirstSubmission.SubmissionID == "" || request.FirstSubmission.Prompt == "" {
		return nil, errors.New("conversation start requires workspace, start ID, session ID, and first prompt")
	}
	if request.Session.Depth != 0 || request.Session.ParentSessionID != nil {
		return nil, errors.New("conversations must be root sessions")
	}
	if request.FirstSubmission.Mode != "" && request.FirstSubmission.Mode != SubmissionQueue {
		return nil, errors.New("first conversation prompt must use queue mode")
	}
	fingerprint := SubmissionFingerprint(request.FirstSubmission.Prompt, SubmissionQueue)
	if request.PayloadFingerprint != "" && request.PayloadFingerprint != fingerprint {
		return nil, ErrSubmissionConflict
	}
	if request.FirstSubmission.PayloadFingerprint != "" && request.FirstSubmission.PayloadFingerprint != fingerprint {
		return nil, ErrSubmissionConflict
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.writerDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin conversation creation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var existingFingerprint, existingConversationID, existingSubmissionID string
	err = tx.QueryRowContext(ctx, `
		SELECT payload_fingerprint, conversation_id, submission_id
		FROM conversation_starts WHERE working_dir = ? AND start_id = ?;
	`, request.Session.WorkingDir, request.StartID).Scan(&existingFingerprint, &existingConversationID, &existingSubmissionID)
	if err == nil {
		if existingFingerprint != fingerprint {
			return nil, ErrSubmissionConflict
		}
		var turnID sql.NullString
		_ = tx.QueryRowContext(ctx, `SELECT turn_id FROM conversation_submissions WHERE conversation_id = ? AND submission_id = ?`, existingConversationID, existingSubmissionID).Scan(&turnID)
		return &ConversationStartResult{ConversationID: existingConversationID, SubmissionID: existingSubmissionID, TurnID: turnID.String, Existing: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("failed to check conversation start key: %w", err)
	}

	session := request.Session
	session.InteractionKind = InteractionConversation
	session.Task = request.FirstSubmission.Prompt
	if session.Status == "" {
		session.Status = StatusRunning
	}
	if session.Role == "" {
		session.Role = "root"
	}
	if session.CreatedAt.IsZero() {
		session.CreatedAt = time.Now().UTC()
	}
	if session.UpdatedAt.IsZero() {
		session.UpdatedAt = session.CreatedAt
	}
	redactedTask := RedactSecrets(session.Task)
	var parentID sql.NullString
	if session.ParentSessionID != nil && *session.ParentSessionID != "" {
		parentID = sql.NullString{String: *session.ParentSessionID, Valid: true}
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO sessions (
			id, created_at, updated_at, status, task, model, working_dir,
			max_steps, total_steps, prompt_tokens, completion_tokens, reasoning_tokens, total_tokens, error_message,
			parent_session_id, role, depth, interaction_kind, history_revision
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1);
	`, session.ID, session.CreatedAt.UTC(), session.UpdatedAt.UTC(), string(session.Status), redactedTask,
		session.Model, session.WorkingDir, session.MaxSteps, session.TotalSteps,
		session.TokenUsage.PromptTokens, session.TokenUsage.CompletionTokens, session.TokenUsage.ReasoningTokens,
		session.TokenUsage.TotalTokens, sql.NullString{}, parentID, session.Role, session.Depth, string(InteractionConversation))
	if err != nil {
		return nil, fmt.Errorf("failed to create conversation session %s: %w", session.ID, err)
	}

	first := request.FirstSubmission
	first.ConversationID = session.ID
	first.Sequence = 1
	first.HistoryRevision = 1
	first.PayloadFingerprint = fingerprint
	if first.Mode == "" {
		first.Mode = SubmissionQueue
	}
	if first.Status == "" {
		first.Status = SubmissionAttached
	}
	if first.Status != SubmissionAttached {
		return nil, errors.New("first conversation prompt must attach to its initial turn")
	}
	if first.CreatedAt.IsZero() {
		first.CreatedAt = session.CreatedAt
	}
	first.AppliedAfterStep = new(int)
	first.AppliedHistoryRevision = 1
	if first.PayloadFingerprint == "" {
		first.PayloadFingerprint = request.PayloadFingerprint
	}
	first.Prompt = RedactSecrets(first.Prompt)
	if err := insertSubmission(ctx, tx, &first); err != nil {
		return nil, fmt.Errorf("failed to persist first conversation prompt: %w", err)
	}
	if first.TurnID == nil || *first.TurnID == "" {
		return nil, errors.New("first conversation prompt requires a turn ID")
	}
	conversationTurn := &ConversationTurnRecord{
		ConversationID:  session.ID,
		TurnID:          *first.TurnID,
		SubmissionID:    first.SubmissionID,
		Status:          StatusRunning,
		StartedAt:       first.CreatedAt,
		HistoryRevision: 1,
	}
	if err := insertConversationTurn(ctx, tx, conversationTurn); err != nil {
		return nil, fmt.Errorf("failed to persist first conversation turn: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO conversation_starts (working_dir, start_id, payload_fingerprint, conversation_id, submission_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?);
	`, session.WorkingDir, request.StartID, fingerprint, session.ID, first.SubmissionID, session.CreatedAt.UTC())
	if err != nil {
		return nil, fmt.Errorf("failed to persist conversation start key: %w", err)
	}
	if request.ProviderProfile != nil {
		if err := insertConversationProviderProfile(ctx, tx, session.ID, request.ProviderProfile); err != nil {
			return nil, fmt.Errorf("failed to persist conversation provider profile: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit conversation creation: %w", err)
	}
	return &ConversationStartResult{ConversationID: session.ID, SubmissionID: first.SubmissionID, TurnID: derefString(first.TurnID)}, nil
}

func insertConversationProviderProfile(ctx context.Context, tx *sql.Tx, conversationID string, profile *ConversationProviderProfile) error {
	if profile == nil || profile.Model == "" || profile.CredentialSource != ConversationCredentialDaemonDefault && profile.CredentialSource != ConversationCredentialEphemeral {
		return errors.New("conversation provider profile requires model and a supported credential source")
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO conversation_provider_profiles (
		conversation_id, base_url, api_type, model, reasoning_effort, max_steps, max_context_tokens,
		max_output_chars, working_tail_size, requires_mcp_config, requires_env, credential_source
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);`, conversationID, profile.BaseURL, profile.APIType, profile.Model,
		profile.ReasoningEffort, profile.MaxSteps, profile.MaxContextTokens, profile.MaxOutputChars, profile.WorkingTailSize, profile.RequiresMCPConfig, profile.RequiresEnv, profile.CredentialSource)
	return err
}

// GetConversationProviderProfile loads the nonsecret provider settings used by a conversation.
func (s *SQLiteStore) GetConversationProviderProfile(ctx context.Context, conversationID string) (*ConversationProviderProfile, error) {
	var profile ConversationProviderProfile
	err := s.readerDB.QueryRowContext(ctx, `SELECT base_url, api_type, model, reasoning_effort, max_steps,
		max_context_tokens, max_output_chars, working_tail_size, requires_mcp_config, requires_env, credential_source
		FROM conversation_provider_profiles WHERE conversation_id = ?;`, conversationID).Scan(
		&profile.BaseURL, &profile.APIType, &profile.Model, &profile.ReasoningEffort, &profile.MaxSteps,
		&profile.MaxContextTokens, &profile.MaxOutputChars, &profile.WorkingTailSize, &profile.RequiresMCPConfig, &profile.RequiresEnv, &profile.CredentialSource)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load conversation provider profile: %w", err)
	}
	return &profile, nil
}

// SaveConversationProviderProfile stores a nonsecret provider profile for a conversation.
func (s *SQLiteStore) SaveConversationProviderProfile(ctx context.Context, conversationID string, profile *ConversationProviderProfile) error {
	if conversationID == "" || profile == nil {
		return errors.New("conversation ID and provider profile are required")
	}
	if profile.Model == "" || profile.CredentialSource != ConversationCredentialDaemonDefault && profile.CredentialSource != ConversationCredentialEphemeral {
		return errors.New("conversation provider profile requires model and a supported credential source")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.writerDB.ExecContext(ctx, `INSERT INTO conversation_provider_profiles (
		conversation_id, base_url, api_type, model, reasoning_effort, max_steps, max_context_tokens,
		max_output_chars, working_tail_size, requires_mcp_config, requires_env, credential_source
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(conversation_id) DO UPDATE SET base_url=excluded.base_url, api_type=excluded.api_type,
	model=excluded.model, reasoning_effort=excluded.reasoning_effort, max_steps=excluded.max_steps,
	max_context_tokens=excluded.max_context_tokens, max_output_chars=excluded.max_output_chars,
	working_tail_size=excluded.working_tail_size, requires_mcp_config=excluded.requires_mcp_config,
	requires_env=excluded.requires_env, credential_source=excluded.credential_source;`,
		conversationID, profile.BaseURL, profile.APIType, profile.Model, profile.ReasoningEffort, profile.MaxSteps,
		profile.MaxContextTokens, profile.MaxOutputChars, profile.WorkingTailSize, profile.RequiresMCPConfig, profile.RequiresEnv, profile.CredentialSource)
	if err != nil {
		return fmt.Errorf("save conversation provider profile: %w", err)
	}
	return nil
}

// AdmitSubmission persists a later prompt before acknowledging it and returns the canonical record.
func (s *SQLiteStore) AdmitSubmission(ctx context.Context, submission *SubmissionRecord) (*SubmissionRecord, error) {
	if submission == nil || submission.ConversationID == "" || submission.SubmissionID == "" || submission.Prompt == "" {
		return nil, errors.New("submission requires conversation ID, submission ID, and prompt")
	}
	if submission.Mode == "" {
		submission.Mode = SubmissionQueue
	}
	if submission.Mode != SubmissionQueue && submission.Mode != SubmissionSteer {
		return nil, fmt.Errorf("unsupported submission mode %q", submission.Mode)
	}
	fingerprint := SubmissionFingerprint(submission.Prompt, submission.Mode)
	if submission.PayloadFingerprint != "" && submission.PayloadFingerprint != fingerprint {
		return nil, ErrSubmissionConflict
	}
	submission.PayloadFingerprint = fingerprint

	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.writerDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin submission admission: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := scanSubmission(tx.QueryRowContext(ctx, `
		SELECT conversation_id, submission_id, prompt, mode, payload_fingerprint, sequence, turn_id, status, created_at, history_revision,
		       applied_after_step, applied_history_revision
		FROM conversation_submissions WHERE conversation_id = ? AND submission_id = ?;
	`, submission.ConversationID, submission.SubmissionID))
	if err == nil {
		if existing.PayloadFingerprint != submission.PayloadFingerprint || existing.Mode != submission.Mode {
			return nil, ErrSubmissionConflict
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("failed to check submission retry: %w", err)
	}

	var kind string
	if err := tx.QueryRowContext(ctx, `SELECT interaction_kind FROM sessions WHERE id = ?`, submission.ConversationID).Scan(&kind); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSessionNotFound
		}
		return nil, fmt.Errorf("failed to verify conversation: %w", err)
	}
	if InteractionKind(kind) != InteractionConversation {
		return nil, errors.New("cannot submit conversation input to a task session")
	}
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence), 0) + 1 FROM conversation_submissions WHERE conversation_id = ?`, submission.ConversationID).Scan(&submission.Sequence); err != nil {
		return nil, fmt.Errorf("failed to allocate submission sequence: %w", err)
	}
	var historyRevision int64
	if err := tx.QueryRowContext(ctx, `SELECT history_revision + 1 FROM sessions WHERE id = ?`, submission.ConversationID).Scan(&historyRevision); err != nil {
		return nil, fmt.Errorf("failed to allocate conversation history revision: %w", err)
	}
	submission.HistoryRevision = historyRevision
	if submission.Status == "" {
		submission.Status = SubmissionQueued
		if submission.Mode == SubmissionSteer {
			submission.Status = SubmissionSteerPending
		}
	}
	if submission.Mode == SubmissionQueue && submission.Status != SubmissionQueued {
		return nil, errors.New("queued submission must start in queued status")
	}
	if submission.Mode == SubmissionSteer && submission.Status != SubmissionSteerPending {
		return nil, errors.New("steering submission must start in steer-pending status")
	}
	if submission.CreatedAt.IsZero() {
		submission.CreatedAt = time.Now().UTC()
	}
	submission.Prompt = RedactSecrets(submission.Prompt)
	if err := insertSubmission(ctx, tx, submission); err != nil {
		return nil, fmt.Errorf("failed to persist conversation submission: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET updated_at = ?, history_revision = ? WHERE id = ?`, submission.CreatedAt.UTC(), historyRevision, submission.ConversationID); err != nil {
		return nil, fmt.Errorf("failed to advance conversation history revision: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit conversation submission: %w", err)
	}
	return submission, nil
}

// UpdateSubmissionStatus advances an accepted submission and records its assigned agent turn.
func (s *SQLiteStore) UpdateSubmissionStatus(ctx context.Context, conversationID, submissionID string, status SubmissionStatus, turnID string) error {
	if conversationID == "" || submissionID == "" {
		return ErrSubmissionNotFound
	}
	if status != SubmissionAttached && status != SubmissionConsumed {
		return fmt.Errorf("invalid submission transition target %q", status)
	}
	if status == SubmissionAttached && turnID == "" {
		return errors.New("attaching a submission requires an agent turn ID")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.writerDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin submission status update: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var current string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM conversation_submissions WHERE conversation_id = ? AND submission_id = ?`, conversationID, submissionID).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSubmissionNotFound
		}
		return fmt.Errorf("failed to read submission status: %w", err)
	}
	from := SubmissionStatus(current)
	valid := from == status || (status == SubmissionAttached && (from == SubmissionQueued || from == SubmissionSteerPending)) || (status == SubmissionConsumed && from == SubmissionAttached)
	if !valid {
		return fmt.Errorf("invalid submission status transition %q -> %q", from, status)
	}
	var assignedTurnID sql.NullString
	if turnID != "" {
		assignedTurnID = sql.NullString{String: turnID, Valid: true}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE conversation_submissions SET status = ?, turn_id = COALESCE(?, turn_id) WHERE conversation_id = ? AND submission_id = ?`, string(status), assignedTurnID, conversationID, submissionID); err != nil {
		return fmt.Errorf("failed to update conversation submission status: %w", err)
	}
	return tx.Commit()
}

// ApplySubmission atomically assigns an admitted prompt to a turn and records
// the global ReAct step boundary and durable history revision at which it enters
// model context. Repeating the same assignment is idempotent.
func (s *SQLiteStore) ApplySubmission(ctx context.Context, conversationID, submissionID, turnID string, appliedAfterStep int) (*SubmissionRecord, error) {
	if conversationID == "" || submissionID == "" || turnID == "" {
		return nil, errors.New("applying a submission requires conversation, submission, and turn IDs")
	}
	if appliedAfterStep < 0 {
		return nil, errors.New("applied step cannot be negative")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.writerDB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin submission apply: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	row := tx.QueryRowContext(ctx, `
		SELECT conversation_id, submission_id, prompt, mode, payload_fingerprint, sequence, turn_id, status, created_at, history_revision,
		       applied_after_step, applied_history_revision
		FROM conversation_submissions WHERE conversation_id = ? AND submission_id = ?;
	`, conversationID, submissionID)
	record, err := scanSubmission(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSubmissionNotFound
		}
		return nil, fmt.Errorf("read submission for apply: %w", err)
	}
	if record.Status == SubmissionAttached && record.TurnID != nil && *record.TurnID == turnID && record.AppliedAfterStep != nil {
		return record, nil
	}
	if record.Status != SubmissionQueued && record.Status != SubmissionSteerPending {
		return nil, fmt.Errorf("cannot apply submission in status %q", record.Status)
	}
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT history_revision + 1 FROM sessions WHERE id = ? AND interaction_kind = ?`, conversationID, string(InteractionConversation)).Scan(&revision); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSessionNotFound
		}
		return nil, fmt.Errorf("read revision for applied submission: %w", err)
	}
	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `UPDATE conversation_submissions SET status = ?, turn_id = ?, applied_after_step = ?, applied_history_revision = ? WHERE conversation_id = ? AND submission_id = ?`, string(SubmissionAttached), turnID, appliedAfterStep, revision, conversationID, submissionID); err != nil {
		return nil, fmt.Errorf("persist applied submission boundary: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET history_revision = ?, updated_at = ? WHERE id = ?`, revision, now, conversationID); err != nil {
		return nil, fmt.Errorf("advance revision for applied submission: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit applied submission: %w", err)
	}
	assignedTurn := turnID
	step := appliedAfterStep
	record.TurnID = &assignedTurn
	record.Status = SubmissionAttached
	record.AppliedAfterStep = &step
	record.AppliedHistoryRevision = revision
	return record, nil
}

// GetSubmission returns one durable conversation submission by its stable client ID.
func (s *SQLiteStore) GetSubmission(ctx context.Context, conversationID, submissionID string) (*SubmissionRecord, error) {
	if conversationID == "" || submissionID == "" {
		return nil, ErrSubmissionNotFound
	}
	record, err := scanSubmission(s.readerDB.QueryRowContext(ctx, `
		SELECT conversation_id, submission_id, prompt, mode, payload_fingerprint, sequence, turn_id, status, created_at, history_revision
		       , applied_after_step, applied_history_revision
		FROM conversation_submissions WHERE conversation_id = ? AND submission_id = ?;
	`, conversationID, submissionID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSubmissionNotFound
		}
		return nil, fmt.Errorf("failed to get conversation submission: %w", err)
	}
	return record, nil
}

// ListSubmissions returns accepted user inputs in their durable FIFO order.
func (s *SQLiteStore) ListSubmissions(ctx context.Context, conversationID string) ([]*SubmissionRecord, error) {
	rows, err := s.readerDB.QueryContext(ctx, `
		SELECT conversation_id, submission_id, prompt, mode, payload_fingerprint, sequence, turn_id, status, created_at, history_revision,
		       applied_after_step, applied_history_revision
		FROM conversation_submissions WHERE conversation_id = ? ORDER BY sequence ASC;
	`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("failed to list conversation submissions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var submissions []*SubmissionRecord
	for rows.Next() {
		submission, err := scanSubmission(rows)
		if err != nil {
			return nil, err
		}
		submissions = append(submissions, submission)
	}
	return submissions, rows.Err()
}

// ListSubmissionsThroughRevision returns accepted prompts visible in a history snapshot.
func (s *SQLiteStore) ListSubmissionsThroughRevision(ctx context.Context, conversationID string, revision int64) ([]*SubmissionRecord, error) {
	rows, err := s.readerDB.QueryContext(ctx, `
		SELECT conversation_id, submission_id, prompt, mode, payload_fingerprint, sequence, turn_id, status, created_at, history_revision,
		       applied_after_step, applied_history_revision
		FROM conversation_submissions WHERE conversation_id = ? AND history_revision <= ? ORDER BY sequence ASC;
	`, conversationID, revision)
	if err != nil {
		return nil, fmt.Errorf("failed to list revision-pinned conversation submissions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var submissions []*SubmissionRecord
	for rows.Next() {
		submission, err := scanSubmission(rows)
		if err != nil {
			return nil, err
		}
		submissions = append(submissions, submission)
	}
	return submissions, rows.Err()
}

// ListConversations returns only root M9 conversation records for a workspace.
func (s *SQLiteStore) ListConversations(ctx context.Context, workingDir string, limit int) ([]*SessionRecord, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.readerDB.QueryContext(ctx, `
		SELECT id, created_at, updated_at, status, task, model, working_dir, max_steps, total_steps,
		       prompt_tokens, completion_tokens, reasoning_tokens, total_tokens, error_message,
		       parent_session_id, role, depth, interaction_kind
		FROM sessions WHERE working_dir = ? AND depth = 0 AND interaction_kind = ?
		ORDER BY updated_at DESC LIMIT ?;
	`, workingDir, string(InteractionConversation), limit)
	if err != nil {
		return nil, fmt.Errorf("failed to list conversations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var sessions []*SessionRecord
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	return sessions, rows.Err()
}

// GetHistoryRevision returns the durable transcript revision for a conversation session.
func (s *SQLiteStore) GetHistoryRevision(ctx context.Context, conversationID string) (int64, error) {
	var revision int64
	err := s.readerDB.QueryRowContext(ctx, `SELECT history_revision FROM sessions WHERE id = ? AND interaction_kind = ?`, conversationID, string(InteractionConversation)).Scan(&revision)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrSessionNotFound
		}
		return 0, fmt.Errorf("failed to get conversation history revision: %w", err)
	}
	return revision, nil
}

// CreateConversationTurn records the start of an agent turn within a conversation.
func (s *SQLiteStore) CreateConversationTurn(ctx context.Context, turn *ConversationTurnRecord) error {
	if turn == nil || turn.ConversationID == "" || turn.TurnID == "" || turn.SubmissionID == "" {
		return errors.New("conversation turn requires conversation, turn, and submission IDs")
	}
	if turn.StartedAt.IsZero() {
		turn.StartedAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.writerDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin conversation turn: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertConversationTurn(ctx, tx, turn); err != nil {
		return fmt.Errorf("failed to create conversation turn %s: %w", turn.TurnID, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, turn.StartedAt.UTC(), turn.ConversationID); err != nil {
		return fmt.Errorf("failed to update conversation activity: %w", err)
	}
	return tx.Commit()
}

// UpdateConversationTurn records a turn's terminal state and completion time.
func (s *SQLiteStore) UpdateConversationTurn(ctx context.Context, turn *ConversationTurnRecord) error {
	if turn == nil || turn.ConversationID == "" || turn.TurnID == "" {
		return errors.New("conversation turn update requires conversation and turn IDs")
	}
	var finishedAt sql.NullTime
	if turn.FinishedAt != nil {
		finishedAt = sql.NullTime{Time: turn.FinishedAt.UTC(), Valid: true}
	}
	var errMessage sql.NullString
	if turn.ErrorMessage != nil {
		errMessage = sql.NullString{String: RedactSecrets(*turn.ErrorMessage), Valid: true}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.writerDB.ExecContext(ctx, `
		UPDATE conversation_turns SET status = ?, finished_at = ?, error_message = ?
		WHERE conversation_id = ? AND turn_id = ?;
	`, string(turn.Status), finishedAt, errMessage, turn.ConversationID, turn.TurnID)
	if err != nil {
		return fmt.Errorf("failed to update conversation turn %s: %w", turn.TurnID, err)
	}
	if count, _ := res.RowsAffected(); count == 0 {
		return ErrSessionNotFound
	}
	return nil
}

// ListConversationTurns returns agent turns in chronological start order.
func (s *SQLiteStore) ListConversationTurns(ctx context.Context, conversationID string) ([]*ConversationTurnRecord, error) {
	rows, err := s.readerDB.QueryContext(ctx, `
		SELECT conversation_id, turn_id, submission_id, status, started_at, finished_at, error_message, history_revision
		FROM conversation_turns WHERE conversation_id = ? ORDER BY started_at, turn_id;
	`, conversationID)
	if err != nil {
		return nil, fmt.Errorf("failed to list conversation turns: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var turns []*ConversationTurnRecord
	for rows.Next() {
		var turn ConversationTurnRecord
		var status string
		var finishedAt sql.NullTime
		var errorMessage sql.NullString
		if err := rows.Scan(&turn.ConversationID, &turn.TurnID, &turn.SubmissionID, &status, &turn.StartedAt, &finishedAt, &errorMessage, &turn.HistoryRevision); err != nil {
			return nil, err
		}
		turn.Status = SessionStatus(status)
		if finishedAt.Valid {
			turn.FinishedAt = &finishedAt.Time
		}
		if errorMessage.Valid {
			turn.ErrorMessage = &errorMessage.String
		}
		turns = append(turns, &turn)
	}
	return turns, rows.Err()
}

func insertConversationTurn(ctx context.Context, tx *sql.Tx, turn *ConversationTurnRecord) error {
	var finishedAt sql.NullTime
	if turn.FinishedAt != nil {
		finishedAt = sql.NullTime{Time: turn.FinishedAt.UTC(), Valid: true}
	}
	var errorMessage sql.NullString
	if turn.ErrorMessage != nil {
		errorMessage = sql.NullString{String: RedactSecrets(*turn.ErrorMessage), Valid: true}
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO conversation_turns (conversation_id, turn_id, submission_id, status, started_at, finished_at, error_message, history_revision)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?);
	`, turn.ConversationID, turn.TurnID, turn.SubmissionID, string(turn.Status), turn.StartedAt.UTC(), finishedAt, errorMessage, turn.HistoryRevision)
	return err
}

func insertSubmission(ctx context.Context, tx *sql.Tx, submission *SubmissionRecord) error {
	var turnID sql.NullString
	if submission.TurnID != nil && *submission.TurnID != "" {
		turnID = sql.NullString{String: *submission.TurnID, Valid: true}
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO conversation_submissions (
			conversation_id, submission_id, prompt, mode, payload_fingerprint, sequence, turn_id, status, created_at, history_revision,
			applied_after_step, applied_history_revision
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
	`, submission.ConversationID, submission.SubmissionID, submission.Prompt, string(submission.Mode),
		submission.PayloadFingerprint, submission.Sequence, turnID, string(submission.Status), submission.CreatedAt.UTC(), submission.HistoryRevision,
		submission.AppliedAfterStep, submission.AppliedHistoryRevision)
	return err
}

func scanSubmission(scanner interface{ Scan(...interface{}) error }) (*SubmissionRecord, error) {
	var submission SubmissionRecord
	var turnID sql.NullString
	var appliedAfterStep sql.NullInt64
	var mode, status string
	err := scanner.Scan(&submission.ConversationID, &submission.SubmissionID, &submission.Prompt, &mode,
		&submission.PayloadFingerprint, &submission.Sequence, &turnID, &status, &submission.CreatedAt, &submission.HistoryRevision,
		&appliedAfterStep, &submission.AppliedHistoryRevision)
	if err != nil {
		return nil, err
	}
	submission.Mode = SubmissionMode(mode)
	submission.Status = SubmissionStatus(status)
	if turnID.Valid {
		submission.TurnID = &turnID.String
	}
	if appliedAfterStep.Valid {
		step := int(appliedAfterStep.Int64)
		submission.AppliedAfterStep = &step
	}
	return &submission, nil
}

// GetSession retrieves a session record by exact ID or unique short prefix.
func (s *SQLiteStore) GetSession(ctx context.Context, idOrPrefix string) (*SessionRecord, error) {
	trimmed := strings.TrimSpace(idOrPrefix)
	if trimmed == "" {
		return nil, ErrSessionNotFound
	}

	query := `
	SELECT id, created_at, updated_at, status, task, model, working_dir, max_steps, total_steps,
	       prompt_tokens, completion_tokens, reasoning_tokens, total_tokens, error_message,
	       parent_session_id, role, depth, interaction_kind
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

// GetLatestResumableSession fetches the most recent interrupted, failed, or running root session in workingDir.
func (s *SQLiteStore) GetLatestResumableSession(ctx context.Context, workingDir string) (*SessionRecord, error) {
	query := `
	SELECT id, created_at, updated_at, status, task, model, working_dir, max_steps, total_steps,
	       prompt_tokens, completion_tokens, reasoning_tokens, total_tokens, error_message,
	       parent_session_id, role, depth, interaction_kind
	FROM sessions
	WHERE working_dir = ? AND depth = 0 AND status IN ('interrupted', 'failed', 'running')
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

// ListSessions returns recent root sessions (depth = 0) in the specified working directory ordered by updated_at DESC.
func (s *SQLiteStore) ListSessions(ctx context.Context, workingDir string, limit int) ([]*SessionRecord, error) {
	if limit <= 0 {
		limit = 20
	}

	query := `
	SELECT id, created_at, updated_at, status, task, model, working_dir, max_steps, total_steps,
	       prompt_tokens, completion_tokens, reasoning_tokens, total_tokens, error_message,
	       parent_session_id, role, depth, interaction_kind
	FROM sessions
	WHERE working_dir = ? AND depth = 0
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

// ListAllSessions returns all recent sessions (including child subagents) in the specified working directory.
func (s *SQLiteStore) ListAllSessions(ctx context.Context, workingDir string, limit int) ([]*SessionRecord, error) {
	if limit <= 0 {
		limit = 20
	}

	query := `
	SELECT id, created_at, updated_at, status, task, model, working_dir, max_steps, total_steps,
	       prompt_tokens, completion_tokens, reasoning_tokens, total_tokens, error_message,
	       parent_session_id, role, depth, interaction_kind
	FROM sessions
	WHERE working_dir = ?
	ORDER BY updated_at DESC
	LIMIT ?;
	`

	rows, err := s.readerDB.QueryContext(ctx, query, workingDir, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to list all sessions: %w", err)
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

// GetChildSessions returns all child sessions belonging to the specified parent session ID ordered by created_at ASC.
func (s *SQLiteStore) GetChildSessions(ctx context.Context, parentID string) ([]*SessionRecord, error) {
	query := `
	SELECT id, created_at, updated_at, status, task, model, working_dir, max_steps, total_steps,
	       prompt_tokens, completion_tokens, reasoning_tokens, total_tokens, error_message,
	       parent_session_id, role, depth, interaction_kind
	FROM sessions
	WHERE parent_session_id = ?
	ORDER BY created_at ASC;
	`

	rows, err := s.readerDB.QueryContext(ctx, query, parentID)
	if err != nil {
		return nil, fmt.Errorf("failed to query child sessions for parent %q: %w", parentID, err)
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

// IncrementSessionTokens adds token usage metrics into an existing session record.
func (s *SQLiteStore) IncrementSessionTokens(ctx context.Context, sessionID string, usage TokenUsage) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	query := `
	UPDATE sessions SET
		prompt_tokens = prompt_tokens + ?,
		completion_tokens = completion_tokens + ?,
		reasoning_tokens = reasoning_tokens + ?,
		total_tokens = total_tokens + ?,
		updated_at = ?
	WHERE id = ?;
	`

	_, err := s.writerDB.ExecContext(
		ctx,
		query,
		usage.PromptTokens,
		usage.CompletionTokens,
		usage.ReasoningTokens,
		usage.TotalTokens,
		time.Now().UTC(),
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("failed to increment session tokens for %s: %w", sessionID, err)
	}
	return nil
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

	redactedErrMsg := ""
	if errMsg != "" {
		redactedErrMsg = RedactSecrets(errMsg)
	}

	_, err := s.writerDB.ExecContext(
		ctx,
		query,
		string(status),
		steps,
		usage.PromptTokens,
		usage.CompletionTokens,
		usage.ReasoningTokens,
		usage.TotalTokens,
		sql.NullString{String: redactedErrMsg, Valid: errMsg != ""},
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
			Args:     RedactJSON(tc.Args),
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
	if err := tx.QueryRowContext(ctx, `SELECT history_revision + 1 FROM sessions WHERE id = ?`, turn.SessionID).Scan(&turn.HistoryRevision); err != nil {
		return fmt.Errorf("failed to allocate history revision for turn %d: %w", turn.StepNumber, err)
	}

	turnQuery := `
	INSERT INTO turns (
		session_id, conversation_turn_id, step_number, role, content, reasoning_content, tool_calls,
		prompt_tokens, completion_tokens, reasoning_tokens, total_tokens, duration_ms, created_at, history_revision
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
	`
	var conversationTurnID sql.NullString
	if turn.ConversationTurnID != "" {
		conversationTurnID = sql.NullString{String: turn.ConversationTurnID, Valid: true}
	}

	_, err = tx.ExecContext(
		ctx,
		turnQuery,
		turn.SessionID,
		conversationTurnID,
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
		turn.HistoryRevision,
	)
	if err != nil {
		return fmt.Errorf("failed to insert turn %d: %w", turn.StepNumber, err)
	}

	sessionQuery := `
	UPDATE sessions
	SET total_steps = ?, prompt_tokens = ?, completion_tokens = ?,
	    reasoning_tokens = ?, total_tokens = ?, updated_at = ?, history_revision = ?
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
		turn.HistoryRevision,
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
	SELECT session_id, conversation_turn_id, step_number, role, content, reasoning_content, tool_calls,
	       prompt_tokens, completion_tokens, reasoning_tokens, total_tokens, duration_ms, created_at, history_revision
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

// GetTurnsThroughRevision returns committed steps visible at a durable history revision.
func (s *SQLiteStore) GetTurnsThroughRevision(ctx context.Context, conversationID string, revision int64) ([]*TurnRecord, error) {
	rows, err := s.readerDB.QueryContext(ctx, `
		SELECT session_id, conversation_turn_id, step_number, role, content, reasoning_content, tool_calls,
		       prompt_tokens, completion_tokens, reasoning_tokens, total_tokens, duration_ms, created_at, history_revision
		FROM turns WHERE session_id = ? AND history_revision <= ? ORDER BY step_number ASC;
	`, conversationID, revision)
	if err != nil {
		return nil, fmt.Errorf("failed to query conversation turns through history revision %d: %w", revision, err)
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

// SaveSnapshot saves an immutable episodic state snapshot generated during context compaction.
func (s *SQLiteStore) SaveSnapshot(ctx context.Context, snapshot *SnapshotRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.writerDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin snapshot transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := tx.QueryRowContext(ctx, `SELECT history_revision + 1 FROM sessions WHERE id = ?`, snapshot.SessionID).Scan(&snapshot.HistoryRevision); err != nil {
		return fmt.Errorf("failed to allocate snapshot history revision: %w", err)
	}

	query := `
	INSERT INTO episodic_snapshots (
		session_id, step_number, distilled_narrative, state_tracker, current_tokens, created_at, history_revision
	) VALUES (?, ?, ?, ?, ?, ?, ?);
	`

	redactedNarrative := RedactSecrets(snapshot.DistilledNarrative)
	redactedTracker := RedactJSON(snapshot.StateTrackerJSON)
	snapshot.DistilledNarrative = redactedNarrative
	snapshot.StateTrackerJSON = redactedTracker

	res, err := tx.ExecContext(
		ctx,
		query,
		snapshot.SessionID,
		snapshot.StepNumber,
		redactedNarrative,
		redactedTracker,
		snapshot.CurrentTokens,
		snapshot.CreatedAt.UTC(),
		snapshot.HistoryRevision,
	)
	if err != nil {
		return fmt.Errorf("failed to insert snapshot at step %d: %w", snapshot.StepNumber, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET history_revision = ?, updated_at = ? WHERE id = ?`, snapshot.HistoryRevision, snapshot.CreatedAt.UTC(), snapshot.SessionID); err != nil {
		return fmt.Errorf("failed to advance snapshot history revision: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit snapshot at step %d: %w", snapshot.StepNumber, err)
	}

	id, _ := res.LastInsertId()
	snapshot.ID = id
	return nil
}

// GetLatestSnapshot returns the most recent episodic snapshot for a session, or nil if none exists.
func (s *SQLiteStore) GetLatestSnapshot(ctx context.Context, sessionID string) (*SnapshotRecord, error) {
	query := `
	SELECT id, session_id, step_number, distilled_narrative, state_tracker, current_tokens, created_at, history_revision
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
		&snap.HistoryRevision,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get latest snapshot: %w", err)
	}
	return &snap, nil
}

// GetSnapshotThroughRevision returns the newest snapshot visible at a durable history revision.
func (s *SQLiteStore) GetSnapshotThroughRevision(ctx context.Context, conversationID string, revision int64) (*SnapshotRecord, error) {
	row := s.readerDB.QueryRowContext(ctx, `
		SELECT id, session_id, step_number, distilled_narrative, state_tracker, current_tokens, created_at, history_revision
		FROM episodic_snapshots WHERE session_id = ? AND history_revision <= ?
		ORDER BY step_number DESC LIMIT 1;
	`, conversationID, revision)
	var snapshot SnapshotRecord
	err := row.Scan(&snapshot.ID, &snapshot.SessionID, &snapshot.StepNumber, &snapshot.DistilledNarrative,
		&snapshot.StateTrackerJSON, &snapshot.CurrentTokens, &snapshot.CreatedAt, &snapshot.HistoryRevision)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to get snapshot through history revision: %w", err)
	}
	return &snapshot, nil
}

func scanSession(scanner interface{ Scan(...interface{}) error }) (*SessionRecord, error) {
	var (
		sess            SessionRecord
		status          string
		errMsg          sql.NullString
		parentSessionID sql.NullString
		interactionKind string
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
		&parentSessionID,
		&sess.Role,
		&sess.Depth,
		&interactionKind,
	)
	if err != nil {
		return nil, err
	}

	sess.Status = SessionStatus(status)
	sess.InteractionKind = InteractionKind(interactionKind)
	if errMsg.Valid {
		sess.ErrorMessage = &errMsg.String
	}
	if parentSessionID.Valid && parentSessionID.String != "" {
		sess.ParentSessionID = &parentSessionID.String
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
		turnID    sql.NullString
	)

	err := rows.Scan(
		&turn.SessionID,
		&turnID,
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
		&turn.HistoryRevision,
	)
	if err != nil {
		return nil, err
	}

	if turnID.Valid {
		turn.ConversationTurnID = turnID.String
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
