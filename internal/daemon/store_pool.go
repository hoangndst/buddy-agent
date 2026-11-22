package daemon

import (
	"errors"
	"path/filepath"
	"sync"

	"github.com/hoangndst/buddy-agent/internal/persistence"
)

type pooledStore struct {
	store persistence.Store
	close func()
}

// StorePool shares SQLite handles among engines that use the same resolved DB path.
// It is closed by SessionManager after workers have drained.
type StorePool struct {
	mu      sync.Mutex
	stores  map[string]pooledStore
	aliases map[string]string
	closed  bool
}

// NewStorePool creates a pool for sharing workspace database handles.
func NewStorePool() *StorePool {
	return &StorePool{stores: make(map[string]pooledStore), aliases: make(map[string]string)}
}

// Get returns the pooled store for key, opening and registering it when absent.
func (p *StorePool) Get(key string, open func() (persistence.Store, func(), error)) (persistence.Store, func(), error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, nil, errors.New("daemon persistence store pool is closed")
	}
	if actual, ok := p.aliases[key]; ok {
		key = actual
	}
	if entry, ok := p.stores[key]; ok {
		return entry.store, func() {}, nil
	}
	store, closeStore, err := open()
	if err != nil {
		return nil, nil, err
	}
	if store == nil {
		return nil, nil, errors.New("store opener returned nil store")
	}
	if pathProvider, ok := store.(interface{ DatabasePath() string }); ok && pathProvider.DatabasePath() != "" {
		actualKey := pathProvider.DatabasePath()
		if abs, err := filepath.Abs(filepath.Clean(actualKey)); err == nil {
			actualKey = abs
		}
		if resolved, err := filepath.EvalSymlinks(actualKey); err == nil {
			actualKey = resolved
		}
		if existing, ok := p.stores[actualKey]; ok {
			if closeStore != nil {
				closeStore()
			} else {
				_ = store.Close()
			}
			p.aliases[key] = actualKey
			return existing.store, func() {}, nil
		}
		p.aliases[key] = actualKey
		key = actualKey
	}
	p.stores[key] = pooledStore{store: store, close: closeStore}
	return store, func() {}, nil
}

// Close releases every store owned by the pool.
func (p *StorePool) Close() error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	entries := make([]pooledStore, 0, len(p.stores))
	for _, entry := range p.stores {
		entries = append(entries, entry)
	}
	p.stores = nil
	p.aliases = nil
	p.mu.Unlock()
	var closeErr error
	for _, entry := range entries {
		if entry.close != nil {
			entry.close()
		} else if entry.store != nil {
			if err := entry.store.Close(); err != nil && closeErr == nil {
				closeErr = err
			}
		}
	}
	return closeErr
}
