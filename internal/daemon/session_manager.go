package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/hoangndst/buddy-agent/internal/agent"
	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
	"github.com/hoangndst/buddy-agent/internal/persistence"
)

const (
	// DefaultMaxCompletedSessions limits retention of completed session workers to prevent memory growth.
	DefaultMaxCompletedSessions = 50
)

// EngineFactory defines a constructor function that builds an agent.Engine and cleanup closure for a session.
type EngineFactory func(sessionID string, params protocol.SessionStartParams, emitHandler agent.EventHandler) (*agent.Engine, func(), error)

// SessionManager tracks all active daemon sessions and manages headless execution lifecycles.
type SessionManager struct {
	mu                    sync.RWMutex
	storesCloseOnce       sync.Once
	sessions              map[string]*SessionWorker
	conversations         map[string]*ConversationWorker
	completedOrder        []string
	idleConversationOrder []string
	maxCompletedSessions  int
	engineFactory         EngineFactory
	conversationStartMu   sync.Mutex
	startBindings         map[string]string
	storePool             *StorePool
	workspaceCatalog      *persistence.WorkspaceCatalog
}

// SetStorePool configures the manager's shared workspace store pool.
func (sm *SessionManager) SetStorePool(pool *StorePool) {
	sm.mu.Lock()
	sm.storePool = pool
	sm.mu.Unlock()
}

// SetWorkspaceCatalog configures workspace discovery for stored conversations.
func (sm *SessionManager) SetWorkspaceCatalog(catalog *persistence.WorkspaceCatalog) {
	sm.mu.Lock()
	sm.workspaceCatalog = catalog
	sm.mu.Unlock()
}

func (sm *SessionManager) closeStores() {
	sm.storesCloseOnce.Do(func() {
		sm.mu.RLock()
		pool := sm.storePool
		catalog := sm.workspaceCatalog
		sm.mu.RUnlock()
		if pool != nil {
			_ = pool.Close()
		}
		if catalog != nil {
			_ = catalog.Close()
		}
	})
}

