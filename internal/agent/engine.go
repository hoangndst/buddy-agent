package agent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/hoangndst/buddy-agent/internal/agent/provider"
	"github.com/hoangndst/buddy-agent/internal/history"
	"github.com/hoangndst/buddy-agent/internal/memory"
	"github.com/hoangndst/buddy-agent/internal/persistence"
	"github.com/hoangndst/buddy-agent/internal/tools"
	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

// EngineOption configures operational parameters for the Agent Engine.
type EngineOption func(*Engine)

// ToolInterceptor authorizes a tool call and may replace its JSON arguments.
type ToolInterceptor func(ctx context.Context, tool string, args json.RawMessage) (allow bool, modifiedArgs json.RawMessage, err error)

// ToolDeniedError reports an explicit user denial without aborting the ReAct turn.
type ToolDeniedError struct{ Reason string }

func (e *ToolDeniedError) Error() string { return e.Reason }

// WithToolInterceptor configures human authorization for tool execution.
func WithToolInterceptor(interceptor ToolInterceptor) EngineOption {
	return func(e *Engine) { e.toolInterceptor = interceptor }
}

// WithMaxContextTokens sets the hard context token ceiling on the engine.
func WithMaxContextTokens(tokens int) EngineOption {
	return func(e *Engine) {
		if tokens > 0 {
			e.maxContextTokens = tokens
		}
	}
}

// WithWorkingTailSize sets the unpruned active turn tail size on the engine.
func WithWorkingTailSize(size int) EngineOption {
	return func(e *Engine) {
		if size > 0 {
			e.workingTailSize = size
		}
	}
}

// WithIsLocal configures whether the engine is communicating with a local LLM endpoint (e.g. Ollama).
func WithIsLocal(isLocal bool) EngineOption {
	return func(e *Engine) {
		e.isLocal = isLocal
	}
}

// WithReasoningEffort sets the reasoning effort parameter on the engine.
func WithReasoningEffort(effort string) EngineOption {
	return func(e *Engine) {
		e.reasoningEffort = effort
	}
}

// WithProviderMetadata records nonsecret provider settings for safe conversation restore.
func WithProviderMetadata(baseURL, apiType string, maxOutputChars int) EngineOption {
	return func(e *Engine) {
		e.providerBaseURL = baseURL
		e.providerAPIType = apiType
		e.maxOutputChars = maxOutputChars
	}
}

// WithStore configures persistent SQLite storage for the Agent Engine.
func WithStore(s persistence.Store) EngineOption {
	return func(e *Engine) {
		e.store = s
	}
}

// WithWorkspaceDir sets the active workspace directory for trajectory recording.
func WithWorkspaceDir(dir string) EngineOption {
	return func(e *Engine) {
		e.workspaceDir = dir
	}
}

// WithCompletionClient configures a custom completion client for memory compaction summarization.
func WithCompletionClient(client history.CompletionClient) EngineOption {
	return func(e *Engine) {
		e.compClient = client
	}
}

// WithDepth sets the execution depth of the engine (0 for root, 1 for subagent).
func WithDepth(depth int) EngineOption {
	return func(e *Engine) {
		e.depth = depth
	}
}

// WithParentSessionID sets the parent session ID for hierarchical persistence.
func WithParentSessionID(id string) EngineOption {
	return func(e *Engine) {
		e.parentSessionID = id
	}
}

// WithRole sets the operational role of the engine (root, researcher, worker).
func WithRole(role string) EngineOption {
	return func(e *Engine) {
		e.role = role
	}
}

// WithSystemPromptOverride configures a dedicated system prompt, bypassing MemoryManager.
func WithSystemPromptOverride(prompt string) EngineOption {
	return func(e *Engine) {
		e.systemPromptOverride = prompt
	}
}

// WithInheritedInvariants injects parent project invariants into child state tracking.
func WithInheritedInvariants(invariants []string) EngineOption {
	return func(e *Engine) {
		e.inheritedInvariants = invariants
	}
}

// WithSessionIDOverride configures an explicit session ID on the engine,
// ensuring parity between daemon session identifiers and persistence stores.
func WithSessionIDOverride(sessionID string) EngineOption {
	return func(e *Engine) {
		e.sessionIDOverride = sessionID
	}
}

// providerCompletionAdapter bridges a ModelProvider to history.CompletionClient.
type providerCompletionAdapter struct {
	prov  provider.ModelProvider
	model string
}

func (a *providerCompletionAdapter) CreateChatCompletion(ctx context.Context, params openai.ChatCompletionNewParams, _ ...option.RequestOption) (*openai.ChatCompletion, error) {
	req := provider.GenerateRequest{
		Model:    params.Model,
		Messages: params.Messages,
	}
	resp, err := a.prov.Generate(ctx, req)
	if err != nil {
		return nil, err
	}
	return &openai.ChatCompletion{
		Choices: []openai.ChatCompletionChoice{
			{
				Message: resp.ToChatMessage(),
			},
		},
		Usage: openai.CompletionUsage{
			PromptTokens:     resp.Usage.PromptTokens,
			CompletionTokens: resp.Usage.CompletionTokens,
			TotalTokens:      resp.Usage.TotalTokens,
		},
	}, nil
}

// ExecutionResult captures post-execution metrics and state changes from an engine run.
type ExecutionResult struct {
	SessionID     string
	Status        persistence.SessionStatus
	Summary       string
	ModifiedFiles map[string]history.FileRecord
	TokenUsage    persistence.TokenUsage
}

