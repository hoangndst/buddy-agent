package daemon

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hoangndst/buddy-agent/internal/agent"
	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
	"github.com/hoangndst/buddy-agent/internal/history"
	"github.com/hoangndst/buddy-agent/internal/persistence"
	"github.com/hoangndst/buddy-agent/internal/tools"
)

// ConversationWorker keeps a conversation runtime resident and runs its accepted
// prompts in FIFO order. The worker owns the engine cleanup closure.
type ConversationWorker struct {
	mu                  sync.Mutex
	eventMu             sync.Mutex
	broadcastMu         sync.Mutex
	conversationID      string
	engine              *agent.Engine
	runtime             *agent.ConversationRuntime
	store               persistence.Store
	ctx                 context.Context
	cancel              context.CancelFunc
	queue               []*persistence.SubmissionRecord
	pendingSteers       []*persistence.SubmissionRecord
	pending             map[string]struct{}
	attachedSteers      map[string][]string
	queueWake           chan struct{}
	done                chan struct{}
	runStarted          bool
	idleNotifyPending   bool
	active              bool
	currentTurn         string
	currentTurnCtx      context.Context
	currentCancel       context.CancelFunc
	pendingApproval     *pendingConversationApproval
	approvalTimeout     time.Duration
	detachedTimeout     time.Duration
	sessionToolAllow    map[string]struct{}
	sessionBashAllow    map[string]struct{}
	currentTerminalSent bool
	controller          ClientConn
	subscribers         map[ClientConn]struct{}
	replaying           map[ClientConn][]sequencedEvent
	ring                *EventRingBuffer
	pendingBroadcasts   map[uint64]sequencedEvent
	nextBroadcast       uint64
	workerEpoch         string
	lastDurableSequence uint64
	cleanup             func()
	onIdle              func()
	closed              bool
	evictionStarted     bool
	executionBlocked    string
	cleanupOnce         sync.Once
	workspaceDir        string
	outputStore         *outputStore
	doneOnce            sync.Once
}

type pendingConversationApproval struct {
	request      protocol.PermissionRequestNotification
	result       chan protocol.PermissionRespondParams
	disconnected chan struct{}
	detachedAt   time.Time
	deadline     time.Time
	responded    bool
}

func earlierTime(a, b time.Time) time.Time {
	if a.IsZero() || b.Before(a) {
		return b
	}
	return a
}

type sequencedEvent struct {
	sequence    uint64
	message     *protocol.JSONRPCMessage
	subscribers []ClientConn
}

// NewConversationWorker creates a resident worker for one persisted conversation.
func NewConversationWorker(parent context.Context, id string, engine *agent.Engine, runtime *agent.ConversationRuntime, controller ClientConn, cleanup func()) (*ConversationWorker, error) {
	if id == "" || engine == nil || runtime == nil || engine.PersistenceStore() == nil {
		return nil, errors.New("conversation worker requires an ID, engine, runtime, and persistence store")
	}
	ctx, cancel := context.WithCancel(parent)
	w := &ConversationWorker{
		conversationID: id, engine: engine, runtime: runtime, store: engine.PersistenceStore(),
		ctx: ctx, cancel: cancel, pending: make(map[string]struct{}), attachedSteers: make(map[string][]string), queueWake: make(chan struct{}, 1),
		done: make(chan struct{}), controller: controller, subscribers: make(map[ClientConn]struct{}), replaying: make(map[ClientConn][]sequencedEvent),
		ring: NewEventRingBuffer(DefaultRingBufferCapacity), pendingBroadcasts: make(map[uint64]sequencedEvent), nextBroadcast: 1, workerEpoch: generateWorkerID("epoch"), cleanup: cleanup,
		approvalTimeout: 15 * time.Minute, detachedTimeout: 5 * time.Minute,
		sessionToolAllow: make(map[string]struct{}), sessionBashAllow: make(map[string]struct{}),
		workspaceDir: engine.WorkspaceDir(), outputStore: newOutputStore(),
	}
	if controller != nil {
		w.subscribers[controller] = struct{}{}
	}
	engine.SetConversationCommitHook(w.commitWithEvents)
	engine.SetConversationBoundaryHook(w.beforeModelRequest)
	engine.SetToolInterceptor(w.interceptTool)
	runtime.SetApplySubmissionHook(w.applySubmission)
	runtime.SetTurnOutcomeHook(w.finishTurn)
	return w, nil
}

// ConversationID returns the persisted conversation identifier owned by the worker.
func (w *ConversationWorker) ConversationID() string { return w.conversationID }

// Submission loads one accepted submission for this conversation.
func (w *ConversationWorker) Submission(ctx context.Context, id string) (*persistence.SubmissionRecord, error) {
	return w.store.GetSubmission(ctx, w.conversationID, id)
}

// SessionRecord loads the persistent session row that backs this conversation.
func (w *ConversationWorker) SessionRecord(ctx context.Context) (*persistence.SessionRecord, error) {
	return w.store.GetSession(ctx, w.conversationID)
}

// State reports the conversation's externally visible runtime state.
func (w *ConversationWorker) State() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pendingApproval != nil {
		return "awaiting_approval"
	}
	if w.active && !w.currentTerminalSent {
		return "running"
	}
	return "idle"
}

// SetOnIdle registers a callback invoked when the worker becomes idle.
func (w *ConversationWorker) SetOnIdle(fn func()) { w.mu.Lock(); w.onIdle = fn; w.mu.Unlock() }

// Start starts processing the persisted initial submission.
func (w *ConversationWorker) Start(first *persistence.SubmissionRecord) error {
	if first == nil || first.SubmissionID == "" || first.TurnID == nil || *first.TurnID == "" {
		return errors.New("conversation worker requires its persisted first submission and turn ID")
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return errors.New("conversation worker is closed")
	}
	w.queue = append(w.queue, first)
	w.pending[first.SubmissionID] = struct{}{}
	w.runStarted = true
	w.mu.Unlock()
	go w.run()
	w.signalQueue()
	return nil
}

// StartIdle restores a resident conversation with no queued submissions.
func (w *ConversationWorker) StartIdle() error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return errors.New("conversation worker is closed")
	}
	w.runStarted = true
	w.idleNotifyPending = true
	w.mu.Unlock()
	go w.run()
	return nil
}

