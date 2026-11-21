package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/hoangndst/buddy-agent/internal/agent"
	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
)

const (
	// DefaultMaxCompletedSessions limits retention of completed session workers to prevent memory growth.
	DefaultMaxCompletedSessions = 50
)

// EngineFactory defines a constructor function that builds an agent.Engine and cleanup closure for a session.
type EngineFactory func(sessionID string, params protocol.SessionStartParams, emitHandler agent.EventHandler) (*agent.Engine, func(), error)

// SessionManager tracks all active daemon sessions and manages headless execution lifecycles.
type SessionManager struct {
	mu                   sync.RWMutex
	sessions             map[string]*SessionWorker
	completedOrder       []string
	maxCompletedSessions int
	engineFactory        EngineFactory
}

// NewSessionManager creates a new SessionManager.
func NewSessionManager(factory EngineFactory) *SessionManager {
	return &SessionManager{
		sessions:             make(map[string]*SessionWorker),
		completedOrder:       make([]string, 0),
		maxCompletedSessions: DefaultMaxCompletedSessions,
		engineFactory:        factory,
	}
}

// SetMaxCompletedSessions overrides the maximum number of completed sessions retained in memory.
func (sm *SessionManager) SetMaxCompletedSessions(limit int) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if limit > 0 {
		sm.maxCompletedSessions = limit
		sm.pruneCompletedLocked()
	}
}

func (sm *SessionManager) onSessionCompleted(sessionID string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	sm.completedOrder = append(sm.completedOrder, sessionID)
	sm.pruneCompletedLocked()
}

func (sm *SessionManager) pruneCompletedLocked() {
	for len(sm.completedOrder) > sm.maxCompletedSessions {
		evictID := sm.completedOrder[0]
		sm.completedOrder = sm.completedOrder[1:]
		if w, ok := sm.sessions[evictID]; ok && !w.IsActive() {
			delete(sm.sessions, evictID)
		}
	}
}

func generateDaemonSessionID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("sess_%s", hex.EncodeToString(b))
}

// StartSession initializes and launches a new headless session worker.
func (sm *SessionManager) StartSession(
	ctx context.Context,
	params protocol.SessionStartParams,
	controller ClientConn,
) (*SessionWorker, error) {
	sessionID := generateDaemonSessionID()

	var worker *SessionWorker

	emitHandler := func(evt agent.Event) {
		if worker != nil {
			worker.EmitEvent(evt)
		}
	}

	eng, cleanup, err := sm.engineFactory(sessionID, params, emitHandler)
	if err != nil {
		return nil, fmt.Errorf("failed to instantiate engine for session %s: %w", sessionID, err)
	}

	worker = NewSessionWorker(ctx, sessionID, params.Task, eng, controller, cleanup)
	worker.SetOnComplete(func() {
		sm.onSessionCompleted(sessionID)
	})

	sm.mu.Lock()
	sm.sessions[sessionID] = worker
	sm.mu.Unlock()

	worker.Start()
	return worker, nil
}

// GetSession retrieves an active or completed session by ID.
func (sm *SessionManager) GetSession(sessionID string) (*SessionWorker, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	worker, ok := sm.sessions[sessionID]
	return worker, ok
}

// ListActiveSessions returns IDs of all sessions currently executing.
func (sm *SessionManager) ListActiveSessions() []string {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	var active []string
	for id, worker := range sm.sessions {
		if worker.IsActive() {
			active = append(active, id)
		}
	}
	return active
}

// CancelSession cancels a specific session.
func (sm *SessionManager) CancelSession(sessionID string) error {
	worker, ok := sm.GetSession(sessionID)
	if !ok {
		return fmt.Errorf("session %s not found", sessionID)
	}
	worker.Cancel()
	return nil
}

// Drain stops all active sessions and waits up to timeout for them to cleanly persist state.
func (sm *SessionManager) Drain(timeout time.Duration) {
	sm.mu.RLock()
	workers := make([]*SessionWorker, 0, len(sm.sessions))
	for _, w := range sm.sessions {
		if w.IsActive() {
			workers = append(workers, w)
		}
	}
	sm.mu.RUnlock()

	for _, w := range workers {
		w.Cancel()
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		stillActive := false
		for _, w := range workers {
			if w.IsActive() {
				stillActive = true
				break
			}
		}
		if !stillActive {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
}
