package history

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/openai/openai-go"
)

// Session manages the complete Three-Partition conversational lifecycle,
// enforcing strict atomic turn blocks, staged micro-pruning, and multi-tier compaction.
type Session struct {
	mu        sync.RWMutex
	compactMu sync.Mutex // Serializes watermark checks and forced compaction [F-05]

	systemAnchor      string        // Partition 1: Immutable System Anchor
	episodicNarrative string        // Partition 2: Distilled narrative summary
	tracker           *StateTracker // Partition 2: Deterministic state table
	blocks            []TurnBlock   // Partition 3 (and staged intermediate history)
	config            SessionConfig
	estimator         *UsageTracker
}

// NewSession initializes a new history Session with production defaults.
func NewSession(systemPrompt, task string, opts ...Option) *Session {
	cfg := DefaultSessionConfig()
	for _, opt := range opts {
		opt(&cfg)
	}

	tracker := NewStateTracker(task, nil)
	s := &Session{
		systemAnchor: systemPrompt,
		tracker:      tracker,
		config:       cfg,
		blocks:       make([]TurnBlock, 0, 16),
	}

	// Initialize with root user task block
	rootBlock := NewUserBlock(task)
	rootBlock.EstimatedTokens = EstimateBlock(&rootBlock)
	s.blocks = append(s.blocks, rootBlock)

	initialTokens := EstimateContent(systemPrompt) + rootBlock.EstimatedTokens
	s.estimator = NewUsageTracker(initialTokens)

	return s
}

// CompileMessages generates API-compliant ChatCompletionMessageParamUnion slices
// adhering strictly to alternating conversational roles and Three-Partition boundaries.
func (s *Session) CompileMessages() []openai.ChatCompletionMessageParamUnion {
	s.mu.RLock()
	defer s.mu.RUnlock()

	msgs := make([]openai.ChatCompletionMessageParamUnion, 0, 8+len(s.blocks)*2)

	// 1. Partition 1: System Anchor (Pinned at prefix 0)
	if s.systemAnchor != "" {
		msgs = append(msgs, openai.SystemMessage(s.systemAnchor))
	}

	// 2. Partition 2: Structured Episodic Anchor
	// Rendered as an openai.UserMessage to preserve valid alternating conversation turns.
	stateTable := s.tracker.RenderEpisodicState()
	hasEpisodicState := s.episodicNarrative != "" || s.tracker.HasDynamicUpdates()

	startIndex := 0
	if hasEpisodicState {
		var episodicBuilder strings.Builder
		episodicBuilder.WriteString(stateTable)
		if s.episodicNarrative != "" {
			episodicBuilder.WriteString("\n\n### Distilled Progress Summary\n")
			episodicBuilder.WriteString(s.episodicNarrative)
		}

		// [F-02] If the oldest uncompacted turn in s.blocks is the initial root user task (BlockKindUser),
		// coalesce it into Partition 2 (which already contains the Root Objective) rather than emitting
		// two consecutive User turns, preserving strict provider alternating role invariants.
		if len(s.blocks) > 0 && s.blocks[0].Kind == BlockKindUser {
			startIndex = 1
		}

		msgs = append(msgs, openai.UserMessage(episodicBuilder.String()))
	} else {
		// If P2 was omitted and the first block in P3 is an assistant turn,
		// inject a synthetic user bridge turn to guarantee alternating conversational turns.
		if len(s.blocks) > 0 && s.blocks[0].Kind != BlockKindUser {
			msgs = append(msgs, openai.UserMessage("Continue executing the task based on the established context."))
		}
	}

	for i := startIndex; i < len(s.blocks); i++ {
		compiled := s.blocks[i].CompileMessages()
		msgs = append(msgs, compiled...)
	}

	return msgs
}

// CalibrateUsage synchronizes baseline token tracking with exact prompt tokens from the API response.
func (s *Session) CalibrateUsage(promptTokens int64) {
	if promptTokens > 0 {
		s.estimator.Calibrate(promptTokens)
	}
}

// CommitSimpleAssistant atomically records an assistant message without tool calls.
func (s *Session) CommitSimpleAssistant(msg openai.ChatCompletionMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()

	block := NewAssistantSimpleBlock(msg)
	block.EstimatedTokens = EstimateBlock(&block)
	s.blocks = append(s.blocks, block)
	s.estimator.AddDelta(block.EstimatedTokens)
	s.eagerMicroPruneBoundaryLocked()
}

