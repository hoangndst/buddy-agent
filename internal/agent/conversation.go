package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/hoangndst/buddy-agent/internal/history"
	"github.com/hoangndst/buddy-agent/internal/persistence"
)

// ConversationRuntime holds reusable history and serializes agent turns for one conversation.
// Transport-specific admission and event ordering remain the responsibility of the daemon worker.
type ConversationRuntime struct {
	mu                  sync.Mutex
	engine              *Engine
	store               persistence.Store
	sessionID           string
	firstSubmissionID   string
	history             *history.Session
	appliedSubmissions  map[string]struct{}
	overridesMu         sync.Mutex
	promptOverrides     map[string]string
	applySubmissionHook func(func() (*persistence.SubmissionRecord, error)) (*persistence.SubmissionRecord, error)
	turnOutcomeHook     func(string, string, persistence.SessionStatus, error, func() error) error
}

func (c *ConversationRuntime) SetApplySubmissionHook(hook func(func() (*persistence.SubmissionRecord, error)) (*persistence.SubmissionRecord, error)) {
	c.applySubmissionHook = hook
}

func (c *ConversationRuntime) SetTurnOutcomeHook(hook func(string, string, persistence.SessionStatus, error, func() error) error) {
	c.turnOutcomeHook = hook
}

func (c *ConversationRuntime) SetPromptOverride(submissionID, prompt string) {
	c.overridesMu.Lock()
	c.promptOverrides[submissionID] = prompt
	c.overridesMu.Unlock()
}

func (c *ConversationRuntime) DeletePromptOverride(submissionID string) {
	c.overridesMu.Lock()
	delete(c.promptOverrides, submissionID)
	c.overridesMu.Unlock()
}

func (c *ConversationRuntime) PromptOverride(submissionID string) (string, bool) {
	c.overridesMu.Lock()
	defer c.overridesMu.Unlock()
	prompt, ok := c.promptOverrides[submissionID]
	return prompt, ok
}

// ApplySteerPrompt is called only at a serialized model-input boundary.
func (c *ConversationRuntime) ApplySteerPrompt(submissionID, prompt string) error {
	if submissionID == "" || prompt == "" {
		return errors.New("steering prompt requires submission ID and content")
	}
	if _, alreadyApplied := c.appliedSubmissions[submissionID]; alreadyApplied {
		return nil
	}
	if err := c.history.AppendUserPrompt(prompt); err != nil {
		return err
	}
	c.appliedSubmissions[submissionID] = struct{}{}
	return nil
}

// NewConversationRuntime binds a history session to an already-created persistent conversation.
func NewConversationRuntime(ctx context.Context, engine *Engine, store persistence.Store, sessionID, firstSubmissionID string, historySession *history.Session) (*ConversationRuntime, error) {
	if engine == nil || store == nil || sessionID == "" || firstSubmissionID == "" || historySession == nil {
		return nil, errors.New("conversation runtime requires engine, store, session ID, first submission ID, and history")
	}
	session, err := store.GetSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load conversation session: %w", err)
	}
	if session.InteractionKind != persistence.InteractionConversation || session.Depth != 0 || session.ParentSessionID != nil {
		return nil, fmt.Errorf("session %q is not a root conversation", sessionID)
	}
	first, err := store.GetSubmission(ctx, sessionID, firstSubmissionID)
	if err != nil {
		return nil, fmt.Errorf("load first conversation submission: %w", err)
	}
	if first.Sequence != 1 {
		return nil, fmt.Errorf("first conversation submission %q has sequence %d", firstSubmissionID, first.Sequence)
	}
	return &ConversationRuntime{
		engine:             engine,
		store:              store,
		sessionID:          sessionID,
		firstSubmissionID:  firstSubmissionID,
		history:            historySession,
		appliedSubmissions: map[string]struct{}{firstSubmissionID: {}},
		promptOverrides:    make(map[string]string),
	}, nil
}