func (sm *SessionManager) registerWorkspace(ctx context.Context, workspace string, store persistence.Store) error {
	sm.mu.RLock()
	catalog := sm.workspaceCatalog
	sm.mu.RUnlock()
	if catalog == nil {
		return nil
	}
	pathProvider, ok := store.(interface{ DatabasePath() string })
	if !ok || pathProvider.DatabasePath() == "" {
		return errors.New("workspace catalog requires a store with a resolved database path")
	}
	databasePath, err := filepath.Abs(filepath.Clean(pathProvider.DatabasePath()))
	if err != nil {
		return fmt.Errorf("resolve workspace database path: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(databasePath); err == nil {
		databasePath = resolved
	}
	return catalog.Register(ctx, workspace, databasePath)
}

// NewSessionManager creates a new SessionManager.
func NewSessionManager(factory EngineFactory) *SessionManager {
	return &SessionManager{
		sessions:              make(map[string]*SessionWorker),
		conversations:         make(map[string]*ConversationWorker),
		completedOrder:        make([]string, 0),
		idleConversationOrder: make([]string, 0),
		startBindings:         make(map[string]string),
		maxCompletedSessions:  DefaultMaxCompletedSessions,
		engineFactory:         factory,
	}
}

// StartConversation creates or retries a durable conversation and starts its resident worker.
func (sm *SessionManager) StartConversation(ctx context.Context, params protocol.ConversationStartParams, controller ClientConn) (*ConversationWorker, *protocol.ConversationStartResult, error) {
	if params.StartID == "" || params.SubmissionID == "" || params.Prompt == "" {
		return nil, nil, fmt.Errorf("conversation.start requires start_id, submission_id, and prompt")
	}
	if params.Mode != "" && params.Mode != string(persistence.SubmissionQueue) {
		return nil, nil, fmt.Errorf("initial conversation prompt must use queue mode")
	}
	sm.conversationStartMu.Lock()
	defer sm.conversationStartMu.Unlock()

	requestedWorkspace, err := canonicalWorkspace(params.WorkspaceDir)
	if err != nil {
		return nil, nil, err
	}
	key := requestedWorkspace + "\x00" + params.StartID
	if id := sm.startBindings[key]; id != "" {
		if worker, ok := sm.GetConversation(id); ok {
			first, err := worker.Submission(ctx, params.SubmissionID)
			if err != nil {
				if err == persistence.ErrSubmissionNotFound {
					return nil, nil, persistence.ErrSubmissionConflict
				}
				return nil, nil, err
			}
			if first.SubmissionID != params.SubmissionID || first.PayloadFingerprint != persistence.SubmissionFingerprint(params.Prompt, persistence.SubmissionQueue) {
				return nil, nil, persistence.ErrSubmissionConflict
			}
			turnID := ""
			if first.TurnID != nil {
				turnID = *first.TurnID
			}
			return worker, &protocol.ConversationStartResult{ConversationID: id, SessionID: id, SubmissionID: params.SubmissionID, TurnID: turnID, Existing: true}, nil
		}
	}

	conversationID := generateWorkerID("conv")
	startParams := protocol.SessionStartParams{
		Task: params.Prompt, WorkspaceDir: requestedWorkspace, Model: params.Model,
		ReasoningEffort: params.ReasoningEffort, MaxSteps: params.MaxSteps,
		MaxContextTokens: params.MaxContextTokens, MaxOutputChars: params.MaxOutputChars,
		WorkingTailSize: params.WorkingTailSize, BaseURL: params.BaseURL, APIKey: params.APIKey,
		APIType: params.APIType, MCPConfig: params.MCPConfig, DBPath: params.DBPath, Env: params.Env,
	}
	var worker *ConversationWorker
	engine, cleanup, err := sm.engineFactory(conversationID, startParams, func(evt agent.Event) {
		if worker != nil {
			worker.EmitEvent(evt)
		}
	})
	if err != nil {
		return nil, nil, fmt.Errorf("create conversation engine: %w", err)
	}
	cleanupEngine := func() {
		if cleanup != nil {
			cleanup()
		}
	}
	store := engine.PersistenceStore()
	if store == nil {
		cleanupEngine()
		return nil, nil, fmt.Errorf("conversation persistence is unavailable")
	}
	workspace, err := canonicalWorkspace(engine.WorkspaceDir())
	if err != nil {
		cleanupEngine()
		return nil, nil, err
	}
	key = workspace + "\x00" + params.StartID
	if id := sm.startBindings[key]; id != "" {
		if existing, ok := sm.GetConversation(id); ok {
			cleanupEngine()
			return existing, &protocol.ConversationStartResult{ConversationID: id, SessionID: id, SubmissionID: params.SubmissionID, Existing: true}, nil
		}
	}

	turnID := generateWorkerID("turn")
	createdAt := time.Now().UTC()
	turnPointer := turnID
	request := &persistence.ConversationStartRequest{
		Session: persistence.SessionRecord{
			ID: conversationID, CreatedAt: createdAt, UpdatedAt: createdAt,
			Status: persistence.StatusRunning, Task: params.Prompt, Model: engine.ModelName(),
			WorkingDir: workspace, MaxSteps: engine.MaxSteps(), Role: "root", Depth: 0,
			InteractionKind: persistence.InteractionConversation,
		},
		StartID: params.StartID,
		FirstSubmission: persistence.SubmissionRecord{
			SubmissionID: params.SubmissionID, Prompt: params.Prompt, Mode: persistence.SubmissionQueue,
			TurnID: &turnPointer, Status: persistence.SubmissionAttached, CreatedAt: createdAt,
		},
	}
	if err := sm.registerWorkspace(ctx, workspace, store); err != nil {
		cleanupEngine()
		return nil, nil, fmt.Errorf("register conversation workspace: %w", err)
	}
	startResult, err := store.CreateConversation(ctx, request)
	if err != nil {
		cleanupEngine()
		return nil, nil, err
	}
	if existing, ok := sm.GetConversation(startResult.ConversationID); ok {
		cleanupEngine()
		sm.startBindings[key] = startResult.ConversationID
		first, getErr := existing.Submission(ctx, startResult.SubmissionID)
		if getErr != nil {
			return nil, nil, getErr
		}
		if startResult.SubmissionID != params.SubmissionID || first.PayloadFingerprint != persistence.SubmissionFingerprint(params.Prompt, persistence.SubmissionQueue) {
			return nil, nil, persistence.ErrSubmissionConflict
		}
		return existing, &protocol.ConversationStartResult{ConversationID: startResult.ConversationID, SessionID: startResult.ConversationID, SubmissionID: startResult.SubmissionID, TurnID: startResult.TurnID, Existing: true}, nil
	}

	var firstSubmission *persistence.SubmissionRecord
	if startResult.Existing {
		cleanupEngine()
		conversationID = startResult.ConversationID
		engine, cleanup, err = sm.engineFactory(conversationID, startParams, func(evt agent.Event) {
			if worker != nil {
				worker.EmitEvent(evt)
			}
		})
		if err != nil {
			return nil, nil, fmt.Errorf("restore conversation engine: %w", err)
		}
		cleanupEngine = func() {
			if cleanup != nil {
				cleanup()
			}
		}
		store = engine.PersistenceStore()
		if store == nil {
			cleanupEngine()
			return nil, nil, fmt.Errorf("conversation persistence is unavailable")
		}
		runtime, restoreErr := agent.RestoreConversationRuntime(ctx, engine, store, conversationID)
		if restoreErr != nil {
			cleanupEngine()
			return nil, nil, restoreErr
		}
		worker, err = NewConversationWorker(ctx, conversationID, engine, runtime, controller, cleanupEngine)
		if err != nil {
			cleanupEngine()
			return nil, nil, err
		}
	} else {
		historySession, historyErr := engine.NewHistorySession(params.Prompt)
		if historyErr != nil {
			cleanupEngine()
			return nil, nil, historyErr
		}
		runtime, runtimeErr := agent.NewConversationRuntime(ctx, engine, store, conversationID, params.SubmissionID, historySession)
		if runtimeErr != nil {
			cleanupEngine()
			return nil, nil, runtimeErr
		}
		worker, err = NewConversationWorker(ctx, conversationID, engine, runtime, controller, cleanupEngine)
		if err != nil {
			cleanupEngine()
			return nil, nil, err
		}
		firstSubmission, err = store.GetSubmission(ctx, conversationID, params.SubmissionID)
		if err != nil {
			worker.Stop()
			return nil, nil, err
		}
	}
	worker.SetOnIdle(func() { sm.onConversationIdle(conversationID) })
	sm.mu.Lock()
	sm.conversations[conversationID] = worker
	sm.startBindings[key] = conversationID
	sm.mu.Unlock()
	if startResult.Existing {
		err = worker.StartIdle()
	} else {
		err = worker.Start(firstSubmission)
	}
	if err != nil {
		sm.mu.Lock()
		delete(sm.conversations, conversationID)
		sm.mu.Unlock()
		worker.Stop()
		return nil, nil, err
	}
	return worker, &protocol.ConversationStartResult{
		ConversationID: startResult.ConversationID, SessionID: startResult.ConversationID,
		SubmissionID: startResult.SubmissionID, TurnID: startResult.TurnID, Existing: startResult.Existing,
	}, nil
}

// ResumeConversation restores an explicit root conversation or the latest one
// in the canonical workspace. Task sessions and child sessions are excluded.
func (sm *SessionManager) ResumeConversation(ctx context.Context, params protocol.ConversationResumeParams, controller ClientConn) (*ConversationWorker, *persistence.SessionRecord, error) {
	workspace, err := canonicalWorkspace(params.WorkspaceDir)
	if err != nil {
		return nil, nil, err
	}
	sm.conversationStartMu.Lock()
	defer sm.conversationStartMu.Unlock()
	if params.ConversationID != "" {
		if worker, ok := sm.GetConversation(params.ConversationID); ok {
			record, err := worker.SessionRecord(ctx)
			if err != nil {
				return nil, nil, err
			}
			if record.WorkingDir != workspace {
				return nil, nil, persistence.ErrSessionNotFound
			}
			return worker, record, nil
		}
	}
	engineID := params.ConversationID
	if engineID == "" {
		engineID = generateWorkerID("restore")
	}
	startParams := protocol.SessionStartParams{WorkspaceDir: workspace, DBPath: params.DBPath}
	var worker *ConversationWorker
	engine, cleanup, err := sm.engineFactory(engineID, startParams, func(evt agent.Event) {
		if worker != nil {
			worker.EmitEvent(evt)
		}
	})
	if err != nil {
		return nil, nil, fmt.Errorf("create engine for conversation restore: %w", err)
	}
	closeEngine := func() {
		if cleanup != nil {
			cleanup()
		}
	}
	store := engine.PersistenceStore()
	if store == nil {
		closeEngine()
		return nil, nil, errors.New("conversation persistence is unavailable")
	}
	var record *persistence.SessionRecord
	if params.ConversationID != "" {
		record, err = store.GetSession(ctx, params.ConversationID)
	} else {
		var conversations []*persistence.SessionRecord
		conversations, err = store.ListConversations(ctx, workspace, 1)
		if err == nil && len(conversations) == 0 {
			err = persistence.ErrSessionNotFound
		}
		if err == nil {
			record = conversations[0]
		}
	}
	if err != nil {
		closeEngine()
		return nil, nil, err
	}
	if record.InteractionKind != persistence.InteractionConversation || record.Depth != 0 || record.ParentSessionID != nil || record.WorkingDir != workspace {
		closeEngine()
		return nil, nil, persistence.ErrSessionNotFound
	}
	if existing, ok := sm.GetConversation(record.ID); ok {
		closeEngine()
		return existing, record, nil
	}
	closeEngine()
	startParams.Task = record.Task
	startParams.Model = record.Model
	startParams.MaxSteps = record.MaxSteps
	engine, cleanup, err = sm.engineFactory(record.ID, startParams, func(evt agent.Event) {
		if worker != nil {
			worker.EmitEvent(evt)
		}
	})
	if err != nil {
		return nil, nil, fmt.Errorf("recreate conversation engine: %w", err)
	}
	closeEngine = func() {
		if cleanup != nil {
			cleanup()
		}
	}
	store = engine.PersistenceStore()
	if store == nil {
		closeEngine()
		return nil, nil, errors.New("conversation persistence is unavailable")
	}
	runtime, err := agent.RestoreConversationRuntime(ctx, engine, store, record.ID)
	if err != nil {
		closeEngine()
		return nil, nil, err
	}
	turns, err := store.ListConversationTurns(ctx, record.ID)
	if err != nil {
		closeEngine()
		return nil, nil, err
	}
	submissions, err := store.ListSubmissions(ctx, record.ID)
	if err != nil {
		closeEngine()
		return nil, nil, err
	}
	now := time.Now().UTC()
	interruptedTurns := make(map[string]struct{})
	for _, turn := range turns {
		if turn.Status != persistence.StatusRunning {
			continue
		}
		message := "daemon restarted before the turn completed"
		turn.FinishedAt = &now
		turn.ErrorMessage = &message
		turn.Status = persistence.StatusInterrupted
		if err := store.UpdateConversationTurn(ctx, turn); err != nil {
			closeEngine()
			return nil, nil, err
		}
		interruptedTurns[turn.TurnID] = struct{}{}
	}
	for _, submission := range submissions {
		if submission.Status != persistence.SubmissionAttached || submission.TurnID == nil {
			continue
		}
		if _, wasInterrupted := interruptedTurns[*submission.TurnID]; !wasInterrupted {
			continue
		}
		if err := store.UpdateSubmissionStatus(ctx, record.ID, submission.SubmissionID, persistence.SubmissionConsumed, *submission.TurnID); err != nil {
			closeEngine()
			return nil, nil, err
		}
	}
	if record.Status == persistence.StatusRunning {
		if err := store.UpdateSessionStatus(ctx, record.ID, persistence.StatusInterrupted, record.TotalSteps, record.TokenUsage, "daemon restarted before the turn completed"); err != nil {
			closeEngine()
			return nil, nil, err
		}
	}
	worker, err = NewConversationWorker(ctx, record.ID, engine, runtime, controller, closeEngine)
	if err != nil {
		closeEngine()
		return nil, nil, err
	}
	worker.SetOnIdle(func() { sm.onConversationIdle(record.ID) })
	sm.mu.Lock()
	sm.conversations[record.ID] = worker
	sm.mu.Unlock()
	if err := worker.StartRestored(submissions); err != nil {
		sm.mu.Lock()
		delete(sm.conversations, record.ID)
		sm.mu.Unlock()
		worker.Stop()
		return nil, nil, err
	}
	return worker, record, nil
}

// ListConversationWorkspaces returns workspaces with persisted conversations.
func (sm *SessionManager) ListConversationWorkspaces(ctx context.Context, limit int) ([]protocol.ConversationWorkspace, error) {
	sm.mu.RLock()
	catalog := sm.workspaceCatalog
	sm.mu.RUnlock()
	if catalog == nil {
		return nil, errors.New("workspace catalog is unavailable")
	}
	entries, err := catalog.List(ctx)
	if err != nil {
		return nil, err
	}
	counts := make(map[string]int)
	updated := make(map[string]time.Time)
	for _, entry := range entries {
		eng, cleanup, err := sm.engineFactory(generateWorkerID("catalog"), protocol.SessionStartParams{WorkspaceDir: entry.WorkspacePath, DBPath: entry.DatabasePath}, nil)
		if err != nil {
			continue
		}
		store := eng.PersistenceStore()
		if store != nil {
			conversations, listErr := store.ListConversations(ctx, entry.WorkspacePath, 1000)
			if listErr == nil && len(conversations) > 0 {
				counts[entry.WorkspacePath] += len(conversations)
				if conversations[0].UpdatedAt.After(updated[entry.WorkspacePath]) {
					updated[entry.WorkspacePath] = conversations[0].UpdatedAt
				}
			}
		}
		if cleanup != nil {
			cleanup()
		}
	}
	workspaces := make([]protocol.ConversationWorkspace, 0, len(counts))
	for path, count := range counts {
		workspaces = append(workspaces, protocol.ConversationWorkspace{WorkspaceDir: path, ConversationCount: count, UpdatedAt: updated[path].UTC().Format(time.RFC3339Nano)})
	}
	sort.Slice(workspaces, func(i, j int) bool {
		if workspaces[i].UpdatedAt == workspaces[j].UpdatedAt {
			return workspaces[i].WorkspaceDir < workspaces[j].WorkspaceDir
		}
		return workspaces[i].UpdatedAt > workspaces[j].UpdatedAt
	})
	if limit > 0 && len(workspaces) > limit {
		workspaces = workspaces[:limit]
	}
	return workspaces, nil
}

// ListConversations returns persisted conversations for the requested workspace.
func (sm *SessionManager) ListConversations(ctx context.Context, workspace string, limit int) ([]protocol.ConversationSummary, error) {
	workspace, err := canonicalWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	sm.mu.RLock()
	catalog := sm.workspaceCatalog
	sm.mu.RUnlock()
	if catalog == nil {
		return nil, errors.New("workspace catalog is unavailable")
	}
	entries, err := catalog.List(ctx)
	if err != nil {
		return nil, err
	}
	var conversations []protocol.ConversationSummary
	for _, entry := range entries {
		if entry.WorkspacePath != workspace {
			continue
		}
		eng, cleanup, err := sm.engineFactory(generateWorkerID("list"), protocol.SessionStartParams{WorkspaceDir: workspace, DBPath: entry.DatabasePath}, nil)
		if err != nil {
			continue
		}
		store := eng.PersistenceStore()
		if store != nil {
			records, listErr := store.ListConversations(ctx, workspace, 1000)
			if listErr == nil {
				for _, record := range records {
					state := string(record.Status)
					if worker, ok := sm.GetConversation(record.ID); ok {
						state = worker.State()
					}
					conversations = append(conversations, protocol.ConversationSummary{
						ConversationID: record.ID, DBPath: entry.DatabasePath, Task: record.Task, State: state,
						CreatedAt: record.CreatedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: record.UpdatedAt.UTC().Format(time.RFC3339Nano),
						HistoryRevision: recordHistoryRevision(ctx, store, record.ID),
					})
				}
			}
		}
		if cleanup != nil {
			cleanup()
		}
	}
	sort.Slice(conversations, func(i, j int) bool { return conversations[i].UpdatedAt > conversations[j].UpdatedAt })
	if limit > 0 && len(conversations) > limit {
		conversations = conversations[:limit]
	}
	return conversations, nil
}

func recordHistoryRevision(ctx context.Context, store persistence.Store, id string) int64 {
	revision, _ := store.GetHistoryRevision(ctx, id)
	return revision
}

func canonicalWorkspace(workspace string) (string, error) {
	if workspace == "" {
		var err error
		workspace, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	abs, err := filepath.Abs(filepath.Clean(workspace))
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return abs, nil
}

// GetConversation returns the resident worker for a conversation when available.
func (sm *SessionManager) GetConversation(conversationID string) (*ConversationWorker, bool) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	w, ok := sm.conversations[conversationID]
	return w, ok
}

func (sm *SessionManager) onConversationIdle(conversationID string) {
	var evict []*ConversationWorker
	sm.mu.Lock()
	filtered := sm.idleConversationOrder[:0]
	for _, id := range sm.idleConversationOrder {
		if id != conversationID {
			filtered = append(filtered, id)
		}
	}
	sm.idleConversationOrder = append(filtered, conversationID)
	for len(sm.idleConversationOrder) > sm.maxCompletedSessions {
		id := sm.idleConversationOrder[0]
		sm.idleConversationOrder = sm.idleConversationOrder[1:]
		if w, ok := sm.conversations[id]; ok && !w.IsActive() {
			delete(sm.conversations, id)
			evict = append(evict, w)
		}
	}
	sm.mu.Unlock()
	for _, w := range evict {
		w.Evict()
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
	conversationWorkers := make([]*ConversationWorker, 0, len(sm.conversations))
	for _, w := range sm.conversations {
		conversationWorkers = append(conversationWorkers, w)
	}
	sm.mu.RUnlock()

	for _, w := range workers {
		w.Cancel()
	}
	for _, w := range conversationWorkers {
		w.Cancel()
	}

	deadline := time.Now().Add(timeout)
	stillActive := false
	for time.Now().Before(deadline) {
		stillActive = false
		for _, w := range workers {
			if w.IsActive() {
				stillActive = true
				break
			}
		}
		for _, w := range conversationWorkers {
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
	stillActive = false
	for _, w := range workers {
		stillActive = stillActive || w.IsActive()
	}
	for _, w := range conversationWorkers {
		stillActive = stillActive || w.IsActive()
	}
	if stillActive {
		for _, w := range conversationWorkers {
			w.StopAsync()
		}
		// Keep shared stores open while canceled executions may still be unwinding.
		// Drain returns at its deadline; resource closure follows once all workers exit.
		go func() {
			for {
				active := false
				for _, w := range workers {
					active = active || w.IsActive()
				}
				for _, w := range conversationWorkers {
					active = active || w.IsActive()
				}
				if !active {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			for _, w := range conversationWorkers {
				<-w.done
			}
			sm.closeStores()
		}()
		return
	}
	for _, w := range conversationWorkers {
		w.Stop()
	}
	sm.closeStores()
}