// CommitToolTurn atomically records an assistant tool call message and all corresponding tool responses.
func (s *Session) CommitToolTurn(
	msg openai.ChatCompletionMessage,
	results []ToolResult,
) {
	s.mu.Lock()
	defer s.mu.Unlock()

	block := NewAssistantToolCallBlock(msg, results)
	block.EstimatedTokens = EstimateBlock(&block)
	s.blocks = append(s.blocks, block)
	s.estimator.AddDelta(block.EstimatedTokens)
	s.eagerMicroPruneBoundaryLocked()
}

// eagerMicroPruneBoundaryLocked micro-prunes the block that just exited the working tail [F-04],
// ensuring historical intermediate blocks remain byte-for-byte immutable across turns.
func (s *Session) eagerMicroPruneBoundaryLocked() {
	if len(s.blocks) <= s.config.WorkingTailSize {
		return
	}
	boundaryIndex := len(s.blocks) - s.config.WorkingTailSize
	transitionIdx := boundaryIndex - 1
	if transitionIdx >= 0 {
		reclaimed, _ := PruneStaleToolOutputs(&s.blocks[transitionIdx], DefaultMinPruneBytes)
		if reclaimed > 0 {
			s.estimator.AddDelta(-reclaimed)
		}
	}
}

// RecordFileAccess delegates touched file metadata tracking to the internal StateTracker.
func (s *Session) RecordFileAccess(path, action string) {
	s.tracker.RecordFileAccess(path, action)
}

// RecordError delegates error snippet logging to the internal StateTracker.
func (s *Session) RecordError(errStr string) {
	s.tracker.RecordError(errStr)
}

// ReloadSystemAnchor updates Partition 1 when durable project notes (MEMORY.md) are modified.
func (s *Session) ReloadSystemAnchor(newSystemPrompt string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.systemAnchor = newSystemPrompt
}

// MaxContextTokens returns the current maximum context limit.
func (s *Session) MaxContextTokens() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config.MaxContextTokens
}

// MaxTurnOutputBytes returns the maximum cumulative bytes permitted for tool outputs
// within a single turn, derived from headroom buffer limits.
func (s *Session) MaxTurnOutputBytes() int {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Headroom = min(30% of max, max(15% of max, 8 * 2800 + 1000))
	maxCap := float64(s.config.MaxContextTokens) * 0.30
	scaledHeadroom := 8*2800 + 1000.0
	minCap := float64(s.config.MaxContextTokens) * 0.15

	headroomTokens := scaledHeadroom
	if headroomTokens < minCap {
		headroomTokens = minCap
	}
	if headroomTokens > maxCap {
		headroomTokens = maxCap
	}

	// 1 token ~= 3.5 bytes
	return int(headroomTokens * RuneTokenRatio)
}

