package cmdutil_test

import (
	"path/filepath"
	"testing"

	"github.com/hoangndst/buddy-agent/internal/cmdutil"
)

func TestFactory_StoreForWorkspace(t *testing.T) {
	f := cmdutil.NewFactory()
	tempDir := t.TempDir()

	// Case 1: Default database path inside workspace
	store, cleanup, err := f.StoreForWorkspace(tempDir)
	if err != nil {
		t.Fatalf("StoreForWorkspace failed: %v", err)
	}
	defer cleanup()
	if store == nil {
		t.Fatal("expected non-nil store")
	}

	// Case 2: Custom absolute database path outside workspace
	customDir := t.TempDir()
	customDB := filepath.Join(customDir, "custom.db")
	store2, cleanup2, err := f.StoreForWorkspace(tempDir, customDB)
	if err != nil {
		t.Fatalf("StoreForWorkspace with customDB failed: %v", err)
	}
	defer cleanup2()
	if store2 == nil {
		t.Fatal("expected non-nil store")
	}
}