// Engine orchestrates the ReAct loop: prompting, tool execution, and memory feedback.
type Engine struct {
	provider                 provider.ModelProvider
	compClient               history.CompletionClient
	model                    string
	reasoningEffort          string
	providerBaseURL          string
	providerAPIType          string
	maxOutputChars           int
	maxSteps                 int
	maxContextTokens         int
	workingTailSize          int
	isLocal                  bool
	registry                 *tools.Registry
	memoryMgr                *memory.Manager
	eventHandler             EventHandler
	toolInterceptor          ToolInterceptor
	store                    persistence.Store
	workspaceDir             string
	depth                    int
	parentSessionID          string
	sessionIDOverride        string
	role                     string
	systemPromptOverride     string
	inheritedInvariants      []string
	lastExecutionResult      *ExecutionResult
	conversationCommitHook   func(func() error, []Event) error
	conversationBoundaryHook func(context.Context, string, string, *history.Session, int) error
}

// SetConversationCommitHook orders revision-advancing persistence writes with
// their associated worker events for a resident conversation.
func (e *Engine) SetConversationCommitHook(hook func(func() error, []Event) error) {
	e.conversationCommitHook = hook
}

// SetConversationBoundaryHook installs a callback run before each model request.
func (e *Engine) SetConversationBoundaryHook(hook func(context.Context, string, string, *history.Session, int) error) {
	e.conversationBoundaryHook = hook
}

// SetToolInterceptor installs the daemon's tool authorization callback.
func (e *Engine) SetToolInterceptor(interceptor ToolInterceptor) { e.toolInterceptor = interceptor }

func (e *Engine) commitAndEmit(commit func() error, events ...Event) error {
	if e.conversationCommitHook != nil {
		return e.conversationCommitHook(commit, events)
	}
	if commit != nil {
		if err := commit(); err != nil {
			return err
		}
	}
	for _, event := range events {
		e.eventHandler(event)
	}
	return nil
}

// LastExecutionResult returns the execution result from the most recent Run invocation.
func (e *Engine) LastExecutionResult() *ExecutionResult {
	return e.lastExecutionResult
}

// Registry returns the tool registry configured for this engine.
func (e *Engine) Registry() *tools.Registry {
	return e.registry
}

// WorkingTailSize returns the configured unpruned active turn tail size.
func (e *Engine) WorkingTailSize() int {
	return e.workingTailSize
}

// ProviderBaseURL returns the configured endpoint without credentials.
func (e *Engine) ProviderBaseURL() string { return e.providerBaseURL }

// ProviderAPIType returns the configured provider protocol.
func (e *Engine) ProviderAPIType() string { return e.providerAPIType }

// ReasoningEffort returns the configured model reasoning effort.
func (e *Engine) ReasoningEffort() string { return e.reasoningEffort }

// MaxContextTokens returns the engine's configured context limit.
func (e *Engine) MaxContextTokens() int { return e.maxContextTokens }

// MaxOutputChars returns the configured tool output limit.
func (e *Engine) MaxOutputChars() int { return e.maxOutputChars }

// PersistenceStore returns the store configured for this engine, if any.
// Conversation runtimes use the same store as the engine so transcript writes
// and turn execution share one persistence boundary.
func (e *Engine) PersistenceStore() persistence.Store {
	return e.store
}

// WorkspaceDir returns the workspace configured for this engine.
func (e *Engine) WorkspaceDir() string {
	return e.workspaceDir
}

// ModelName returns the provider model configured for this engine.
func (e *Engine) ModelName() string {
	return e.model
}

// MaxSteps returns the per-turn step budget configured for this engine.
func (e *Engine) MaxSteps() int {
	return e.maxSteps
}

// NewEngine creates a new Agent Engine.
func NewEngine(
	prov provider.ModelProvider,
	model string,
	maxSteps int,
	reg *tools.Registry,
	mem *memory.Manager,
	handler EventHandler,
	opts ...EngineOption,
) *Engine {
	if maxSteps <= 0 {
		maxSteps = 30
	}
	if handler == nil {
		handler = func(Event) {}
	}
	e := &Engine{
		provider:         prov,
		model:            model,
		maxSteps:         maxSteps,
		maxContextTokens: 64000,
		workingTailSize:  5,
		isLocal:          false,
		registry:         reg,
		memoryMgr:        mem,
		eventHandler:     handler,
	}
	for _, opt := range opts {
		opt(e)
	}
	if e.compClient == nil {
		if comp, ok := prov.(history.CompletionClient); ok {
			e.compClient = comp
		} else {
			e.compClient = &providerCompletionAdapter{prov: e.provider, model: e.model}
		}
	}
	return e
}

// Run executes a multi-step task to completion or until max steps are exhausted.
func (e *Engine) Run(ctx context.Context, task string) (string, error) {
	if task == "" {
		return "", errors.New("task cannot be empty")
	}
	e.lastExecutionResult = nil

	session, err := e.NewHistorySession(task)
	if err != nil {
		return "", err
	}

	sessionID := generateSessionID()
	if e.sessionIDOverride != "" {
		sessionID = e.sessionIDOverride
	}
	if e.registry != nil {
		if t, ok := e.registry.Get("invoke_subagent"); ok {
			if subTool, ok := t.(*InvokeSubagentTool); ok {
				subTool.SetParentSessionID(sessionID)
				subTool.SetParentTracker(session.Tracker())
				subTool.Reset()
			}
		}
	}
	if e.store != nil {
		role := e.role
		if role == "" {
			if e.depth == 0 {
				role = "root"
			} else {
				role = "subagent"
			}
		}
		sessRec := &persistence.SessionRecord{
			ID:         sessionID,
			CreatedAt:  time.Now().UTC(),
			UpdatedAt:  time.Now().UTC(),
			Status:     persistence.StatusRunning,
			Task:       task,
			Model:      e.model,
			WorkingDir: e.workspaceDir,
			MaxSteps:   e.maxSteps,
			TotalSteps: 0,
			Role:       role,
			Depth:      e.depth,
		}
		if e.parentSessionID != "" {
			sessRec.ParentSessionID = &e.parentSessionID
		}
		_ = e.store.CreateSession(context.Background(), sessRec)
	}

	// Guard against oversized initial task or system prompt exceeding context ceiling
	if err := session.EnsureWatermarks(ctx, e.compClient, e.model); err != nil {
		if errors.Is(err, history.ErrContextExhausted) {
			e.eventHandler(Event{Type: EventContextExhausted, Step: 0, Error: err})
			if e.store != nil {
				_ = e.store.UpdateSessionStatus(context.Background(), sessionID, persistence.StatusFailed, 0, persistence.TokenUsage{}, err.Error())
			}
			return "", err
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			if e.store != nil {
				_ = e.store.UpdateSessionStatus(context.Background(), sessionID, persistence.StatusInterrupted, 0, persistence.TokenUsage{}, "Process interrupted by signal or user")
			}
			return "", err
		}
		e.eventHandler(Event{Type: EventError, Step: 0, Error: err})
		if e.store != nil {
			_ = e.store.UpdateSessionStatus(context.Background(), sessionID, persistence.StatusFailed, 0, persistence.TokenUsage{}, err.Error())
		}
		return "", fmt.Errorf("initial context validation failed: %w", err)
	}

	return e.executeLoop(ctx, sessionID, "", session, 1, e.maxSteps, persistence.TokenUsage{})
}