// StartRestored starts the worker with its durable queued submissions restored.
func (w *ConversationWorker) StartRestored(submissions []*persistence.SubmissionRecord) error {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return errors.New("conversation worker is closed")
	}
	for _, submission := range submissions {
		if submission.Status != persistence.SubmissionQueued && submission.Status != persistence.SubmissionSteerPending {
			continue
		}
		if _, exists := w.pending[submission.SubmissionID]; exists {
			continue
		}
		w.pending[submission.SubmissionID] = struct{}{}
		if submission.Mode == persistence.SubmissionSteer {
			w.queue = append([]*persistence.SubmissionRecord{submission}, w.queue...)
		} else {
			w.queue = append(w.queue, submission)
		}
	}
	// Preserve FIFO within each mode while restoring steering ahead of queued work.
	sort.SliceStable(w.queue, func(i, j int) bool {
		if w.queue[i].Mode != w.queue[j].Mode {
			return w.queue[i].Mode == persistence.SubmissionSteer
		}
		return w.queue[i].Sequence < w.queue[j].Sequence
	})
	w.runStarted = true
	w.idleNotifyPending = len(w.queue) == 0
	w.mu.Unlock()
	go w.run()
	w.signalQueue()
	return nil
}

func (w *ConversationWorker) signalQueue() {
	select {
	case w.queueWake <- struct{}{}:
	default:
	}
}

func (w *ConversationWorker) run() {
	defer func() {
		w.mu.Lock()
		w.active = false
		w.currentTurn = ""
		w.currentTurnCtx = nil
		w.currentCancel = nil
		w.mu.Unlock()
		w.cleanupOnce.Do(func() {
			w.outputStore.clear()
			if w.cleanup != nil {
				w.cleanup()
			}
		})
		w.doneOnce.Do(func() { close(w.done) })
	}()
	for {
		w.mu.Lock()
		if w.closed || w.ctx.Err() != nil {
			w.mu.Unlock()
			return
		}
		if w.executionBlocked != "" {
			var idle func()
			if len(w.queue) == 0 && w.idleNotifyPending {
				idle = w.onIdle
				w.idleNotifyPending = false
			}
			w.active = false
			w.mu.Unlock()
			if idle != nil {
				go idle()
			}
			select {
			case <-w.queueWake:
				continue
			case <-w.ctx.Done():
				return
			}
		}
		if len(w.queue) == 0 {
			notifyIdle := w.active || w.idleNotifyPending
			idle := w.onIdle
			w.idleNotifyPending = false
			w.active = false
			w.mu.Unlock()
			if notifyIdle && idle != nil {
				go idle()
			}
			select {
			case <-w.queueWake:
				continue
			case <-w.ctx.Done():
				return
			}
		}
		sub := w.queue[0]
		w.queue = w.queue[1:]
		delete(w.pending, sub.SubmissionID)
		turnID := ""
		if sub.TurnID != nil {
			turnID = *sub.TurnID
		}
		if turnID == "" {
			turnID = generateWorkerID("turn")
		}
		turnCtx, cancelTurn := context.WithCancel(w.ctx)
		w.active, w.currentTurn, w.currentCancel = true, turnID, cancelTurn
		w.currentTurnCtx = turnCtx
		w.currentTerminalSent = false
		w.mu.Unlock()

		answer, runErr := w.runtime.RunTurn(turnCtx, sub.SubmissionID, turnID)
		cancelTurn()
		w.mu.Lock()
		hasTerminal := w.currentTerminalSent
		w.mu.Unlock()
		if !hasTerminal {
			final := agent.Event{Type: agent.EventFinished, Content: answer, Timestamp: time.Now()}
			if runErr != nil {
				final.Type = agent.EventError
				final.Content = runErr.Error()
			}
			w.emit(final, true)
		}
		w.mu.Lock()
		w.currentTurn = ""
		w.currentTurnCtx = nil
		w.currentCancel = nil
		w.mu.Unlock()
	}
}

// Submit persists the prompt before queueing it. Steers arriving during a turn
// are staged for the next model-input boundary; when idle they run ahead of queued work.
func (w *ConversationWorker) Submit(ctx context.Context, submission *persistence.SubmissionRecord) (*persistence.SubmissionRecord, bool, error) {
	if submission == nil || submission.ConversationID != w.conversationID {
		return nil, false, errors.New("submission targets a different conversation")
	}
	if submission.Mode == "" {
		submission.Mode = persistence.SubmissionQueue
	}
	if submission.Mode != persistence.SubmissionQueue && submission.Mode != persistence.SubmissionSteer {
		return nil, false, fmt.Errorf("unsupported submission mode %q", submission.Mode)
	}
	rawPrompt := submission.Prompt
	w.eventMu.Lock()
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		w.eventMu.Unlock()
		return nil, false, errors.New("conversation worker was evicted")
	}
	if w.executionBlocked != "" {
		reason := w.executionBlocked
		w.mu.Unlock()
		w.eventMu.Unlock()
		return nil, false, fmt.Errorf("conversation execution is blocked: %s", reason)
	}
	_, lookupErr := w.store.GetSubmission(ctx, w.conversationID, submission.SubmissionID)
	existing := lookupErr == nil
	if lookupErr != nil && !errors.Is(lookupErr, persistence.ErrSubmissionNotFound) {
		w.mu.Unlock()
		w.eventMu.Unlock()
		return nil, false, lookupErr
	}
	submission.TurnID = nil
	accepted, err := w.store.AdmitSubmission(ctx, submission)
	if err != nil {
		w.mu.Unlock()
		w.eventMu.Unlock()
		return nil, false, err
	}
	_, pending := w.pending[accepted.SubmissionID]
	if !pending && (accepted.Status == persistence.SubmissionQueued || accepted.Status == persistence.SubmissionSteerPending) {
		w.pending[accepted.SubmissionID] = struct{}{}
		w.runtime.SetPromptOverride(accepted.SubmissionID, rawPrompt)
		if accepted.Mode == persistence.SubmissionSteer && w.active && w.currentTurn != "" && !w.currentTerminalSent {
			w.pendingSteers = append(w.pendingSteers, accepted)
		} else if accepted.Mode == persistence.SubmissionSteer {
			w.queue = append([]*persistence.SubmissionRecord{accepted}, w.queue...)
		} else {
			w.queue = append(w.queue, accepted)
		}
		if !w.active || w.currentTerminalSent || accepted.Mode == persistence.SubmissionQueue {
			sort.SliceStable(w.queue, func(i, j int) bool {
				if w.queue[i].Mode != w.queue[j].Mode {
					return w.queue[i].Mode == persistence.SubmissionSteer
				}
				return w.queue[i].Sequence < w.queue[j].Sequence
			})
			w.signalQueue()
		}
	}
	w.mu.Unlock()
	var staged []sequencedEvent
	if !existing && !pending {
		evt := agent.Event{Type: agent.EventSubmissionAccepted, Step: int(accepted.Sequence), Content: accepted.Prompt, HistoryRevision: accepted.HistoryRevision, Timestamp: time.Now()}
		if item, sequenceErr := w.sequenceLocked(evt, false); sequenceErr == nil {
			staged = append(staged, item)
			w.lastDurableSequence = item.sequence
		}
	}
	w.eventMu.Unlock()
	w.broadcast(staged)
	return accepted, existing || pending, nil
}

