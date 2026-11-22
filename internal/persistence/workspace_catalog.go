package persistence

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type WorkspaceCatalogEntry struct {
	WorkspacePath string    `json:"workspace_path"`
	DatabasePath  string    `json:"database_path"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// WorkspaceCatalog is a small user-level index for finding workspace databases.
// Conversation transcripts remain in their workspace-specific SQLite databases.
type WorkspaceCatalog struct{ db *sql.DB }

func NewWorkspaceCatalog(path string) (*WorkspaceCatalog, error) {
	if path == "" {
		return nil, fmt.Errorf("workspace catalog path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, fmt.Errorf("create workspace catalog directory: %w", err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, fmt.Errorf("open workspace catalog: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS workspace_catalog (
		workspace_path TEXT NOT NULL,
		database_path TEXT NOT NULL,
		updated_at DATETIME NOT NULL,
		PRIMARY KEY (workspace_path, database_path)
	); CREATE INDEX IF NOT EXISTS idx_workspace_catalog_updated ON workspace_catalog(updated_at DESC);`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("initialize workspace catalog: %w", err)
	}
	return &WorkspaceCatalog{db: db}, nil
}

func (c *WorkspaceCatalog) Register(ctx context.Context, workspacePath, databasePath string) error {
	if workspacePath == "" || databasePath == "" {
		return fmt.Errorf("workspace and database paths are required")
	}
	if _, err := c.db.ExecContext(ctx, `INSERT INTO workspace_catalog(workspace_path, database_path, updated_at)
		VALUES (?, ?, ?) ON CONFLICT(workspace_path, database_path) DO UPDATE SET updated_at = excluded.updated_at`,
		workspacePath, databasePath, time.Now().UTC()); err != nil {
		return fmt.Errorf("register workspace database: %w", err)
	}
	return nil
}

func (c *WorkspaceCatalog) List(ctx context.Context) ([]WorkspaceCatalogEntry, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT workspace_path, database_path, updated_at FROM workspace_catalog ORDER BY updated_at DESC, workspace_path, database_path`)
	if err != nil {
		return nil, fmt.Errorf("list workspace catalog: %w", err)
	}
	defer rows.Close()
	var entries []WorkspaceCatalogEntry
	for rows.Next() {
		var entry WorkspaceCatalogEntry
		if err := rows.Scan(&entry.WorkspacePath, &entry.DatabasePath, &entry.UpdatedAt); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func (c *WorkspaceCatalog) Close() error { return c.db.Close() }