// NewHistorySession creates an in-memory history with the engine's current system prompt and limits.
func (e *Engine) NewHistorySession(rootPrompt string) (*history.Session, error) {
	if rootPrompt == "" {
		return nil, errors.New("root prompt cannot be empty")
	}
	systemPrompt, err := e.currentSystemPrompt()
	if err != nil {
		return nil, err
	}
	session := history.NewSession(
		systemPrompt,
		rootPrompt,
		history.WithMaxContextTokens(e.maxContextTokens),
		history.WithWorkingTailSize(e.workingTailSize),
	)
	for _, invariant := range e.inheritedInvariants {
		session.Tracker().AddInvariant(invariant)
	}
	return session, nil
}

func (e *Engine) currentSystemPrompt() (string, error) {
	var systemPrompt string
	if e.systemPromptOverride != "" {
		systemPrompt = e.systemPromptOverride
	} else if e.memoryMgr != nil {
		var err error
		systemPrompt, err = e.memoryMgr.LoadSystemPrompt()
		if err != nil {
			return "", fmt.Errorf("failed to load system prompt: %w", err)
		}
	}
	return systemPrompt, nil
}

// RunConversationTurn executes one user-facing turn against an existing conversation history.
// Step numbers remain global to the persisted session while the step budget resets for this turn.
func (e *Engine) RunConversationTurn(
	ctx context.Context,
	sessionID, conversationTurnID string,
	session *history.Session,
	startStep, stepBudget int,
	initialUsage persistence.TokenUsage,
) (string, error) {
	if sessionID == "" || conversationTurnID == "" || session == nil {
		return "", errors.New("conversation turn requires session ID, turn ID, and history")
	}
	if startStep <= 0 {
		startStep = 1
	}
	if stepBudget <= 0 {
		stepBudget = e.maxSteps
	}
	e.lastExecutionResult = nil
	if e.memoryMgr != nil && e.systemPromptOverride == "" {
		prompt, err := e.memoryMgr.LoadSystemPrompt()
		if err != nil {
			return "", fmt.Errorf("failed to refresh system prompt: %w", err)
		}
		session.ReloadSystemAnchor(prompt)
	}
	if e.registry != nil {
		if tool, ok := e.registry.Get("invoke_subagent"); ok {
			if subTool, ok := tool.(*InvokeSubagentTool); ok {
				subTool.SetParentSessionID(sessionID)
				subTool.SetParentTracker(session.Tracker())
				subTool.Reset()
			}
		}
	}
	if e.store != nil {
		if err := e.store.UpdateSessionStatus(ctx, sessionID, persistence.StatusRunning, startStep-1, initialUsage, ""); err != nil {
			return "", fmt.Errorf("failed to mark conversation active: %w", err)
		}
	}
	if err := session.EnsureWatermarks(ctx, e.compClient, e.model); err != nil {
		if e.store != nil {
			status := persistence.StatusFailed
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				status = persistence.StatusInterrupted
			}
			_ = e.store.UpdateSessionStatus(context.Background(), sessionID, status, startStep-1, initialUsage, err.Error())
		}
		return "", err
	}
	return e.executeLoop(ctx, sessionID, conversationTurnID, session, startStep, startStep+stepBudget-1, initialUsage)
}