// EnsureWatermarks inspects token usage against soft and hard watermarks,
// executing Tier 1 staged boundary micro-pruning, Tier 2 distillation, and Tier 3 fallback.
func (s *Session) EnsureWatermarks(
	ctx context.Context,
	client CompletionClient,
	model string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.compactMu.Lock()
	defer s.compactMu.Unlock()

	s.mu.Lock()

	hardWatermark := int(float64(s.config.MaxContextTokens) * s.config.HardWatermarkRatio)

	// Ensure all intermediate blocks before boundaryIndex are micro-pruned
	if len(s.blocks) > s.config.WorkingTailSize {
		boundaryIndex := len(s.blocks) - s.config.WorkingTailSize
		reclaimedTotal := 0
		for i := 0; i < boundaryIndex; i++ {
			reclaimed, _ := PruneStaleToolOutputs(&s.blocks[i], DefaultMinPruneBytes)
			reclaimedTotal += reclaimed
		}
		if reclaimedTotal > 0 {
			s.estimator.AddDelta(-reclaimedTotal)
		}
	}

	currentTokens := s.estimator.CurrentTokens()

	// If hard watermark not reached or no blocks outside working tail, check safe halt and return
	if currentTokens < hardWatermark || len(s.blocks) <= s.config.WorkingTailSize {
		if s.estimator.CurrentTokens() > s.config.MaxContextTokens {
			s.mu.Unlock()
			return ErrContextExhausted
		}
		s.mu.Unlock()
		return nil
	}

	// Tier 2: Macro-Distillation & Tier 3: Deterministic Fallback
	// Snapshot intermediate candidate blocks using Clone() [F-10]
	boundaryIndex := len(s.blocks) - s.config.WorkingTailSize
	intermediateBlocks := make([]TurnBlock, boundaryIndex)
	for i := 0; i < boundaryIndex; i++ {
		intermediateBlocks[i] = s.blocks[i].Clone()
	}
	narrativeSnapshot := s.episodicNarrative
	distillModel := model
	if s.config.DistillationModel != "" {
		distillModel = s.config.DistillationModel
	}
	trackerRef := s.tracker

	// Release lock during distillation network call [B3]
	s.mu.Unlock()

	// Check parent cancellation before network call
	if err := ctx.Err(); err != nil {
		return err
	}

	// Attempt Tier 2 Distillation with isolated timeout
	summary, err := DistillIntermediateBlocks(
		ctx,
		client,
		distillModel,
		narrativeSnapshot,
		intermediateBlocks,
		trackerRef,
	)

	s.mu.Lock()
	defer s.mu.Unlock()

	// [F-01] If parent context was canceled or timed out, propagate parent ctx.Err() without mutating state
	if ctx.Err() != nil {
		return ctx.Err()
	}

	if err == nil && summary != "" {
		// Tier 2 Success: Commit new Episodic Anchor and evict intermediate blocks
		s.episodicNarrative = summary
	} else {
		// Tier 3 Local Fallback: Deterministic eviction with notice in state table
		notice := fmt.Sprintf(
			"[Notice: %d intermediate turns evicted via local fallback due to summarizer unavailability: %v]",
			len(intermediateBlocks),
			err,
		)
		if s.episodicNarrative != "" {
			s.episodicNarrative += "\n\n" + notice
		} else {
			s.episodicNarrative = notice
		}
	}

	if len(s.blocks) >= len(intermediateBlocks) {
		s.blocks = s.blocks[len(intermediateBlocks):]
	}
	s.estimator.ResetBaseline(s.recalculateTotalTokensLocked())

	// Safe Halt Check: Verify irreducible context does not exceed hard ceiling
	if s.estimator.CurrentTokens() > s.config.MaxContextTokens {
		return ErrContextExhausted
	}

	return nil
}

// DownscaleWatermark lowers the maximum context ceiling dynamically (e.g. upon HTTP 400 recovery).
// If newCeiling <= 0, applies a 0.75x proportional downscale from the current ceiling.
// Clamps ceiling at MinViableContextTokens (8192).
// Returns ErrContextExhausted if irreducible state exceeds the new ceiling.
func (s *Session) DownscaleWatermark(newCeiling int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// [F-08] If provider returns explicit limit below MinViableContextTokens,
	// abort immediately rather than clamping up to 8k and performing doomed retries.
	if newCeiling > 0 && newCeiling < MinViableContextTokens {
		return ErrContextExhausted
	}

	if newCeiling <= 0 {
		newCeiling = int(float64(s.config.MaxContextTokens) * 0.75)
	}
	if newCeiling < MinViableContextTokens {
		newCeiling = MinViableContextTokens
	}

	// [F-07] Calculate irreducible state:
	// System Anchor (P1) + Episodic State Table & Narrative (P2) + Working Tail blocks (P3)
	irreducible := EstimateContent(s.systemAnchor)
	irreducible += EstimateContent(s.tracker.RenderEpisodicState())
	if s.episodicNarrative != "" {
		irreducible += EstimateContent(s.episodicNarrative)
	}

	tailStart := len(s.blocks) - s.config.WorkingTailSize
	if tailStart < 0 {
		tailStart = 0
	}
	for i := tailStart; i < len(s.blocks); i++ {
		irreducible += EstimateBlock(&s.blocks[i])
	}

	if newCeiling < irreducible {
		return ErrContextExhausted
	}

	s.config.MaxContextTokens = newCeiling
	return nil
}

