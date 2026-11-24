package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
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

func conversationProviderProfile(engine *agent.Engine, params protocol.SessionStartParams) (*persistence.ConversationProviderProfile, error) {
	baseURL := engine.ProviderBaseURL()
	if !safeProviderEndpoint(baseURL) {
		return nil, errors.New("provider endpoint contains URL userinfo, query, or fragment data and cannot be persisted safely")
	}
	source := persistence.ConversationCredentialDaemonDefault
	if params.APIKey != nil {
		source = persistence.ConversationCredentialEphemeral
	}
	effort := engine.ReasoningEffort()
	if params.ReasoningEffort != nil {
		effort = *params.ReasoningEffort
	}
	return &persistence.ConversationProviderProfile{
		BaseURL: baseURL, APIType: engine.ProviderAPIType(), Model: engine.ModelName(),
		ReasoningEffort: effort, MaxSteps: engine.MaxSteps(), MaxContextTokens: engine.MaxContextTokens(),
		MaxOutputChars: engine.MaxOutputChars(), WorkingTailSize: engine.WorkingTailSize(),
		CredentialSource: source, RequiresMCPConfig: params.MCPConfig != nil, RequiresEnv: params.Env != nil,
	}, nil
}

func safeProviderEndpoint(baseURL string) bool {
	if baseURL == "" {
		return true
	}
	parsed, err := url.Parse(baseURL)
	return err == nil && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == ""
}

func conversationRestoreParams(workspace, dbPath string, record *persistence.SessionRecord, profile *persistence.ConversationProviderProfile, supplied protocol.ConversationResumeParams) (protocol.SessionStartParams, *persistence.ConversationProviderProfile, string) {
	blocked := ""
	unsafeEndpoint := false
	if profile != nil && !safeProviderEndpoint(profile.BaseURL) {
		blocked = "saved provider endpoint contains secret URL data; replace it before execution"
		unsafeEndpoint = true
		profile.BaseURL = ""
		profile.CredentialSource = persistence.ConversationCredentialEphemeral
	}
	if profile == nil {
		profile = &persistence.ConversationProviderProfile{
			BaseURL: supplied.BaseURL, APIType: supplied.APIType, Model: record.Model,
			MaxSteps: record.MaxSteps, CredentialSource: persistence.ConversationCredentialEphemeral,
		}
		if supplied.Model != "" {
			profile.Model = supplied.Model
		}
		profile.ReasoningEffort = derefStringValue(supplied.ReasoningEffort)
		profile.MaxContextTokens, profile.MaxOutputChars, profile.WorkingTailSize = supplied.MaxContextTokens, supplied.MaxOutputChars, supplied.WorkingTailSize
		profile.RequiresMCPConfig = supplied.MCPConfig != nil
		profile.RequiresEnv = supplied.Env != nil
		if !safeProviderEndpoint(profile.BaseURL) {
			blocked = "provider endpoint contains secret URL data; replace it before execution"
			unsafeEndpoint = true
			profile.BaseURL = ""
		}
		if blocked == "" {
			blocked = "conversation has no saved provider profile; re-supply provider settings and credentials before execution"
		}
		if !unsafeEndpoint && supplied.APIKey != nil && supplied.BaseURL != "" && supplied.APIType != "" {
			blocked = ""
		}
	}
	if unsafeEndpoint {
		// The sentinel scheme cannot route to a real HTTP endpoint while a
		// blocked worker restores its durable transcript.
		profile.BaseURL = "blocked://invalid"
	}
	if !unsafeEndpoint && (supplied.Model != "" && supplied.Model != profile.Model || supplied.BaseURL != "" && supplied.BaseURL != profile.BaseURL || supplied.APIType != "" && supplied.APIType != profile.APIType) {
		blocked = "current provider settings do not match the saved conversation profile"
	}
	if !unsafeEndpoint && (supplied.ReasoningEffort != nil && *supplied.ReasoningEffort != profile.ReasoningEffort || supplied.MaxSteps > 0 && supplied.MaxSteps != profile.MaxSteps || supplied.MaxContextTokens > 0 && supplied.MaxContextTokens != profile.MaxContextTokens || supplied.MaxOutputChars > 0 && supplied.MaxOutputChars != profile.MaxOutputChars || supplied.WorkingTailSize > 0 && supplied.WorkingTailSize != profile.WorkingTailSize) {
		blocked = "current model settings do not match the saved conversation profile"
	}
	if profile.CredentialSource == persistence.ConversationCredentialEphemeral && supplied.APIKey == nil {
		blocked = "conversation credentials are required again before execution"
	}
	if profile.RequiresMCPConfig && supplied.MCPConfig == nil {
		blocked = "conversation requires its MCP configuration to be re-supplied before execution"
	}
	if profile.RequiresEnv && len(supplied.Env) == 0 {
		blocked = "conversation requires its execution environment to be re-supplied before execution"
	}
	key := (*string)(nil)
	if profile.CredentialSource == persistence.ConversationCredentialEphemeral && !unsafeEndpoint {
		key = supplied.APIKey
	}
	effort := profile.ReasoningEffort
	return protocol.SessionStartParams{
		Task: record.Task, WorkspaceDir: workspace, DBPath: dbPath, Model: profile.Model, BaseURL: profile.BaseURL,
		APIType: profile.APIType, APIKey: key, CredentialSource: profile.CredentialSource,
		ReasoningEffort: &effort, MaxSteps: profile.MaxSteps, MaxContextTokens: profile.MaxContextTokens,
		MaxOutputChars: profile.MaxOutputChars, WorkingTailSize: profile.WorkingTailSize,
		MCPConfig: supplied.MCPConfig, Env: supplied.Env,
	}, profile, blocked
}

func derefStringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func resumeParamsFromStart(workspace string, params protocol.ConversationStartParams) protocol.ConversationResumeParams {
	return protocol.ConversationResumeParams{
		WorkspaceDir: workspace, DBPath: params.DBPath, Model: params.Model, BaseURL: params.BaseURL, APIType: params.APIType,
		APIKey: params.APIKey, ReasoningEffort: params.ReasoningEffort, MaxSteps: params.MaxSteps,
		MaxContextTokens: params.MaxContextTokens, MaxOutputChars: params.MaxOutputChars,
		WorkingTailSize: params.WorkingTailSize, MCPConfig: params.MCPConfig, Env: params.Env,
	}
}

func (sm *SessionManager) createConversationRestoreEngine(conversationID string, params protocol.SessionStartParams, emit agent.EventHandler) (*agent.Engine, func(), string, error) {
	engine, cleanup, err := sm.engineFactory(conversationID, params, emit)
	if err == nil {
		return engine, cleanup, "", nil
	}
	if params.CredentialSource != persistence.ConversationCredentialDaemonDefault {
		return nil, nil, "", err
	}
	// The saved endpoint no longer matches daemon-default credentials. Rebuild
	// with no credentials so transcript restore remains available but cannot
	// accidentally send requests to another endpoint.
	params.CredentialSource = persistence.ConversationCredentialEphemeral
	params.APIKey = nil
	engine, cleanup, fallbackErr := sm.engineFactory(conversationID, params, emit)
	if fallbackErr != nil {
		return nil, nil, "", fmt.Errorf("restore history without provider credentials: %w", fallbackErr)
	}
	blocked := "saved endpoint does not match the daemon-default provider endpoint; restore the daemon's saved provider configuration before execution"
	return engine, cleanup, blocked, nil
}