// RunTurn serializes one queued user submission against the conversation's shared history.
func (c *ConversationRuntime) RunTurn(ctx context.Context, submissionID, turnID string) (string, error) {
	if submissionID == "" || turnID == "" {
		return "", errors.New("conversation turn requires submission and turn IDs")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	submission, err := c.store.GetSubmission(ctx, c.sessionID, submissionID)
	if err != nil {
		return "", fmt.Errorf("load conversation submission: %w", err)
	}
	if submission.Status == persistence.SubmissionConsumed {
		return "", fmt.Errorf("conversation submission %q has already been consumed", submissionID)
	}
	prompt := submission.Prompt
	c.overridesMu.Lock()
	if override, ok := c.promptOverrides[submissionID]; ok {
		prompt = override
	}
	c.overridesMu.Unlock()
	storedTurns, err := c.store.GetAllTurns(ctx, c.sessionID)
	if err != nil {
		return "", fmt.Errorf("load conversation step history: %w", err)
	}
	lastStep := 0
	for _, stored := range storedTurns {
		if stored.StepNumber > lastStep {
			lastStep = stored.StepNumber
		}
	}
	if _, applied := c.appliedSubmissions[submissionID]; !applied {
		if submission.Status != persistence.SubmissionAttached {
			apply := func() (*persistence.SubmissionRecord, error) {
				return c.store.ApplySubmission(ctx, c.sessionID, submissionID, turnID, lastStep)
			}
			if c.applySubmissionHook != nil {
				submission, err = c.applySubmissionHook(apply)
			} else {
				submission, err = apply()
			}
			if err != nil {
				return "", fmt.Errorf("apply conversation submission: %w", err)
			}
		}
		if err := c.history.AppendUserPrompt(prompt); err != nil {
			return "", fmt.Errorf("append conversation prompt: %w", err)
		}
		c.appliedSubmissions[submissionID] = struct{}{}
	}

	turns, err := c.store.ListConversationTurns(ctx, c.sessionID)
	if err != nil {
		return "", fmt.Errorf("list conversation turns: %w", err)
	}
	turnExists := false
	for _, turn := range turns {
		if turn.TurnID == turnID {
			turnExists = true
			break
		}
	}
	if !turnExists {
		if err := c.store.CreateConversationTurn(ctx, &persistence.ConversationTurnRecord{
			ConversationID:  c.sessionID,
			TurnID:          turnID,
			SubmissionID:    submissionID,
			Status:          persistence.StatusRunning,
			StartedAt:       time.Now().UTC(),
			HistoryRevision: submission.AppliedHistoryRevision,
		}); err != nil {
			return "", fmt.Errorf("create conversation turn: %w", err)
		}
	}

	startStep := 1
	for _, stored := range storedTurns {
		if stored.StepNumber >= startStep {
			startStep = stored.StepNumber + 1
		}
	}
	session, err := c.store.GetSession(ctx, c.sessionID)
	if err != nil {
		return "", fmt.Errorf("load conversation usage: %w", err)
	}
	answer, runErr := c.engine.RunConversationTurn(ctx, c.sessionID, turnID, c.history, startStep, c.engine.maxSteps, session.TokenUsage)
	status := persistence.StatusCompleted
	if runErr != nil {
		status = persistence.StatusFailed
		if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			status = persistence.StatusInterrupted
		}
	}
	finishedAt := time.Now().UTC()
	var errorMessage *string
	if runErr != nil {
		message := runErr.Error()
		errorMessage = &message
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	commitOutcome := func() error {
		if err := c.store.UpdateConversationTurn(persistCtx, &persistence.ConversationTurnRecord{
			ConversationID: c.sessionID, TurnID: turnID, Status: status, FinishedAt: &finishedAt, ErrorMessage: errorMessage,
		}); err != nil {
			return fmt.Errorf("persist conversation turn outcome: %w", err)
		}
		if err := c.store.UpdateSubmissionStatus(persistCtx, c.sessionID, submissionID, persistence.SubmissionConsumed, turnID); err != nil {
			return fmt.Errorf("mark conversation submission consumed: %w", err)
		}
		return nil
	}
	var outcomeErr error
	if c.turnOutcomeHook != nil {
		outcomeErr = c.turnOutcomeHook(turnID, answer, status, runErr, commitOutcome)
	} else {
		outcomeErr = commitOutcome()
	}
	if outcomeErr != nil && runErr == nil {
		runErr = outcomeErr
	}
	c.DeletePromptOverride(submissionID)
	return answer, runErr
}

// History returns the conversation's shared in-memory history for rendering or state inspection.
func (c *ConversationRuntime) History() *history.Session {
	return c.history
}

// RestoreConversationRuntime rebuilds a conversation history from its latest snapshot,
// committed ReAct steps, and user submissions already attached to turns.
func RestoreConversationRuntime(ctx context.Context, engine *Engine, store persistence.Store, sessionID string) (*ConversationRuntime, error) {
	if engine == nil || store == nil || sessionID == "" {
		return nil, errors.New("restore requires engine, store, and conversation ID")
	}
	sessionRecord, err := store.GetSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load conversation session: %w", err)
	}
	if sessionRecord.InteractionKind != persistence.InteractionConversation || sessionRecord.Depth != 0 || sessionRecord.ParentSessionID != nil {
		return nil, fmt.Errorf("session %q is not a root conversation", sessionID)
	}
	baseSystemPrompt, err := engine.currentSystemPrompt()
	if err != nil {
		return nil, err
	}
	snapshot, err := store.GetLatestSnapshot(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load conversation snapshot: %w", err)
	}
	compactedStep := 0
	snapshotRevision := int64(0)
	narrative := ""
	var tracker *history.StateTracker
	if snapshot != nil {
		compactedStep = snapshot.StepNumber
		snapshotRevision = snapshot.HistoryRevision
		narrative = snapshot.DistilledNarrative
		var state history.StateTrackerData
		if err := json.Unmarshal([]byte(snapshot.StateTrackerJSON), &state); err != nil {
			return nil, fmt.Errorf("decode conversation state tracker: %w", err)
		}
		tracker = history.RestoreStateTracker(state)
	}
	steps, err := store.GetTurnsAfterStep(ctx, sessionID, compactedStep)
	if err != nil {
		return nil, fmt.Errorf("load conversation steps: %w", err)
	}
	submissions, err := store.ListSubmissions(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load conversation submissions: %w", err)
	}
	if len(submissions) == 0 || submissions[0].Sequence != 1 {
		return nil, errors.New("conversation is missing its initial submission")
	}
	firstSubmissionID := submissions[0].SubmissionID
	appliedSubmissions := make([]*persistence.SubmissionRecord, 0, len(submissions))
	for _, submission := range submissions {
		if submission.Sequence == 1 || submission.TurnID == nil || submission.AppliedAfterStep == nil || submission.AppliedHistoryRevision <= snapshotRevision || (submission.Status != persistence.SubmissionAttached && submission.Status != persistence.SubmissionConsumed) {
			continue
		}
		appliedSubmissions = append(appliedSubmissions, submission)
	}
	sort.SliceStable(appliedSubmissions, func(i, j int) bool {
		if *appliedSubmissions[i].AppliedAfterStep == *appliedSubmissions[j].AppliedAfterStep {
			return appliedSubmissions[i].Sequence < appliedSubmissions[j].Sequence
		}
		return *appliedSubmissions[i].AppliedAfterStep < *appliedSubmissions[j].AppliedAfterStep
	})

	trailing := make([]history.TurnBlock, 0, len(steps)+len(submissions))
	appended := make(map[string]struct{}, len(submissions))
	promptIndex := 0
	appendPrompt := func(submission *persistence.SubmissionRecord) {
		block := history.NewUserBlock(submission.Prompt)
		block.StepNumber = *submission.AppliedAfterStep
		trailing = append(trailing, block)
		appended[submission.SubmissionID] = struct{}{}
	}
	for _, step := range steps {
		for promptIndex < len(appliedSubmissions) && *appliedSubmissions[promptIndex].AppliedAfterStep < step.StepNumber {
			appendPrompt(appliedSubmissions[promptIndex])
			promptIndex++
		}
		trailing = append(trailing, mapTurnRecordToBlock(step))
	}
	for promptIndex < len(appliedSubmissions) {
		appendPrompt(appliedSubmissions[promptIndex])
		promptIndex++
	}

	restored, err := history.RestoreSession(history.RestoredState{
		SessionID:         sessionID,
		Task:              sessionRecord.Task,
		SystemAnchor:      baseSystemPrompt,
		CompactedStep:     compactedStep,
		EpisodicNarrative: narrative,
		StateTracker:      tracker,
		TrailingTurns:     trailing,
	}, history.WithMaxContextTokens(engine.maxContextTokens), history.WithWorkingTailSize(engine.workingTailSize))
	if err != nil {
		return nil, fmt.Errorf("restore conversation history: %w", err)
	}
	runtime, err := NewConversationRuntime(ctx, engine, store, sessionID, firstSubmissionID, restored)
	if err != nil {
		return nil, err
	}
	for submissionID := range appended {
		runtime.appliedSubmissions[submissionID] = struct{}{}
	}
	return runtime, nil
}