// beforeModelRequest attaches staged steer prompts after the previous durable
// step has committed and before the next provider request compiles its messages.
func (w *ConversationWorker) beforeModelRequest(ctx context.Context, conversationID, turnID string, historySession *history.Session, step int) error {
	if conversationID != w.conversationID || turnID == "" || historySession == nil {
		return errors.New("steering boundary does not match active conversation turn")
	}
	w.eventMu.Lock()
	w.mu.Lock()
	if !w.active || w.currentTurn != turnID {
		w.mu.Unlock()
		w.eventMu.Unlock()
		return errors.New("steering boundary is no longer active")
	}
	w.mu.Unlock()
	if len(w.pendingSteers) == 0 {
		w.eventMu.Unlock()
		return nil
	}
	pending := append([]*persistence.SubmissionRecord(nil), w.pendingSteers...)
	w.pendingSteers = nil
	var staged []sequencedEvent
	var boundaryErr error
	for i, steer := range pending {
		prompt, ok := w.runtime.PromptOverride(steer.SubmissionID)
		if !ok {
			prompt = steer.Prompt
		}
		applied, err := w.store.ApplySubmission(ctx, w.conversationID, steer.SubmissionID, turnID, max(step-1, 0))
		if err != nil {
			w.pendingSteers = append(pending[i:], w.pendingSteers...)
			boundaryErr = err
			break
		}
		if err := w.runtime.ApplySteerPrompt(steer.SubmissionID, prompt); err != nil {
			// The durable attachment is recoverable from the applied step on resume.
			boundaryErr = err
			break
		}
		w.attachedSteers[turnID] = append(w.attachedSteers[turnID], steer.SubmissionID)
		evt := agent.Event{Type: agent.EventSubmissionApplied, Step: max(step-1, 0), Content: applied.Prompt, HistoryRevision: applied.AppliedHistoryRevision, Timestamp: time.Now()}
		item, sequenceErr := w.sequenceLocked(evt, false)
		if sequenceErr != nil {
			boundaryErr = sequenceErr
			break
		}
		w.lastDurableSequence = item.sequence
		staged = append(staged, item)
	}
	w.eventMu.Unlock()
	w.broadcast(staged)
	return boundaryErr
}

// IsActive reports whether the worker is executing a conversation turn.
func (w *ConversationWorker) IsActive() bool { w.mu.Lock(); defer w.mu.Unlock(); return w.active }

// SetExecutionBlocked pauses restored work until missing ephemeral settings are supplied.
func (w *ConversationWorker) SetExecutionBlocked(reason string) {
	w.mu.Lock()
	w.executionBlocked = reason
	w.mu.Unlock()
	w.signalQueue()
}

// ExecutionBlocked reports why a restored worker cannot execute queued prompts.
func (w *ConversationWorker) ExecutionBlocked() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.executionBlocked
}

// ReplaceBlockedExecution installs a newly configured engine/runtime on a blocked idle worker.
func (w *ConversationWorker) ReplaceBlockedExecution(engine *agent.Engine, runtime *agent.ConversationRuntime, cleanup func()) error {
	if engine == nil || runtime == nil {
		return errors.New("replacement conversation execution requires an engine and runtime")
	}
	w.mu.Lock()
	if w.executionBlocked == "" || w.active || w.closed {
		w.mu.Unlock()
		return errors.New("conversation worker is not blocked and idle")
	}
	previousCleanup := w.cleanup
	w.engine, w.runtime, w.cleanup = engine, runtime, cleanup
	w.executionBlocked = ""
	engine.SetConversationCommitHook(w.commitWithEvents)
	engine.SetConversationBoundaryHook(w.beforeModelRequest)
	runtime.SetApplySubmissionHook(w.applySubmission)
	runtime.SetTurnOutcomeHook(w.finishTurn)
	w.mu.Unlock()
	if previousCleanup != nil {
		previousCleanup()
	}
	w.signalQueue()
	return nil
}

// IsController reports whether c currently controls this conversation.
func (w *ConversationWorker) IsController(c ClientConn) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.controller == c
}

// Attach registers a client, optionally transfers control, and replays retained events.
func (w *ConversationWorker) Attach(c ClientConn, epoch string, after uint64, takeControl bool) (string, bool, error) {
	w.eventMu.Lock()
	w.mu.Lock()
	role := "observer"
	if takeControl || w.controller == nil {
		w.controller = c
		role = "controller"
	}
	gap := epoch != "" && epoch != w.workerEpoch
	var replay []sequencedEvent
	if !gap {
		for _, item := range w.ring.ReplayAfter(after) {
			msg, err := createConversationEventNotification(w.conversationID, w.workerEpoch, item.EventID, item.TurnID, item.Event, item.Terminal)
			if err == nil {
				replay = append(replay, sequencedEvent{sequence: item.EventID, message: msg})
			}
		}
	}
	w.subscribers[c] = struct{}{}
	w.replaying[c] = nil
	w.mu.Unlock()
	w.eventMu.Unlock()
	for _, event := range replay {
		if err := c.SendWithTimeout(event.message, time.Second); err != nil {
			w.Detach(c)
			return role, true, err
		}
	}
	for {
		w.eventMu.Lock()
		buffered := w.replaying[c]
		w.replaying[c] = nil
		if len(buffered) == 0 {
			delete(w.replaying, c)
			w.eventMu.Unlock()
			break
		}
		w.eventMu.Unlock()
		for _, event := range buffered {
			if err := c.SendWithTimeout(event.message, time.Second); err != nil {
				w.Detach(c)
				return role, true, err
			}
		}
	}
	for _, pending := range w.pendingPermissionForController(c) {
		if err := sendPermissionRequest(c, pending); err != nil {
			w.Detach(c)
			return role, true, err
		}
	}
	return role, gap, nil
}