// Resume restores an interrupted or failed session from persistence and continues execution.
func (e *Engine) Resume(ctx context.Context, sessionID string, maxStepsOverride int) (string, error) {
	if e.store == nil {
		return "", errors.New("persistence store is required to resume session")
	}

	sessionRec, err := e.store.GetSession(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("failed to load session %q: %w", sessionID, err)
	}
	if sessionRec.InteractionKind == persistence.InteractionConversation {
		return "", fmt.Errorf("session %q is a conversation; resume it with the conversation interface", sessionID)
	}

	if sessionRec.Status == persistence.StatusCompleted {
		return "", persistence.ErrSessionCompleted
	}

	if sessionRec.Depth > 0 || (sessionRec.ParentSessionID != nil && *sessionRec.ParentSessionID != "") {
		return "", fmt.Errorf("cannot directly resume child subagent session %q (depth=%d, role=%q)", sessionID, sessionRec.Depth, sessionRec.Role)
	}

	activeDir := e.workspaceDir
	if activeDir == "" {
		activeDir = "."
	}
	recordedDir := sessionRec.WorkingDir
	if recordedDir == "" {
		recordedDir = "."
	}
	cleanCurrent, err1 := filepath.Abs(filepath.Clean(activeDir))
	cleanRecorded, err2 := filepath.Abs(filepath.Clean(recordedDir))
	if err1 == nil && err2 == nil && cleanCurrent != cleanRecorded {
		return "", fmt.Errorf("cannot resume session %q: working directory mismatch (session: %s, active workspace: %s)",
			sessionID, sessionRec.WorkingDir, activeDir)
	}

	// 1. Build initial system prompt
	systemPrompt, err := e.memoryMgr.LoadSystemPrompt()
	if err != nil {
		return "", fmt.Errorf("failed to load system prompt: %w", err)
	}

	// 2. Fetch latest snapshot
	snap, err := e.store.GetLatestSnapshot(ctx, sessionRec.ID)
	if err != nil {
		return "", fmt.Errorf("failed to fetch snapshot: %w", err)
	}

	compactedStep := 0
	narrative := ""
	var tracker *history.StateTracker
	if snap != nil {
		compactedStep = snap.StepNumber
		narrative = snap.DistilledNarrative
		if snap.StateTrackerJSON != "" {
			var trackerData history.StateTrackerData
			if err := json.Unmarshal([]byte(snap.StateTrackerJSON), &trackerData); err == nil {
				tracker = history.RestoreStateTracker(trackerData)
			}
		}
	}
	if tracker == nil {
		tracker = history.NewStateTracker(sessionRec.Task, nil)
	}

	// 3. Fetch trailing turns after compactedStep
	turnRecords, err := e.store.GetTurnsAfterStep(ctx, sessionRec.ID, compactedStep)
	if err != nil {
		return "", fmt.Errorf("failed to fetch trailing turns: %w", err)
	}

	trailingBlocks := make([]history.TurnBlock, len(turnRecords))
	for i, tr := range turnRecords {
		trailingBlocks[i] = mapTurnRecordToBlock(tr)
		for _, tc := range tr.ToolCalls {
			if tc.Name == "write_file" || tc.Name == "edit_file" {
				var args struct{ Path string }
				if json.Unmarshal([]byte(tc.Args), &args) == nil && args.Path != "" {
					tracker.RecordFileAccess(args.Path, "modified")
				}
			}
			if tc.ExitCode != 0 {
				tracker.RecordError(fmt.Sprintf("%s: %s", tc.Name, tc.Result))
			}
		}
	}

	restoredState := history.RestoredState{
		SessionID:         sessionRec.ID,
		Task:              sessionRec.Task,
		SystemAnchor:      systemPrompt,
		CompactedStep:     compactedStep,
		EpisodicNarrative: narrative,
		StateTracker:      tracker,
		TrailingTurns:     trailingBlocks,
	}

	session, err := history.RestoreSession(
		restoredState,
		history.WithMaxContextTokens(e.maxContextTokens),
		history.WithWorkingTailSize(e.workingTailSize),
	)
	if err != nil {
		return "", fmt.Errorf("failed to restore history session: %w", err)
	}

	if e.registry != nil {
		if t, ok := e.registry.Get("invoke_subagent"); ok {
			if subTool, ok := t.(*InvokeSubagentTool); ok {
				subTool.SetParentSessionID(sessionRec.ID)
				subTool.SetParentTracker(session.Tracker())
				subTool.Reset()
				if e.store != nil {
					if children, err := e.store.GetChildSessions(ctx, sessionRec.ID); err == nil {
						subTool.SetCallCount(len(children))
					}
				}
			}
		}
	}

	maxStep := 0
	allTurns, _ := e.store.GetAllTurns(ctx, sessionRec.ID)
	for _, t := range allTurns {
		if t.StepNumber > maxStep {
			maxStep = t.StepNumber
		}
	}
	startStep := maxStep + 1

	totalMaxSteps := sessionRec.MaxSteps
	if maxStepsOverride > 0 {
		totalMaxSteps = startStep - 1 + maxStepsOverride
	} else if startStep > totalMaxSteps {
		totalMaxSteps = startStep + 10
	}

	_ = e.store.UpdateSessionStatus(ctx, sessionRec.ID, persistence.StatusRunning, startStep-1, sessionRec.TokenUsage, "")

	// Guard against oversized restored history exceeding context ceiling
	if err := session.EnsureWatermarks(ctx, e.compClient, e.model); err != nil {
		if errors.Is(err, history.ErrContextExhausted) {
			e.eventHandler(Event{Type: EventContextExhausted, Step: startStep - 1, Error: err})
			if e.store != nil {
				_ = e.store.UpdateSessionStatus(context.Background(), sessionRec.ID, persistence.StatusFailed, startStep-1, sessionRec.TokenUsage, err.Error())
			}
			return "", err
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			if e.store != nil {
				_ = e.store.UpdateSessionStatus(context.Background(), sessionRec.ID, persistence.StatusInterrupted, startStep-1, sessionRec.TokenUsage, "Process interrupted by signal or user")
			}
			return "", err
		}
		e.eventHandler(Event{Type: EventError, Step: startStep - 1, Error: err})
		if e.store != nil {
			_ = e.store.UpdateSessionStatus(context.Background(), sessionRec.ID, persistence.StatusFailed, startStep-1, sessionRec.TokenUsage, err.Error())
		}
		return "", fmt.Errorf("resumed context validation failed: %w", err)
	}

	return e.executeLoop(ctx, sessionRec.ID, "", session, startStep, totalMaxSteps, sessionRec.TokenUsage)
}

