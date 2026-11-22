package persistence

import (
	"context"
	"path/filepath"
	"testing"
)

func TestWorkspaceCatalogRetainsWorkspaceDatabasePaths(t *testing.T) {
	catalog, err := NewWorkspaceCatalog(filepath.Join(t.TempDir(), ".buddy", "workspaces.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = catalog.Close() }()
	ctx := context.Background()
	if err := catalog.Register(ctx, "/projects/alpha", "/projects/alpha/.buddy/history.db"); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Register(ctx, "/projects/alpha", "/custom/alpha.db"); err != nil {
		t.Fatal(err)
	}
	if err := catalog.Register(ctx, "/projects/old-name", "/projects/old-name/.buddy/history.db"); err != nil {
		t.Fatal(err)
	}
	entries, err := catalog.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected retained old and current database paths, got %+v", entries)
	}
}