// Sync captures the durable transcript revision and event watermark for a client.
func (w *ConversationWorker) Sync(ctx context.Context, params protocol.ConversationSyncParams, conn ClientConn) (*protocol.ConversationSyncResult, string, error) {
	w.eventMu.Lock()
	w.mu.Lock()
	role := "observer"
	if conn != nil {
		if w.controller == nil {
			w.controller = conn
		}
		if w.controller == conn {
			role = "controller"
		}
		w.subscribers[conn] = struct{}{}
	}
	active := w.active && !w.currentTerminalSent
	epoch := w.workerEpoch
	turnID := w.currentTurn
	if !active {
		turnID = ""
	}
	lastDurableSequence := w.lastDurableSequence
	executionBlocked := w.executionBlocked
	var pendingApprovals []protocol.PermissionRequestNotification
	if conn != nil && w.controller == conn && w.pendingApproval != nil && !w.pendingApproval.responded {
		pendingApprovals = []protocol.PermissionRequestNotification{boundedPermissionRequest(w.pendingApproval.request)}
	}
	w.mu.Unlock()
	revision := params.HistoryRevision
	watermark := w.ring.Watermark()
	if revision == 0 {
		var err error
		revision, err = w.store.GetHistoryRevision(ctx, w.conversationID)
		if err != nil {
			w.eventMu.Unlock()
			return nil, "", err
		}
	}
	// Event sequence, subscriber registration, and history revision are captured
	// while all revision-advancing worker writes are excluded. Page reads happen
	// after releasing this ordering lock and remain pinned to the captured revision.
	w.eventMu.Unlock()
	submissions, err := w.store.ListSubmissionsThroughRevision(ctx, w.conversationID, revision)
	if err != nil {
		return nil, "", err
	}
	turns, err := w.store.GetTurnsThroughRevision(ctx, w.conversationID, revision)
	if err != nil {
		return nil, "", err
	}
	conversationTurns, err := w.store.ListConversationTurns(ctx, w.conversationID)
	if err != nil {
		return nil, "", err
	}
	turnStates := make([]protocol.ConversationTurnState, 0, len(conversationTurns))
	for _, turn := range conversationTurns {
		if turn.HistoryRevision > revision {
			continue
		}
		state := protocol.ConversationTurnState{TurnID: turn.TurnID, SubmissionID: turn.SubmissionID, Status: string(turn.Status), StartedAt: turn.StartedAt.UTC().Format(time.RFC3339Nano)}
		if turn.FinishedAt != nil {
			state.FinishedAt = turn.FinishedAt.UTC().Format(time.RFC3339Nano)
		}
		if turn.ErrorMessage != nil {
			state.Error = *turn.ErrorMessage
		}
		turnStates = append(turnStates, state)
	}
	type orderedEntry struct {
		revision int64
		sequence int64
		step     int
		entry    protocol.ConversationTranscriptEntry
	}
	entries := make([]orderedEntry, 0, len(submissions)+len(turns))
	for _, submission := range submissions {
		turn := ""
		if submission.TurnID != nil {
			turn = *submission.TurnID
		}
		entries = append(entries, orderedEntry{revision: submission.HistoryRevision, sequence: submission.Sequence, entry: protocol.ConversationTranscriptEntry{
			SubmissionID: submission.SubmissionID, TurnID: turn, Role: "user", Mode: string(submission.Mode), Status: string(submission.Status), Content: submission.Prompt, CreatedAt: submission.CreatedAt.UTC().Format(time.RFC3339Nano),
		}})
	}
	for _, turn := range turns {
		activities, omitted := conversationActivities(turn.ToolCalls)
		entries = append(entries, orderedEntry{revision: turn.HistoryRevision, step: turn.StepNumber, entry: protocol.ConversationTranscriptEntry{
			TurnID: turn.ConversationTurnID, StepNumber: turn.StepNumber, Role: turn.Role, Content: turn.Content, CreatedAt: turn.CreatedAt.UTC().Format(time.RFC3339Nano),
			Activities: activities, ActivitiesOmitted: omitted,
		}})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].revision != entries[j].revision {
			return entries[i].revision < entries[j].revision
		}
		if entries[i].sequence != entries[j].sequence {
			return entries[i].sequence < entries[j].sequence
		}
		if entries[i].step != entries[j].step {
			return entries[i].step < entries[j].step
		}
		return entries[i].entry.Role == "user" && entries[j].entry.Role != "user"
	})
	offset := 0
	if params.PageToken != "" {
		raw, decodeErr := base64.RawURLEncoding.DecodeString(params.PageToken)
		if decodeErr != nil {
			return nil, "", errors.New("invalid transcript page token")
		}
		parts := strings.Split(string(raw), ":")
		if len(parts) != 2 {
			return nil, "", errors.New("invalid transcript page token")
		}
		tokenRevision, e1 := strconv.ParseInt(parts[0], 10, 64)
		parsedOffset, e2 := strconv.Atoi(parts[1])
		if e1 != nil || e2 != nil || tokenRevision != revision || parsedOffset < 0 || parsedOffset > len(entries) {
			return nil, "", errors.New("transcript page token does not match pinned revision")
		}
		offset = parsedOffset
	}
	pageSize := params.PageSize
	if pageSize <= 0 {
		pageSize = 50
	}
	if pageSize > 100 {
		pageSize = 100
	}
	state := "idle"
	if active {
		state = "running"
	}
	if len(pendingApprovals) > 0 {
		state = "awaiting_approval"
	}
	earliest := w.ring.EarliestID()
	incomplete := (params.WorkerEpoch != "" && params.WorkerEpoch != epoch) || params.AfterSequence+1 < earliest || watermark > lastDurableSequence
	if params.WorkerEpoch == "" && watermark > 0 && earliest > 1 {
		incomplete = true
	}
	if active && turnID != "" && len(pendingApprovals) == 0 {
		state = "running"
	}
	makeResult := func(transcript []protocol.ConversationTranscriptEntry, end int) *protocol.ConversationSyncResult {
		next := ""
		if end < len(entries) {
			next = base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d:%d", revision, end)))
		}
		return &protocol.ConversationSyncResult{
			ConversationID: w.conversationID, Role: role, WorkerEpoch: epoch, EventWatermark: watermark,
			HistoryRevision: revision, State: state, ActiveTurnID: turnID, Transcript: transcript, Turns: turnStates, NextPageToken: next,
			PendingApprovals: pendingApprovals, OutputIncomplete: incomplete, ExecutionBlocked: executionBlocked,
		}
	}
	transcript := make([]protocol.ConversationTranscriptEntry, 0, min(pageSize, len(entries)-offset))
	end := offset
	for end < len(entries) && len(transcript) < pageSize {
		candidate := append(transcript, entries[end].entry)
		if conversationSyncFrameSize(makeResult(candidate, end+1)) <= protocol.DefaultMaxMessageSize {
			transcript = candidate
			end++
			continue
		}
		if len(transcript) > 0 {
			break
		}
		bounded, ok := boundTranscriptEntry(entries[end].entry, func(candidate protocol.ConversationTranscriptEntry) bool {
			return conversationSyncFrameSize(makeResult([]protocol.ConversationTranscriptEntry{candidate}, end+1)) <= protocol.DefaultMaxMessageSize
		})
		if !ok {
			return nil, "", errors.New("conversation metadata exceeds maximum JSON-RPC frame size")
		}
		transcript = append(transcript, bounded)
		end++
		break
	}
	result := makeResult(transcript, end)
	if conversationSyncFrameSize(result) > protocol.DefaultMaxMessageSize {
		return nil, "", errors.New("conversation sync metadata exceeds maximum JSON-RPC frame size")
	}
	return result, role, nil
}