func (e *Engine) executeLoop(
	ctx context.Context,
	sessionID string,
	conversationTurnID string,
	session *history.Session,
	startStep, totalMaxSteps int,
	initialUsage persistence.TokenUsage,
) (string, error) {
	const maxTurnRetries = 2
	cumulativeUsage := initialUsage

	getBillingUsage := func() persistence.TokenUsage {
		usage := cumulativeUsage
		if e.registry != nil {
			if t, ok := e.registry.Get("invoke_subagent"); ok {
				if subTool, ok := t.(*InvokeSubagentTool); ok {
					cUsage := subTool.ChildUsage()
					usage.PromptTokens += cUsage.PromptTokens
					usage.CompletionTokens += cUsage.CompletionTokens
					usage.ReasoningTokens += cUsage.ReasoningTokens
					usage.TotalTokens += cUsage.TotalTokens
				}
			}
		}
		return usage
	}

	defer func() {
		if e.lastExecutionResult == nil {
			e.lastExecutionResult = &ExecutionResult{
				SessionID:     sessionID,
				Status:        persistence.StatusFailed,
				ModifiedFiles: session.Tracker().ExportData().ModifiedFiles,
				TokenUsage:    getBillingUsage(),
			}
		}
	}()

	for step := startStep; step <= totalMaxSteps; step++ {
		session.SetCurrentStep(step)
		select {
		case <-ctx.Done():
			if e.store != nil && sessionID != "" {
				shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				defer cancel()
				_ = e.store.UpdateSessionStatus(shutdownCtx, sessionID, persistence.StatusInterrupted, step-1, getBillingUsage(), "Process interrupted by signal or user")
			}
			return "", ctx.Err()
		default:
		}

		stepStartTime := time.Now()
		e.eventHandler(Event{Type: EventStepStart, Step: step, Timestamp: stepStartTime})

		var genResp *provider.GenerateResponse
		var completionErr error

		// Bounded reactive recovery completion loop
		for retry := 0; retry <= maxTurnRetries; retry++ {
			if e.conversationBoundaryHook != nil {
				if err := e.conversationBoundaryHook(ctx, sessionID, conversationTurnID, session, step); err != nil {
					return "", err
				}
			}
			genReq := provider.GenerateRequest{
				Model:           e.model,
				Messages:        session.CompileMessages(),
				Tools:           e.registry.Tools(),
				ReasoningEffort: e.reasoningEffort,
				IsLocal:         e.isLocal,
				LocalNumCtx:     session.MaxContextTokens(),
			}

			startCall := time.Now()
			var resp *provider.GenerateResponse
			var err error
			if streamingProvider, ok := e.provider.(provider.StreamModelProvider); ok {
				resp, err = streamingProvider.GenerateStream(ctx, genReq, func(delta string) error {
					if delta != "" {
						e.eventHandler(Event{Type: EventTextDelta, Step: step, Timestamp: time.Now(), Content: delta, Protocol: e.provider.Protocol()})
					}
					return ctx.Err()
				})
			} else {
				resp, err = e.provider.Generate(ctx, genReq)
			}
			callDuration := time.Since(startCall)

			if err == nil {
				if resp.Usage.PromptTokens > 0 {
					session.CalibrateUsage(resp.Usage.PromptTokens)
				}
				cumulativeUsage.PromptTokens += resp.Usage.PromptTokens
				cumulativeUsage.CompletionTokens += resp.Usage.CompletionTokens
				cumulativeUsage.ReasoningTokens += resp.Usage.ReasoningTokens
				cumulativeUsage.TotalTokens += resp.Usage.TotalTokens
				genResp = resp
				completionErr = nil
				break
			}

			completionErr = err
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				if e.store != nil && sessionID != "" {
					status := persistence.StatusInterrupted
					errMsg := "Process interrupted by signal or user"
					if errors.Is(ctx.Err(), context.DeadlineExceeded) {
						status = persistence.StatusFailed
						errMsg = "Execution timeout reached"
					}
					_ = e.store.UpdateSessionStatus(context.Background(), sessionID, status, step-1, getBillingUsage(), errMsg)
				}
				return "", err
			}
			targetTokens, matched := ParseContextLengthError(err)
			if !matched || retry == maxTurnRetries {
				e.eventHandler(Event{Type: EventError, Step: step, Duration: callDuration, Error: err})
				if e.store != nil && sessionID != "" {
					_ = e.store.UpdateSessionStatus(ctx, sessionID, persistence.StatusFailed, step-1, getBillingUsage(), err.Error())
				}
				return "", fmt.Errorf("step %d LLM call failed: %w", step, err)
			}

			e.eventHandler(Event{
				Type:     EventContextRetrying,
				Step:     step,
				Duration: callDuration,
				Content:  fmt.Sprintf("Context limit exceeded; ratcheting down and forcing compaction (retry %d/%d)", retry+1, maxTurnRetries),
				Error:    err,
			})

			if err := session.DownscaleWatermark(targetTokens); err != nil {
				if errors.Is(err, history.ErrContextExhausted) {
					e.eventHandler(Event{Type: EventContextExhausted, Step: step, Error: err})
					if e.store != nil && sessionID != "" {
						_ = e.store.UpdateSessionStatus(ctx, sessionID, persistence.StatusFailed, step-1, getBillingUsage(), err.Error())
					}
					return "", err
				}
				e.eventHandler(Event{Type: EventError, Step: step, Error: err})
				if e.store != nil && sessionID != "" {
					_ = e.store.UpdateSessionStatus(ctx, sessionID, persistence.StatusFailed, step-1, getBillingUsage(), err.Error())
				}
				return "", fmt.Errorf("downscale watermark failed: %w", err)
			}

			e.eventHandler(Event{Type: EventCompactionStart, Step: step})
			if err := session.ForceCompaction(ctx, e.compClient, e.model); err != nil {
				if errors.Is(err, history.ErrContextExhausted) {
					e.eventHandler(Event{Type: EventContextExhausted, Step: step, Error: err})
					if e.store != nil && sessionID != "" {
						_ = e.store.UpdateSessionStatus(ctx, sessionID, persistence.StatusFailed, step-1, getBillingUsage(), err.Error())
					}
					return "", err
				}
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return "", err
				}
				e.eventHandler(Event{Type: EventError, Step: step, Error: err})
				if e.store != nil && sessionID != "" {
					_ = e.store.UpdateSessionStatus(ctx, sessionID, persistence.StatusFailed, step-1, getBillingUsage(), err.Error())
				}
				return "", fmt.Errorf("force compaction failed: %w", err)
			}
			e.eventHandler(Event{Type: EventCompactionDone, Step: step})
		}

		if completionErr != nil {
			return "", completionErr
		}

		// Emit reasoning event if model produced chain-of-thought tokens
		if genResp.ReasoningContent != "" {
			e.eventHandler(Event{
				Type:      EventReasoning,
				Step:      step,
				Timestamp: time.Now(),
				Content:   genResp.ReasoningContent,
				Protocol:  genResp.Protocol,
				Usage:     genResp.Usage,
			})
		}

		// If no tools called, task has concluded
		if len(genResp.ToolCalls) == 0 {
			session.CommitSimpleAssistant(genResp.ToChatMessage())
			var turnRec *persistence.TurnRecord
			if e.store != nil && sessionID != "" {
				turnRec = &persistence.TurnRecord{
					SessionID:          sessionID,
					ConversationTurnID: conversationTurnID,
					StepNumber:         step,
					Role:               "assistant",
					Content:            genResp.Content,
					ReasoningContent:   genResp.ReasoningContent,
					TokenUsage: persistence.TokenUsage{
						PromptTokens:     genResp.Usage.PromptTokens,
						CompletionTokens: genResp.Usage.CompletionTokens,
						ReasoningTokens:  genResp.Usage.ReasoningTokens,
						TotalTokens:      genResp.Usage.TotalTokens,
					},
					DurationMs: time.Since(stepStartTime).Milliseconds(),
					CreatedAt:  time.Now().UTC(),
				}
			}
			textComplete := Event{
				Type:      EventTextComplete,
				Step:      step,
				Timestamp: time.Now(),
				Content:   genResp.Content,
				Protocol:  genResp.Protocol,
				Usage:     genResp.Usage,
			}
			finished := Event{
				Type:      EventFinished,
				Step:      step,
				Timestamp: time.Now(),
				Content:   genResp.Content,
				Protocol:  genResp.Protocol,
				Usage:     genResp.Usage,
			}
			var commit func() error
			if turnRec != nil {
				commit = func() error { return e.store.CommitTurn(ctx, turnRec, step, getBillingUsage()) }
			}
			if err := e.commitAndEmit(commit, textComplete, finished); err != nil {
				e.eventHandler(Event{Type: EventError, Step: step, Error: err})
				return "", fmt.Errorf("persist completed conversation step: %w", err)
			}
			if e.store != nil && sessionID != "" {
				_ = e.store.UpdateSessionStatus(ctx, sessionID, persistence.StatusCompleted, step, getBillingUsage(), "")
			}
			e.lastExecutionResult = &ExecutionResult{
				SessionID:     sessionID,
				Status:        persistence.StatusCompleted,
				Summary:       genResp.Content,
				ModifiedFiles: session.Tracker().ExportData().ModifiedFiles,
				TokenUsage:    getBillingUsage(),
			}
			return genResp.Content, nil
		}

		// Emit conversational text complete before tool calls if content is present
		if genResp.Content != "" {
			e.eventHandler(Event{
				Type:      EventTextComplete,
				Step:      step,
				Timestamp: time.Now(),
				Content:   genResp.Content,
				Protocol:  genResp.Protocol,
				Usage:     genResp.Usage,
			})
		}

		// Dispatch tool calls and accumulate atomically
		toolResults := make([]history.ToolResult, 0, len(genResp.ToolCalls))
		cumulativeBytes := 0
		maxTurnBytes := session.MaxTurnOutputBytes()
		var toolAuthorizationErr error

		for i := range genResp.ToolCalls {
			tc := &genResp.ToolCalls[i]
			args := json.RawMessage(tc.Arguments)
			var denied *ToolDeniedError
			toolExists := false
			if e.registry != nil {
				_, toolExists = e.registry.Get(tc.Name)
			}
			if e.toolInterceptor != nil && toolExists {
				allow, modifiedArgs, err := e.toolInterceptor(ctx, tc.Name, args)
				if err != nil && !errors.As(err, &denied) {
					toolAuthorizationErr = fmt.Errorf("authorize tool %s: %w", tc.Name, err)
					genResp.ToolCalls = genResp.ToolCalls[:i]
					break
				}
				if denied == nil && !allow {
					denied = &ToolDeniedError{Reason: "denied by user"}
				}
				if denied == nil && len(modifiedArgs) > 0 {
					if !json.Valid(modifiedArgs) {
						toolAuthorizationErr = fmt.Errorf("authorize tool %s: modified arguments are not valid JSON", tc.Name)
						genResp.ToolCalls = genResp.ToolCalls[:i]
						break
					}
					args = modifiedArgs
					tc.Arguments = string(modifiedArgs)
				}
			}
			e.eventHandler(Event{
				Type:       EventToolCall,
				Step:       step,
				Timestamp:  time.Now(),
				ToolCallID: tc.ID,
				ToolName:   tc.Name,
				ToolArgs:   string(args),
				Protocol:   genResp.Protocol,
			})

			// Telemetry hook: track file access for StateTracker
			if denied == nil && (tc.Name == "write_file" || tc.Name == "edit_file") {
				var args struct{ Path string }
				if json.Unmarshal([]byte(tc.Arguments), &args) == nil && args.Path != "" {
					session.RecordFileAccess(args.Path, "modified")
				}
			}

			startTool := time.Now()
			result := ""
			if denied != nil {
				reason := strings.TrimSpace(denied.Reason)
				if reason == "" {
					reason = "denied by user"
				}
				result = fmt.Sprintf("Tool %s execution denied by user: %s\nexit code: 1", tc.Name, reason)
			} else if e.registry == nil {
				result = "Tool error: tool registry is not configured"
			} else {
				result = e.registry.Dispatch(ctx, tc.Name, args)
			}
			toolDuration := time.Since(startTool)
			exitCode := parseToolExitCode(result)

			// Telemetry hook: track execution errors for StateTracker
			if exitCode != 0 {
				session.RecordError(fmt.Sprintf("%s: %s", tc.Name, result))
			}

			// Invariant refresh: reload system anchor if durable notes were updated
			if denied == nil && tc.Name == "save_memory" {
				if refreshedPrompt, err := e.memoryMgr.LoadSystemPrompt(); err == nil {
					session.ReloadSystemAnchor(refreshedPrompt)
				}
			}

			// Headroom protection: cap cumulative tool outputs within turn
			cumulativeBytes += len(result)
			if cumulativeBytes > maxTurnBytes {
				exitSuffix := ""
				for _, line := range strings.Split(result, "\n") {
					lower := strings.ToLower(line)
					if strings.Contains(lower, "exit code") || strings.Contains(lower, "exit status") {
						exitSuffix = " | " + strings.TrimSpace(line)
						break
					}
				}
				result = "[Output truncated: cumulative turn output limit reached]" + exitSuffix
			}

			e.eventHandler(Event{
				Type:         EventToolResult,
				Step:         step,
				Timestamp:    time.Now(),
				Duration:     toolDuration,
				ToolCallID:   tc.ID,
				ToolName:     tc.Name,
				ToolResult:   result,
				ToolExitCode: exitCode,
				Protocol:     genResp.Protocol,
			})

			toolResults = append(toolResults, history.ToolResult{
				ToolCallID: tc.ID,
				Name:       tc.Name,
				Content:    result,
			})
		}
		if toolAuthorizationErr != nil && len(toolResults) == 0 {
			return "", toolAuthorizationErr
		}

		// Atomic turn block commit
		session.CommitToolTurn(genResp.ToChatMessage(), toolResults)

		if e.store != nil && sessionID != "" {
			toolCalls := make([]persistence.ToolCallRecord, len(toolResults))
			for i, tr := range toolResults {
				argsStr := ""
				for _, tc := range genResp.ToolCalls {
					if tc.ID == tr.ToolCallID {
						argsStr = tc.Arguments
						break
					}
				}
				toolCalls[i] = persistence.ToolCallRecord{
					ID:       tr.ToolCallID,
					Name:     tr.Name,
					Args:     argsStr,
					Result:   tr.Content,
					ExitCode: parseToolExitCode(tr.Content),
				}
			}
			turnRec := &persistence.TurnRecord{
				SessionID:          sessionID,
				ConversationTurnID: conversationTurnID,
				StepNumber:         step,
				Role:               "assistant",
				Content:            genResp.Content,
				ReasoningContent:   genResp.ReasoningContent,
				ToolCalls:          toolCalls,
				TokenUsage: persistence.TokenUsage{
					PromptTokens:     genResp.Usage.PromptTokens,
					CompletionTokens: genResp.Usage.CompletionTokens,
					ReasoningTokens:  genResp.Usage.ReasoningTokens,
					TotalTokens:      genResp.Usage.TotalTokens,
				},
				DurationMs: time.Since(stepStartTime).Milliseconds(),
				CreatedAt:  time.Now().UTC(),
			}
			persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			commitErr := e.commitAndEmit(func() error { return e.store.CommitTurn(persistCtx, turnRec, step, getBillingUsage()) }, Event{
				Type: EventStepCommitted, Step: step, Timestamp: time.Now(), Content: genResp.Content, Protocol: genResp.Protocol,
			})
			cancel()
			if commitErr != nil {
				return "", fmt.Errorf("persist conversation tool step: %w", commitErr)
			}
		}

		if ctx.Err() != nil {
			if e.store != nil && sessionID != "" {
				shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
				_ = e.store.UpdateSessionStatus(shutdownCtx, sessionID, persistence.StatusInterrupted, step, getBillingUsage(), "Process interrupted by signal or user")
				cancel()
			}
			return "", ctx.Err()
		}
		if toolAuthorizationErr != nil {
			return "", toolAuthorizationErr
		}

		// Watermark check: execute staged micro-pruning or macro-distillation
		preTokens := session.CurrentTokens()
		if preTokens >= int(float64(session.MaxContextTokens())*0.70) {
			e.eventHandler(Event{Type: EventCompactionStart, Step: step, Timestamp: time.Now()})
		}
		if err := session.EnsureWatermarks(ctx, e.compClient, e.model); err != nil {
			if errors.Is(err, history.ErrContextExhausted) {
				e.eventHandler(Event{Type: EventContextExhausted, Step: step, Error: err})
				if e.store != nil && sessionID != "" {
					_ = e.store.UpdateSessionStatus(ctx, sessionID, persistence.StatusFailed, step, getBillingUsage(), err.Error())
				}
				return "", err
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return "", err
			}
			e.eventHandler(Event{Type: EventError, Step: step, Error: err})
			if e.store != nil && sessionID != "" {
				_ = e.store.UpdateSessionStatus(ctx, sessionID, persistence.StatusFailed, step, getBillingUsage(), err.Error())
			}
			return "", fmt.Errorf("context compaction failed: %w", err)
		}
		if session.CurrentTokens() < preTokens {
			e.eventHandler(Event{Type: EventCompactionDone, Step: step, Timestamp: time.Now()})
			if e.store != nil && sessionID != "" && session.EpisodicNarrative() != "" {
				narrative, trackerJSON, currentTokens := session.ExtractSnapshot()
				snapRec := &persistence.SnapshotRecord{
					SessionID:          sessionID,
					StepNumber:         session.LastCompactedStep(),
					DistilledNarrative: narrative,
					StateTrackerJSON:   trackerJSON,
					CurrentTokens:      currentTokens,
					CreatedAt:          time.Now().UTC(),
				}
				if err := e.commitAndEmit(func() error { return e.store.SaveSnapshot(ctx, snapRec) }, Event{
					Type: EventSnapshotCommitted, Step: snapRec.StepNumber, Timestamp: time.Now(),
				}); err != nil {
					return "", fmt.Errorf("persist conversation snapshot: %w", err)
				}
			}
		}
	}

	// Reserve-Step Budget Exhaustion Synthesis:
	// For subagents (depth > 0), if maximum step budget is exhausted without finishing,
	// make a forced non-tool attempt to synthesize findings so the parent receives analytical output.
	if e.depth > 0 && e.provider != nil {
		synthesisPrompt := "You have reached your maximum step budget. Do not call any tools. Provide a concise, complete synthesis of your findings, touched files, and any unresolved blockers based on your execution so far."
		msgs := append(session.CompileMessages(), openai.UserMessage(synthesisPrompt))

		genReq := provider.GenerateRequest{
			Model:           e.model,
			Messages:        msgs,
			Tools:           nil, // No tools allowed during emergency synthesis
			ReasoningEffort: e.reasoningEffort,
			IsLocal:         e.isLocal,
			LocalNumCtx:     session.MaxContextTokens(),
		}
		synthResp, err := e.provider.Generate(ctx, genReq)
		if err == nil && synthResp.Content != "" {
			cumulativeUsage.PromptTokens += synthResp.Usage.PromptTokens
			cumulativeUsage.CompletionTokens += synthResp.Usage.CompletionTokens
			cumulativeUsage.ReasoningTokens += synthResp.Usage.ReasoningTokens
			cumulativeUsage.TotalTokens += synthResp.Usage.TotalTokens

			synthStep := totalMaxSteps + 1
			if e.store != nil && sessionID != "" {
				turnRec := &persistence.TurnRecord{
					SessionID:          sessionID,
					ConversationTurnID: conversationTurnID,
					StepNumber:         synthStep,
					Role:               "assistant",
					Content:            synthResp.Content,
					TokenUsage: persistence.TokenUsage{
						PromptTokens:     synthResp.Usage.PromptTokens,
						CompletionTokens: synthResp.Usage.CompletionTokens,
						ReasoningTokens:  synthResp.Usage.ReasoningTokens,
						TotalTokens:      synthResp.Usage.TotalTokens,
					},
					DurationMs: 0,
					CreatedAt:  time.Now().UTC(),
				}
				if err := e.commitAndEmit(func() error { return e.store.CommitTurn(ctx, turnRec, synthStep, getBillingUsage()) }, Event{
					Type: EventStepCommitted, Step: synthStep, Content: synthResp.Content, Timestamp: time.Now(),
				}); err != nil {
					return "", fmt.Errorf("persist synthesized step: %w", err)
				}
				_ = e.store.UpdateSessionStatus(ctx, sessionID, persistence.StatusFailed, synthStep, getBillingUsage(), "maximum step budget reached (synthesized)")
			}
			e.lastExecutionResult = &ExecutionResult{
				SessionID:     sessionID,
				Status:        persistence.StatusFailed,
				Summary:       synthResp.Content,
				ModifiedFiles: session.Tracker().ExportData().ModifiedFiles,
				TokenUsage:    getBillingUsage(),
			}
			return fmt.Sprintf("[Subagent: budget_exhausted] %s", synthResp.Content), nil
		}
	}

	maxStepErr := fmt.Errorf("reached maximum step budget (%d) without finishing", totalMaxSteps)
	if e.store != nil && sessionID != "" {
		_ = e.store.UpdateSessionStatus(ctx, sessionID, persistence.StatusFailed, totalMaxSteps, getBillingUsage(), maxStepErr.Error())
	}
	e.lastExecutionResult = &ExecutionResult{
		SessionID:     sessionID,
		Status:        persistence.StatusFailed,
		Summary:       maxStepErr.Error(),
		ModifiedFiles: session.Tracker().ExportData().ModifiedFiles,
		TokenUsage:    getBillingUsage(),
	}
	e.eventHandler(Event{Type: EventError, Error: maxStepErr})
	return "", maxStepErr
}

