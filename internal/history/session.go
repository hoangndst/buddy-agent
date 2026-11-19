package history

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/openai/openai-go"
)

// Session manages the complete Three-Partition conversational lifecycle,
// enforcing strict atomic turn blocks, staged micro-pruning, and multi-tier compaction.
type Session struct {
	mu sync.RWMutex

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

	if hasEpisodicState {
		var episodicBuilder strings.Builder
		episodicBuilder.WriteString(stateTable)
		if s.episodicNarrative != "" {
			episodicBuilder.WriteString("\n\n### Distilled Progress Summary\n")
			episodicBuilder.WriteString(s.episodicNarrative)
		}
		msgs = append(msgs, openai.UserMessage(episodicBuilder.String()))
	}

	// 3. Partition 3: Active Working Tail (& any active unevicted blocks)
	// If P2 was omitted and the first block in P3 is an assistant turn,
	// inject a synthetic user bridge turn to guarantee alternating conversational turns.
	if !hasEpisodicState && len(s.blocks) > 0 && s.blocks[0].Kind != BlockKindUser {
		msgs = append(msgs, openai.UserMessage("Continue executing the task based on the established context."))
	}

	for i := range s.blocks {
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

	s.mu.Lock()

	softWatermark := int(float64(s.config.MaxContextTokens) * s.config.SoftWatermarkRatio)
	hardWatermark := int(float64(s.config.MaxContextTokens) * s.config.HardWatermarkRatio)

	currentTokens := s.estimator.CurrentTokens()

	// 1. Tier 1: Staged Boundary Micro-Pruning
	// Stale tool outputs transitioning out of the working tail into intermediate history are collapsed.
	if currentTokens >= softWatermark && len(s.blocks) > s.config.WorkingTailSize {
		boundaryIndex := len(s.blocks) - s.config.WorkingTailSize
		reclaimedTotal := 0

		for i := 0; i < boundaryIndex; i++ {
			reclaimed, _ := PruneStaleToolOutputs(&s.blocks[i], DefaultMinPruneBytes)
			reclaimedTotal += reclaimed
		}

		if reclaimedTotal > 0 {
			currentTokens -= reclaimedTotal
			if currentTokens < 0 {
				currentTokens = 0
			}
			s.estimator.ResetBaseline(currentTokens)
		}
	}

	// If hard watermark not reached or no blocks outside working tail, check safe halt and return
	if currentTokens < hardWatermark || len(s.blocks) <= s.config.WorkingTailSize {
		if s.estimator.CurrentTokens() > s.config.MaxContextTokens {
			s.mu.Unlock()
			return ErrContextExhausted
		}
		s.mu.Unlock()
		return nil
	}

	// 2. Tier 2: Macro-Distillation & Tier 3: Deterministic Fallback
	// Snapshot intermediate candidate blocks and state under lock
	boundaryIndex := len(s.blocks) - s.config.WorkingTailSize
	intermediateBlocks := make([]TurnBlock, boundaryIndex)
	copy(intermediateBlocks, s.blocks[:boundaryIndex])
	narrativeSnapshot := s.episodicNarrative
	distillModel := model
	if s.config.DistillationModel != "" {
		distillModel = s.config.DistillationModel
	}
	trackerRef := s.tracker

	// Release lock during distillation network call [B3]
	s.mu.Unlock()

	// Check cancellation before network call
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

	// If parent context canceled or timed out, propagate error without mutating state
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		return ctx.Err()
	}

	if err == nil && summary != "" {
		// Tier 2 Success: Commit new Episodic Anchor and evict intermediate blocks
		s.episodicNarrative = summary
	} else {
		// Tier 3 Local Fallback: Deterministic eviction with notice
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

	// 3. Safe Halt Check: Verify irreducible context does not exceed hard ceiling
	if s.estimator.CurrentTokens() > s.config.MaxContextTokens {
		return ErrContextExhausted
	}

	return nil
}

// DownscaleWatermark lowers the maximum context ceiling dynamically (e.g. upon HTTP 400 recovery).
// If newCeiling <= 0, applies a 0.75x proportional downscale from the current ceiling.
// Clamps ceiling at MinViableContextTokens (8192).
// Returns ErrContextExhausted if irreducible state (P1 + P3 tail) exceeds the new ceiling.
func (s *Session) DownscaleWatermark(newCeiling int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if newCeiling <= 0 {
		newCeiling = int(float64(s.config.MaxContextTokens) * 0.75)
	}
	if newCeiling < MinViableContextTokens {
		newCeiling = MinViableContextTokens
	}

	// Calculate irreducible state: System Anchor (P1) + Working Tail blocks
	irreducible := EstimateContent(s.systemAnchor)
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
	copy(intermediateBlocks, s.blocks[:boundaryIndex])
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

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
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