const (
	conversationActivityLimit       = 32
	conversationActivityIDSize      = 128
	conversationActivityToolSize    = 64
	conversationActivitySummarySize = 160
	conversationActivityOutputSize  = 1024
)

func conversationActivities(calls []persistence.ToolCallRecord) ([]protocol.ConversationActivity, int) {
	activities := make([]protocol.ConversationActivity, 0, min(len(calls), conversationActivityLimit))
	for _, call := range calls[:min(len(calls), conversationActivityLimit)] {
		output := persistence.SanitizeToolOutput(call.Result)
		truncated := len(output) > conversationActivityOutputSize
		if truncated {
			output = output[:utf8SafePrefix(output, conversationActivityOutputSize)] + " …[preview truncated]"
		}
		id := call.ID
		if len(id) > conversationActivityIDSize {
			id = ""
		}
		activities = append(activities, protocol.ConversationActivity{
			ID: id, Tool: boundedActivityText(call.Name, conversationActivityToolSize), Summary: toolActivitySummary(call.Name, call.Args),
			Output: output, OutputTruncated: truncated, ExitCode: call.ExitCode,
		})
	}
	return activities, max(0, len(calls)-len(activities))
}

func boundedActivityText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:utf8SafePrefix(value, limit)]
}

func toolActivitySummary(tool, rawArgs string) string {
	var args map[string]any
	if json.Unmarshal([]byte(persistence.RedactJSON(rawArgs)), &args) != nil {
		return ""
	}
	var summary string
	if tool == "bash" {
		summary, _ = args["command"].(string)
		if line, _, found := strings.Cut(summary, "\n"); found && line != "" {
			summary = line + " …"
		}
	} else {
		for _, key := range []string{"path", "file_path", "directory", "query"} {
			if value, ok := args[key].(string); ok && value != "" {
				summary = value
				break
			}
		}
	}
	summary = strings.TrimSpace(persistence.RedactSecrets(summary))
	if len(summary) > conversationActivitySummarySize {
		summary = summary[:utf8SafePrefix(summary, conversationActivitySummarySize)] + "…"
	}
	return summary
}

func conversationSyncFrameSize(result *protocol.ConversationSyncResult) int {
	msg, err := protocol.NewResponse(strings.Repeat("x", 128), result)
	if err != nil {
		return protocol.DefaultMaxMessageSize + 1
	}
	encoded, err := protocol.EncodeMessage(msg)
	if err != nil {
		return protocol.DefaultMaxMessageSize + 1
	}
	return len(encoded)
}

const transcriptTruncationMarker = "…[content truncated to fit sync frame]"

func boundTranscriptEntry(entry protocol.ConversationTranscriptEntry, fits func(protocol.ConversationTranscriptEntry) bool) (protocol.ConversationTranscriptEntry, bool) {
	entry.ContentTruncated = true
	original := entry.Content
	low, high := 0, len(original)
	var best protocol.ConversationTranscriptEntry
	found := false
	for low <= high {
		mid := low + (high-low)/2
		prefixLength := utf8SafePrefix(original, mid)
		candidate := entry
		candidate.Content = original[:prefixLength] + transcriptTruncationMarker
		if fits(candidate) {
			best, found = candidate, true
			low = mid + 1
		} else {
			high = mid - 1
		}
	}
	return best, found
}

// Detach removes a client connection and releases its controller role if held.
func (w *ConversationWorker) Detach(c ClientConn) {
	w.eventMu.Lock()
	delete(w.replaying, c)
	w.mu.Lock()
	delete(w.subscribers, c)
	if w.controller == c {
		w.controller = nil
		if pending := w.pendingApproval; pending != nil && pending.detachedAt.IsZero() {
			pending.detachedAt = time.Now()
			pending.deadline = earlierTime(pending.deadline, time.Now().Add(w.detachedTimeout))
			select {
			case pending.disconnected <- struct{}{}:
			default:
			}
		}
	}
	w.mu.Unlock()
	w.eventMu.Unlock()
}

// Interrupt cancels the active turn only when turnID matches it.
func (w *ConversationWorker) Interrupt(turnID string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.active || w.currentTurn != turnID {
		return errors.New("turn is not active")
	}
	if w.currentCancel == nil {
		return errors.New("turn cancellation is not available")
	}
	w.currentCancel()
	return nil
}

// RequestPermission waits for a decision from the current conversation controller.
// The request remains worker-local so reconnect can replay it, while the active
// turn keeps the worker pinned against idle eviction.
func (w *ConversationWorker) RequestPermission(tool string, args map[string]any) (bool, error) {
	response, err := w.requestPermission(w.currentTurnContext(), tool, args)
	if err != nil {
		return false, err
	}
	return response.Decision == protocol.DecisionAllowOnce || response.Decision == protocol.DecisionAllowSession, nil
}