func generateSessionID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return fmt.Sprintf("sess_%s_%x", time.Now().UTC().Format("20060102150405"), b)
}

func mapTurnRecordToBlock(tr *persistence.TurnRecord) history.TurnBlock {
	if tr.Role == "user" {
		b := history.NewUserBlock(tr.Content)
		b.StepNumber = tr.StepNumber
		return b
	}
	if len(tr.ToolCalls) == 0 {
		msg := openai.ChatCompletionMessage{Role: "assistant", Content: tr.Content}
		b := history.NewAssistantSimpleBlock(msg)
		b.StepNumber = tr.StepNumber
		return b
	}

	toolCalls := make([]openai.ChatCompletionMessageToolCall, len(tr.ToolCalls))
	results := make([]history.ToolResult, len(tr.ToolCalls))
	for i, tc := range tr.ToolCalls {
		toolCalls[i] = openai.ChatCompletionMessageToolCall{
			ID:   tc.ID,
			Type: "function",
			Function: openai.ChatCompletionMessageToolCallFunction{
				Name:      tc.Name,
				Arguments: tc.Args,
			},
		}
		results[i] = history.ToolResult{
			ToolCallID: tc.ID,
			Name:       tc.Name,
			Content:    tc.Result,
		}
	}
	msg := openai.ChatCompletionMessage{
		Role:      "assistant",
		Content:   tr.Content,
		ToolCalls: toolCalls,
	}
	b := history.NewAssistantToolCallBlock(msg, results)
	b.StepNumber = tr.StepNumber
	return b
}

// parseToolExitCode extracts a process exit code from tool output or defaults to 0 on success.
func parseToolExitCode(output string) int {
	// 1. Check for subprocess runner header "Exit Code: <N>"
	for _, marker := range []string{"Exit Code: ", "exit code: "} {
		if idx := strings.Index(output, marker); idx != -1 {
			var code int
			if _, err := fmt.Sscanf(output[idx+len(marker):], "%d", &code); err == nil {
				return code
			}
		}
	}

	// 2. Check for standard Go exec error "exit status <N>"
	if idx := strings.Index(output, "exit status "); idx != -1 {
		var code int
		if _, err := fmt.Sscanf(output[idx+len("exit status "):], "%d", &code); err == nil {
			return code
		}
		return 1
	}

	// 3. Tool execution error
	if strings.HasPrefix(output, "Tool error:") || strings.HasPrefix(output, "Error:") || strings.HasPrefix(output, "error:") {
		return -1
	}

	return 0
}
