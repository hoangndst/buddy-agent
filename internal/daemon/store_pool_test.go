package daemon

import (
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/hoangndst/buddy-agent/internal/persistence"
)

func TestStorePoolSharesAndClosesStoreOnce(t *testing.T) {
	pool := NewStorePool()
	var opens, closes atomic.Int32
	open := func() (persistence.Store, func(), error) {
		opens.Add(1)
		store, err := persistence.NewSQLiteStore(":memory:")
		if err != nil {
			return nil, nil, err
		}
		return store, func() { closes.Add(1); _ = store.Close() }, nil
	}
	first, _, err := pool.Get("/db/shared.sqlite", open)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := pool.Get("/db/shared.sqlite", open)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || opens.Load() != 1 {
		t.Fatalf("same DB key did not share a store: same=%v opens=%d", first == second, opens.Load())
	}
	if err := pool.Close(); err != nil {
		t.Fatal(err)
	}
	if closes.Load() != 1 {
		t.Fatalf("expected one store close, got %d", closes.Load())
	}
	if _, _, err := pool.Get("/db/shared.sqlite", open); err == nil {
		t.Fatal("expected a closed pool to reject new stores")
	}
}

func TestStorePoolDeduplicatesByResolvedDatabasePath(t *testing.T) {
	pool := NewStorePool()
	var closes atomic.Int32
	databasePath := filepath.Join(t.TempDir(), "shared.db")
	open := func() (persistence.Store, func(), error) {
		store, err := persistence.NewSQLiteStore(databasePath)
		if err != nil { return nil, nil, err }
		return store, func() { closes.Add(1); _ = store.Close() }, nil
	}
	first, _, err := pool.Get("requested-path-a", open)
	if err != nil { t.Fatal(err) }
	second, _, err := pool.Get("requested-path-b", open)
	if err != nil { t.Fatal(err) }
	if first != second { t.Fatal("same resolved SQLite path produced two pooled stores") }
	if closes.Load() != 1 { t.Fatalf("duplicate opened store should be closed immediately; closes=%d", closes.Load()) }
	if err := pool.Close(); err != nil { t.Fatal(err) }
	if closes.Load() != 2 { t.Fatalf("pool close did not release the retained store: closes=%d", closes.Load()) }
}
