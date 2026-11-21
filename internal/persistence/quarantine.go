package persistence

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	// sqlite driver registration
	_ "modernc.org/sqlite"
)

// CheckOrQuarantine tests database integrity. If corruption is detected,
// the corrupted database file and its WAL/SHM siblings are quarantined with a timestamp.
func CheckOrQuarantine(dbPath string) error {
	if dbPath == ":memory:" || strings.HasPrefix(dbPath, "file:") {
		return nil
	}

	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		return nil
	}

	// Open raw connection to run quick_check
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=query_only(ON)", dbPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("failed to open database %s: %w", dbPath, err)
	}
	defer func() { _ = db.Close() }()

	var result string
	err = db.QueryRow("PRAGMA quick_check(1);").Scan(&result)
	if err != nil {
		errStr := strings.ToLower(err.Error())
		if strings.Contains(errStr, "corrupt") || strings.Contains(errStr, "malformed") || strings.Contains(errStr, "not a database") {
			_ = db.Close()
			return quarantineFiles(dbPath)
		}
		return fmt.Errorf("database quick_check operational error: %w", err)
	}
	if result != "ok" {
		resLower := strings.ToLower(result)
		if strings.Contains(resLower, "lock") || strings.Contains(resLower, "busy") || strings.Contains(resLower, "timeout") {
			return fmt.Errorf("database is temporarily locked: %s", result)
		}
		_ = db.Close()
		return quarantineFiles(dbPath)
	}

	return nil
}

func quarantineFiles(dbPath string) error {
	timestamp := time.Now().Unix()
	quarantinePath := fmt.Sprintf("%s.corrupt.%d", dbPath, timestamp)
	if err := os.Rename(dbPath, quarantinePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to quarantine corrupt database %s: %w", dbPath, err)
	}

	// Clean up or move WAL and SHM
	_ = os.Rename(dbPath+"-wal", quarantinePath+"-wal")
	_ = os.Rename(dbPath+"-shm", quarantinePath+"-shm")

	return nil
}