func (w *ConversationWorker) currentTurnContext() context.Context {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.currentTurnCtx
}

func (w *ConversationWorker) interceptTool(ctx context.Context, tool string, raw json.RawMessage) (bool, json.RawMessage, error) {
	var args map[string]any
	var canonicalArgs json.RawMessage
	if tool == "bash" {
		command, err := tools.ParseBashCommand(raw)
		if err != nil {
			return false, nil, fmt.Errorf("decode bash tool arguments for approval: %w", err)
		}
		args = map[string]any{"command": command}
		canonicalArgs, err = json.Marshal(args)
		if err != nil {
			return false, nil, fmt.Errorf("encode bash tool arguments for approval: %w", err)
		}
	} else {
		args = make(map[string]any)
	}
	if tool != "bash" && len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return false, nil, fmt.Errorf("decode %s arguments for approval: %w", tool, err)
		}
	}
	response, err := w.requestPermission(ctx, tool, args)
	if err != nil {
		return false, nil, err
	}
	if response.Decision == protocol.DecisionDeny {
		return false, nil, &agent.ToolDeniedError{Reason: response.DenialReason}
	}
	if response.ModifiedArguments == nil {
		if tool == "bash" {
			return true, canonicalArgs, nil
		}
		return true, nil, nil
	}
	modified, err := json.Marshal(*response.ModifiedArguments)
	if err != nil {
		return false, nil, fmt.Errorf("encode modified %s arguments: %w", tool, err)
	}
	if tool == "bash" {
		command, err := tools.ParseBashCommand(modified)
		if err != nil {
			return false, nil, fmt.Errorf("decode modified bash arguments: %w", err)
		}
		canonical, err := json.Marshal(map[string]string{"command": command})
		if err != nil {
			return false, nil, fmt.Errorf("encode modified bash arguments: %w", err)
		}
		return true, canonical, nil
	}
	return true, modified, nil
}

func (w *ConversationWorker) requestPermission(ctx context.Context, tool string, args map[string]any) (protocol.PermissionRespondParams, error) {
	w.mu.Lock()
	if !w.active || w.currentTurn == "" || w.currentTurnCtx == nil || ctx == nil {
		w.mu.Unlock()
		return protocol.PermissionRespondParams{}, errors.New("permission request requires an active conversation turn")
	}
	if ctx.Err() != nil {
		w.mu.Unlock()
		return protocol.PermissionRespondParams{}, ctx.Err()
	}
	if w.sessionToolIsAllowedLocked(tool, args) {
		w.mu.Unlock()
		return protocol.PermissionRespondParams{Decision: protocol.DecisionAllowOnce}, nil
	}
	if w.pendingApproval != nil {
		w.mu.Unlock()
		return protocol.PermissionRespondParams{}, errors.New("conversation already has a pending permission request")
	}
	req := protocol.PermissionRequestNotification{
		RequestID: generateWorkerID("permission"), SessionID: w.conversationID, ConversationID: w.conversationID,
		TurnID: w.currentTurn, Tool: tool, Arguments: args,
	}
	now := time.Now()
	pending := &pendingConversationApproval{
		request: req, result: make(chan protocol.PermissionRespondParams, 1), disconnected: make(chan struct{}, 1),
		deadline: now.Add(w.approvalTimeout),
	}
	controller := w.controller
	if controller == nil {
		pending.detachedAt = now
		pending.deadline = earlierTime(pending.deadline, now.Add(w.detachedTimeout))
	}
	w.pendingApproval = pending
	deadline := pending.deadline
	w.mu.Unlock()

	if controller != nil {
		if err := sendPermissionRequest(controller, req); err != nil {
			w.Detach(controller)
		}
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case response := <-pending.result:
		w.mu.Lock()
		if err := ctx.Err(); err != nil {
			if w.pendingApproval == pending {
				w.pendingApproval = nil
			}
			w.mu.Unlock()
			return protocol.PermissionRespondParams{}, err
		}
		if w.pendingApproval == pending {
			w.pendingApproval = nil
			w.cacheSessionApprovalLocked(tool, args, response)
			w.mu.Unlock()
			return response, nil
		}
		w.mu.Unlock()
		return protocol.PermissionRespondParams{}, errors.New("permission request is no longer pending")
	case <-ctx.Done():
		w.clearPendingApproval(pending)
		return protocol.PermissionRespondParams{}, ctx.Err()
	case <-pending.disconnected:
		w.mu.Lock()
		deadline := pending.deadline
		w.mu.Unlock()
		resetTimer(timer, time.Until(deadline))
		select {
		case response := <-pending.result:
			w.mu.Lock()
			if err := ctx.Err(); err != nil {
				if w.pendingApproval == pending {
					w.pendingApproval = nil
				}
				w.mu.Unlock()
				return protocol.PermissionRespondParams{}, err
			}
			if w.pendingApproval == pending {
				w.pendingApproval = nil
				w.cacheSessionApprovalLocked(tool, args, response)
				w.mu.Unlock()
				return response, nil
			}
			w.mu.Unlock()
			return protocol.PermissionRespondParams{}, errors.New("permission request is no longer pending")
		case <-ctx.Done():
			w.clearPendingApproval(pending)
			return protocol.PermissionRespondParams{}, ctx.Err()
		case <-timer.C:
			return w.expireApproval(ctx, pending)
		}
	case <-timer.C:
		return w.expireApproval(ctx, pending)
	}
}

func (w *ConversationWorker) clearPendingApproval(pending *pendingConversationApproval) {
	w.mu.Lock()
	if w.pendingApproval == pending {
		w.pendingApproval = nil
	}
	w.mu.Unlock()
}

func (w *ConversationWorker) expireApproval(ctx context.Context, pending *pendingConversationApproval) (protocol.PermissionRespondParams, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := ctx.Err(); err != nil {
		if w.pendingApproval == pending {
			w.pendingApproval = nil
		}
		return protocol.PermissionRespondParams{}, err
	}
	if w.pendingApproval == pending && pending.responded {
		select {
		case response := <-pending.result:
			w.pendingApproval = nil
			w.cacheSessionApprovalLocked(pending.request.Tool, pending.request.Arguments, response)
			return response, nil
		default:
		}
	}
	if w.pendingApproval == pending {
		w.pendingApproval = nil
	}
	return protocol.PermissionRespondParams{}, errors.New("tool approval timed out")
}

func resetTimer(timer *time.Timer, delay time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	if delay < 0 {
		delay = 0
	}
	timer.Reset(delay)
}