// ForceCompaction immediately forces compaction of intermediate blocks outside the working tail,
// executing Tier 2 distillation or Tier 3 fallback regardless of current token watermark levels.
func (s *Session) ForceCompaction(ctx context.Context, client CompletionClient, model string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.compactMu.Lock()
	defer s.compactMu.Unlock()

	s.mu.Lock()
	if len(s.blocks) <= s.config.WorkingTailSize {
		// Nothing to compact outside the working tail
		currentTokens := s.estimator.CurrentTokens()
		s.mu.Unlock()
		if currentTokens > s.config.MaxContextTokens {
			return ErrContextExhausted
		}
		return nil
	}

	boundaryIndex := len(s.blocks) - s.config.WorkingTailSize
	intermediateBlocks := make([]TurnBlock, boundaryIndex)
	for i := 0; i < boundaryIndex; i++ {
		intermediateBlocks[i] = s.blocks[i].Clone()
	}
	narrativeSnapshot := s.episodicNarrative
	distillModel := model
	if s.config.DistillationModel != "" {
		distillModel = s.config.DistillationModel
	}
	trackerRef := s.tracker
	s.mu.Unlock()

	// Check cancellation before network call
	if err := ctx.Err(); err != nil {
		return err
	}

	summary, err := DistillIntermediateBlocks(
		ctx,
		client,
		distillModel,
		narrativeSnapshot,
		intermediateBlocks,
		trackerRef,
	)

	s.mu.Lock()
	defer s.mu.Unlock()

	// [F-01] If parent context was canceled or timed out, propagate parent ctx.Err() without mutating state
	if ctx.Err() != nil {
		return ctx.Err()
	}

	if err == nil && summary != "" {
		s.episodicNarrative = summary
	} else {
		notice := fmt.Sprintf(
			"[Notice: %d intermediate turns evicted via local fallback: %v]",
			len(intermediateBlocks),
			err,
		)
		if s.episodicNarrative != "" {
			s.episodicNarrative += "\n\n" + notice
		} else {
			s.episodicNarrative = notice
		}
	}

	if len(s.blocks) >= len(intermediateBlocks) {
		s.blocks = s.blocks[len(intermediateBlocks):]
	}
	s.estimator.ResetBaseline(s.recalculateTotalTokensLocked())

	if s.estimator.CurrentTokens() > s.config.MaxContextTokens {
		return ErrContextExhausted
	}

	return nil
}

// recalculateTotalTokensLocked calculates total tokens across all active partitions.
func (s *Session) recalculateTotalTokensLocked() int {
	total := EstimateContent(s.systemAnchor)
	total += EstimateContent(s.tracker.RenderEpisodicState())
	if s.episodicNarrative != "" {
		total += EstimateContent(s.episodicNarrative)
	}
	for i := range s.blocks {
		total += EstimateBlock(&s.blocks[i])
	}
	return total
}

// CurrentTokens returns the current estimated token footprint.
func (s *Session) CurrentTokens() int {
	return s.estimator.CurrentTokens()
}

// BlockCount returns the number of active turn blocks in session memory.
func (s *Session) BlockCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.blocks)
}

// RestoredState holds state needed to reconstruct a Session during crash recovery.
type RestoredState struct {
	SessionID         string
	Task              string
	SystemAnchor      string
	CompactedStep     int
	EpisodicNarrative string
	StateTracker      *StateTracker
	TrailingTurns     []TurnBlock
}

// RestoreSession reconstructs a history Session from an episodic snapshot and trailing unpruned turns.
func RestoreSession(state RestoredState, opts ...Option) (*Session, error) {
	cfg := DefaultSessionConfig()
	for _, opt := range opts {
		opt(&cfg)
	}

	tracker := state.StateTracker
	if tracker == nil {
		tracker = NewStateTracker(state.Task, nil)
	}

	s := &Session{
		systemAnchor:      state.SystemAnchor,
		episodicNarrative: state.EpisodicNarrative,
		tracker:           tracker,
		config:            cfg,
		blocks:            make([]TurnBlock, 0, len(state.TrailingTurns)+1),
	}

	// Invariant: If no compaction has occurred (CompactedStep == 0), the root user task
	// must be anchored as blocks[0] to preserve conversational role alternation and prompt fidelity.
	if state.CompactedStep == 0 {
		rootBlock := NewUserBlock(state.Task)
		rootBlock.EstimatedTokens = EstimateBlock(&rootBlock)
		s.blocks = append(s.blocks, rootBlock)
	}

	s.blocks = append(s.blocks, state.TrailingTurns...)
	s.estimator = NewUsageTracker(s.recalculateTotalTokensLocked())

	return s, nil
}

// ExtractSnapshot exports episodic narrative, serialized state tracker JSON, and token estimate.
func (s *Session) ExtractSnapshot() (narrative string, trackerJSON string, tokens int) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data := s.tracker.ExportData()
	bytes, _ := json.Marshal(data)
	return s.episodicNarrative, string(bytes), s.estimator.CurrentTokens()
}

// EpisodicNarrative returns the distilled narrative.
func (s *Session) EpisodicNarrative() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.episodicNarrative
}

// Tracker returns the active StateTracker.
func (s *Session) Tracker() *StateTracker {
	return s.tracker
}