func (sm *SessionManager) reconfigureBlockedConversation(ctx context.Context, worker *ConversationWorker, record *persistence.SessionRecord, profile *persistence.ConversationProviderProfile, supplied protocol.ConversationResumeParams) error {
	if worker.ExecutionBlocked() == "" {
		return nil
	}
	params, profile, blocked := conversationRestoreParams(record.WorkingDir, supplied.DBPath, record, profile, supplied)
	if blocked != "" {
		return nil
	}
	engine, cleanup, fallbackBlock, err := sm.createConversationRestoreEngine(record.ID, params, func(evt agent.Event) { worker.EmitEvent(evt) })
	if err != nil {
		return fmt.Errorf("reconfigure conversation execution: %w", err)
	}
	if fallbackBlock != "" {
		if cleanup != nil {
			cleanup()
		}
		return nil
	}
	store := engine.PersistenceStore()
	if store == nil {
		if cleanup != nil {
			cleanup()
		}
		return errors.New("conversation persistence is unavailable")
	}
	runtime, err := agent.RestoreConversationRuntime(ctx, engine, store, record.ID)
	if err != nil {
		if cleanup != nil {
			cleanup()
		}
		return err
	}
	if profile != nil {
		if err := worker.store.SaveConversationProviderProfile(ctx, record.ID, profile); err != nil {
			if cleanup != nil {
				cleanup()
			}
			return err
		}
	}
	if err := worker.ReplaceBlockedExecution(engine, runtime, cleanup); err != nil {
		if cleanup != nil {
			cleanup()
		}
		return err
	}
	return nil
}

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
			record, err := worker.SessionRecord(ctx)
			if err != nil {
				return nil, nil, err
			}
			profile, err := worker.store.GetConversationProviderProfile(ctx, id)
			if err != nil {
				return nil, nil, err
			}
			if err := sm.reconfigureBlockedConversation(ctx, worker, record, profile, resumeParamsFromStart(requestedWorkspace, params)); err != nil {
				return nil, nil, err
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
	providerProfile, err := conversationProviderProfile(engine, startParams)
	if err != nil {
		cleanupEngine()
		return nil, nil, err
	}
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
		ProviderProfile: providerProfile,
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
		record, recordErr := existing.SessionRecord(ctx)
		if recordErr != nil {
			return nil, nil, recordErr
		}
		profile, profileErr := existing.store.GetConversationProviderProfile(ctx, startResult.ConversationID)
		if profileErr != nil {
			return nil, nil, profileErr
		}
		if err := sm.reconfigureBlockedConversation(ctx, existing, record, profile, resumeParamsFromStart(workspace, params)); err != nil {
			return nil, nil, err
		}
		return existing, &protocol.ConversationStartResult{ConversationID: startResult.ConversationID, SessionID: startResult.ConversationID, SubmissionID: startResult.SubmissionID, TurnID: startResult.TurnID, Existing: true}, nil
	}

	var firstSubmission *persistence.SubmissionRecord
	if startResult.Existing {
		if startResult.SubmissionID != params.SubmissionID {
			cleanupEngine()
			return nil, nil, persistence.ErrSubmissionConflict
		}
		record, recordErr := store.GetSession(ctx, startResult.ConversationID)
		if recordErr != nil {
			cleanupEngine()
			return nil, nil, recordErr
		}
		profile, profileErr := store.GetConversationProviderProfile(ctx, startResult.ConversationID)
		if profileErr != nil {
			cleanupEngine()
			return nil, nil, profileErr
		}
		hadProviderProfile := profile != nil
		restoreParams, profile, executionBlocked := conversationRestoreParams(workspace, params.DBPath, record, profile, resumeParamsFromStart(workspace, params))
		cleanupEngine()
		conversationID = startResult.ConversationID
		engine, cleanup, fallbackBlocked, err := sm.createConversationRestoreEngine(conversationID, restoreParams, func(evt agent.Event) {
			if worker != nil {
				worker.EmitEvent(evt)
			}
		})
		if err != nil {
			return nil, nil, fmt.Errorf("restore conversation engine: %w", err)
		}
		if fallbackBlocked != "" {
			executionBlocked = fallbackBlocked
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
		if !hadProviderProfile && executionBlocked == "" {
			if err := store.SaveConversationProviderProfile(ctx, conversationID, profile); err != nil {
				cleanupEngine()
				return nil, nil, err
			}
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
		if executionBlocked != "" {
			worker.SetExecutionBlocked(executionBlocked)
		}
	} else {
		historySession, historyErr := engine.NewHistorySession(params.Prompt)
		if historyErr != nil {
			cleanupEngine()
			return nil, nil, historyErr
		}
		baselineCtx, baselineCancel := context.WithTimeout(ctx, 3*time.Second)
		baselinePaths, _, baselineErr := gitChangedPaths(baselineCtx, workspace)
		baselineCancel()
		historySession.Tracker().SetInitialDirtyPaths(baselinePaths, baselineErr == nil)
		runtime, runtimeErr := agent.NewConversationRuntime(ctx, engine, store, conversationID, params.SubmissionID, historySession)
		if runtimeErr != nil {
			cleanupEngine()
			return nil, nil, runtimeErr
		}
		narrative, trackerJSON, tokens := historySession.ExtractSnapshot()
		initialRevision, revisionErr := store.GetHistoryRevision(ctx, conversationID)
		if revisionErr != nil {
			cleanupEngine()
			return nil, nil, revisionErr
		}
		if err := store.SaveSnapshot(ctx, &persistence.SnapshotRecord{
			SessionID: conversationID, StepNumber: 0, DistilledNarrative: narrative,
			StateTrackerJSON: trackerJSON, CurrentTokens: tokens, HistoryRevision: initialRevision, CreatedAt: time.Now().UTC(),
		}); err != nil {
			cleanupEngine()
			return nil, nil, fmt.Errorf("persist initial conversation baseline: %w", err)
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
			profile, err := worker.store.GetConversationProviderProfile(ctx, record.ID)
			if err != nil {
				return nil, nil, err
			}
			if err := sm.reconfigureBlockedConversation(ctx, worker, record, profile, params); err != nil {
				return nil, nil, err
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
		profile, err := existing.store.GetConversationProviderProfile(ctx, record.ID)
		if err != nil {
			return nil, nil, err
		}
		if err := sm.reconfigureBlockedConversation(ctx, existing, record, profile, params); err != nil {
			return nil, nil, err
		}
		return existing, record, nil
	}
	profile, err := store.GetConversationProviderProfile(ctx, record.ID)
	if err != nil {
		closeEngine()
		return nil, nil, err
	}
	hadProviderProfile := profile != nil
	startParams, profile, executionBlocked := conversationRestoreParams(workspace, params.DBPath, record, profile, params)
	closeEngine()
	engine, cleanup, fallbackBlocked, err := sm.createConversationRestoreEngine(record.ID, startParams, func(evt agent.Event) {
		if worker != nil {
			worker.EmitEvent(evt)
		}
	})
	if err != nil {
		return nil, nil, fmt.Errorf("recreate conversation engine: %w", err)
	}
	if fallbackBlocked != "" {
		executionBlocked = fallbackBlocked
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
	if !hadProviderProfile && executionBlocked == "" {
		if err := store.SaveConversationProviderProfile(ctx, record.ID, profile); err != nil {
			closeEngine()
			return nil, nil, err
		}
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
	if executionBlocked != "" {
		worker.SetExecutionBlocked(executionBlocked)
	}
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
	worker, ok := sm.conversations[conversationID]
	if !ok || !worker.idleForRetention() {
		sm.mu.Unlock()
		return
	}
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
		if w, ok := sm.conversations[id]; ok && w.reserveEviction() {
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