func (w *ConversationWorker) sessionToolIsAllowedLocked(tool string, args map[string]any) bool {
	if tool == "bash" {
		command, _ := args["command"].(string)
		_, ok := w.sessionBashAllow[command]
		return ok && command != ""
	}
	_, ok := w.sessionToolAllow[tool]
	return ok && isReadOnlyTool(tool)
}

func isReadOnlyTool(tool string) bool {
	switch tool {
	case "read_file", "list_directory", "search_files":
		return true
	default:
		return false
	}
}

func (w *ConversationWorker) cacheSessionApprovalLocked(tool string, args map[string]any, response protocol.PermissionRespondParams) {
	if response.Decision != protocol.DecisionAllowSession {
		return
	}
	if tool == "bash" {
		command, _ := args["command"].(string)
		if command != "" {
			if w.sessionBashAllow == nil {
				w.sessionBashAllow = make(map[string]struct{})
			}
			w.sessionBashAllow[command] = struct{}{}
		}
	} else if isReadOnlyTool(tool) {
		if w.sessionToolAllow == nil {
			w.sessionToolAllow = make(map[string]struct{})
		}
		w.sessionToolAllow[tool] = struct{}{}
	}
}

// RespondPermission applies a controller's decision to the exact active request.
func (w *ConversationWorker) RespondPermission(conn ClientConn, params protocol.PermissionRespondParams) error {
	if params.Decision != protocol.DecisionAllowOnce && params.Decision != protocol.DecisionAllowSession && params.Decision != protocol.DecisionDeny {
		return errors.New("permission decision must be allow_once, allow_session, or deny")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if conn == nil || w.controller != conn {
		return errors.New("only the current conversation controller may respond to permission requests")
	}
	pending := w.pendingApproval
	if !w.active || w.currentTurn != params.TurnID || pending == nil || pending.responded ||
		pending.request.RequestID != params.RequestID || pending.request.TurnID != params.TurnID {
		return errors.New("no pending permission request matching conversation turn and request ID")
	}
	if w.currentTurnCtx == nil || w.currentTurnCtx.Err() != nil {
		return errors.New("conversation turn is no longer active")
	}
	if time.Now().After(pending.deadline) {
		return errors.New("permission request has expired")
	}
	if params.Decision != protocol.DecisionAllowOnce && params.ModifiedArguments != nil {
		return errors.New("modified arguments can only be approved once")
	}
	if params.Decision == protocol.DecisionAllowSession {
		if pending.request.Tool == "bash" {
			command, _ := pending.request.Arguments["command"].(string)
			if command == "" {
				return errors.New("cannot authorize an empty bash command for the session")
			}
			if params.CommandPattern != "" && params.CommandPattern != command {
				return errors.New("bash session authorization must match the exact approved command")
			}
			params.CommandPattern = command
		} else if !isReadOnlyTool(pending.request.Tool) {
			return errors.New("session authorization is limited to read-only tools and exact bash commands")
		} else if params.CommandPattern != "" {
			return errors.New("command patterns apply only to bash session authorization")
		}
	}
	params.ConversationID = w.conversationID
	params.TurnID = pending.request.TurnID
	pending.responded = true
	select {
	case pending.result <- params:
		return nil
	default:
		pending.responded = false
		return errors.New("permission approval channel busy")
	}
}

func (w *ConversationWorker) pendingPermissionForController(conn ClientConn) []protocol.PermissionRequestNotification {
	w.mu.Lock()
	defer w.mu.Unlock()
	if conn == nil || w.controller != conn || w.pendingApproval == nil || w.pendingApproval.responded {
		return nil
	}
	return []protocol.PermissionRequestNotification{w.pendingApproval.request}
}

func sendPermissionRequest(conn ClientConn, req protocol.PermissionRequestNotification) error {
	msg, err := protocol.NewNotification(protocol.NotifyPermissionRequired, boundedPermissionRequest(req))
	if err != nil {
		return err
	}
	return conn.SendWithTimeout(msg, time.Second)
}

func boundedPermissionRequest(req protocol.PermissionRequestNotification) protocol.PermissionRequestNotification {
	msg, err := protocol.NewNotification(protocol.NotifyPermissionRequired, req)
	if err != nil {
		return req
	}
	encoded, err := protocol.EncodeMessage(msg)
	if err == nil && len(encoded) <= protocol.DefaultMaxMessageSize {
		return req
	}
	args, _ := json.Marshal(req.Arguments)
	req.Arguments = map[string]any{"preview": truncateForEvent(string(args), 16*1024)}
	return req
}

// Cancel requests cancellation of the worker and its active turn.
func (w *ConversationWorker) Cancel() { w.cancel() }

// EmitEvent adds a nonterminal engine event to the conversation stream.
func (w *ConversationWorker) EmitEvent(evt agent.Event) { w.emit(evt, false) }

func (w *ConversationWorker) sequenceLocked(evt agent.Event, terminal bool) (sequencedEvent, error) {
	w.mu.Lock()
	turnID := w.currentTurn
	if terminal && turnID != "" {
		w.currentTerminalSent = true
	}
	subs := make([]ClientConn, 0, len(w.subscribers))
	for c := range w.subscribers {
		subs = append(subs, c)
	}
	w.mu.Unlock()
	sequence := w.ring.PushWithIdentity(evt, terminal, turnID)
	msg, err := createConversationEventNotification(w.conversationID, w.workerEpoch, sequence, turnID, evt, terminal)
	if err != nil {
		return sequencedEvent{}, err
	}
	return sequencedEvent{sequence: sequence, message: msg, subscribers: subs}, nil
}

func (w *ConversationWorker) broadcast(events []sequencedEvent) {
	if len(events) == 0 {
		return
	}
	w.broadcastMu.Lock()
	defer w.broadcastMu.Unlock()
	for _, event := range events {
		w.pendingBroadcasts[event.sequence] = event
	}
	for {
		event, ok := w.pendingBroadcasts[w.nextBroadcast]
		if !ok {
			return
		}
		delete(w.pendingBroadcasts, w.nextBroadcast)
		w.nextBroadcast++
		for _, c := range event.subscribers {
			w.eventMu.Lock()
			if _, replaying := w.replaying[c]; replaying {
				w.replaying[c] = append(w.replaying[c], event)
				w.eventMu.Unlock()
				continue
			}
			w.eventMu.Unlock()
			if err := c.SendWithTimeout(event.message, time.Second); err != nil {
				w.Detach(c)
				_ = c.Close()
			}
		}
	}
}

func (w *ConversationWorker) emit(evt agent.Event, terminal bool) {
	if evt.Type == agent.EventToolResult && len(evt.ToolResult) > 256*1024 {
		stored := persistence.RedactSecrets(evt.ToolResult)
		ref, total, truncated := w.outputStore.put(stored)
		evt.OutputRef, evt.OutputBytes, evt.OutputTruncated = ref, total, truncated
		evt.ToolResult = boundedToolPreview(stored, 1024)
	}
	w.eventMu.Lock()
	item, err := w.sequenceLocked(evt, terminal)
	w.eventMu.Unlock()
	if err == nil {
		w.broadcast([]sequencedEvent{item})
	}
}

// commitWithEvents places revision-advancing writes and their corresponding
// event sequences in one per-conversation order. Network sends happen afterward.
func (w *ConversationWorker) commitWithEvents(commit func() error, events []agent.Event) error {
	w.eventMu.Lock()
	if commit != nil {
		if err := commit(); err != nil {
			w.eventMu.Unlock()
			return err
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	revision, revisionErr := w.store.GetHistoryRevision(ctx, w.conversationID)
	cancel()
	if revisionErr != nil {
		w.eventMu.Unlock()
		return revisionErr
	}
	staged := make([]sequencedEvent, 0, len(events))
	for _, evt := range events {
		if evt.Type == agent.EventFinished {
			continue
		}
		evt.HistoryRevision = revision
		item, err := w.sequenceLocked(evt, false)
		if err != nil {
			w.eventMu.Unlock()
			return err
		}
		staged = append(staged, item)
		w.lastDurableSequence = item.sequence
	}
	w.eventMu.Unlock()
	w.broadcast(staged)
	return nil
}

func (w *ConversationWorker) applySubmission(apply func() (*persistence.SubmissionRecord, error)) (*persistence.SubmissionRecord, error) {
	w.eventMu.Lock()
	record, err := apply()
	var staged []sequencedEvent
	if err == nil {
		step := 0
		if record.AppliedAfterStep != nil {
			step = *record.AppliedAfterStep
		}
		evt := agent.Event{Type: agent.EventSubmissionApplied, Step: step, Content: record.Prompt, HistoryRevision: record.AppliedHistoryRevision, Timestamp: time.Now()}
		item, sequenceErr := w.sequenceLocked(evt, false)
		if sequenceErr != nil {
			err = sequenceErr
		} else {
			staged = append(staged, item)
			w.lastDurableSequence = item.sequence
		}
	}
	w.eventMu.Unlock()
	w.broadcast(staged)
	return record, err
}

func (w *ConversationWorker) finishTurn(turnID, answer string, _ persistence.SessionStatus, runErr error, commit func() error) error {
	w.eventMu.Lock()
	commitErr := commit()
	for _, submissionID := range w.attachedSteers[turnID] {
		if err := w.store.UpdateSubmissionStatus(context.Background(), w.conversationID, submissionID, persistence.SubmissionConsumed, turnID); err != nil && commitErr == nil {
			commitErr = err
		}
	}
	delete(w.attachedSteers, turnID)
	// If the active turn ended before another model boundary, promote staged
	// steers ahead of ordinary queued submissions for the next turn.
	if len(w.pendingSteers) > 0 {
		w.mu.Lock()
		w.queue = append(w.queue, w.pendingSteers...)
		w.pendingSteers = nil
		sort.SliceStable(w.queue, func(i, j int) bool {
			if w.queue[i].Mode != w.queue[j].Mode {
				return w.queue[i].Mode == persistence.SubmissionSteer
			}
			return w.queue[i].Sequence < w.queue[j].Sequence
		})
		w.mu.Unlock()
	}
	event := agent.Event{Type: agent.EventFinished, Content: answer, Timestamp: time.Now()}
	if runErr != nil {
		event.Type = agent.EventError
		event.Content = runErr.Error()
	}
	if commitErr != nil {
		event.Type = agent.EventError
		event.Content = commitErr.Error()
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	event.HistoryRevision, _ = w.store.GetHistoryRevision(ctx, w.conversationID)
	cancel()
	item, sequenceErr := w.sequenceLocked(event, true)
	if sequenceErr == nil {
		w.lastDurableSequence = item.sequence
	}
	w.eventMu.Unlock()
	if sequenceErr == nil {
		w.broadcast([]sequencedEvent{item})
	}
	if commitErr != nil {
		return commitErr
	}
	return sequenceErr
}

// Evict releases an idle worker and notifies its connected clients.
func (w *ConversationWorker) Evict() {
	w.mu.Lock()
	if w.evictionStarted || w.active {
		w.mu.Unlock()
		return
	}
	w.closed = true
	w.evictionStarted = true
	connections := make([]ClientConn, 0, len(w.subscribers))
	for c := range w.subscribers {
		connections = append(connections, c)
	}
	w.subscribers = make(map[ClientConn]struct{})
	w.mu.Unlock()
	msg, _ := protocol.NewNotification(protocol.NotifyConversationEvicted, map[string]string{"conversation_id": w.conversationID})
	for _, c := range connections {
		_ = c.Send(msg)
	}
	w.Stop()
}

func (w *ConversationWorker) reserveEviction() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed || w.active || len(w.queue) > 0 || len(w.pendingSteers) > 0 {
		return false
	}
	w.closed = true
	return true
}

func (w *ConversationWorker) idleForRetention() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return !w.closed && !w.active && len(w.queue) == 0 && len(w.pendingSteers) == 0
}

// Stop requests shutdown and waits for worker cleanup to finish.
func (w *ConversationWorker) Stop() {
	w.StopAsync()
	<-w.done
}

// StopAsync requests shutdown without waiting for an in-flight provider or tool
// to return. The run goroutine owns cleanup once it exits.
func (w *ConversationWorker) StopAsync() {
	w.mu.Lock()
	w.closed = true
	started := w.runStarted
	w.mu.Unlock()
	w.cancel()
	if started {
		return
	}
	w.cleanupOnce.Do(func() {
		w.outputStore.clear()
		if w.cleanup != nil {
			w.cleanup()
		}
	})
	w.doneOnce.Do(func() { close(w.done) })
}

func generateWorkerID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(b)
}
