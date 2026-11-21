// Package history provides context compaction, session memory management,
// and episodic anchoring for autonomous agent conversations in pure Go.
package history

import (
	"context"
	"errors"
	"time"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

// ErrContextExhausted is returned when the minimum viable context state
// (System Anchor + Episodic Anchor + active turn) exceeds MaxContextTokens.
var ErrContextExhausted = errors.New("context exhausted: minimum viable working state exceeds configured limit")

// MinViableContextTokens defines the hard minimum viable operational context window (8k).
const MinViableContextTokens = 8192

// CompletionClient defines the minimal LLM completion interface required
// by the context engine for Tier 2 distillation. Decoupled from internal/agent
// to guarantee zero cyclic package dependencies.
type CompletionClient interface {
	CreateChatCompletion(ctx context.Context, params openai.ChatCompletionNewParams, opts ...option.RequestOption) (*openai.ChatCompletion, error)
}

// BlockKind identifies the atomic structure of a conversational turn.
type BlockKind int

const (
	// BlockKindUser represents a single user interaction turn.
	BlockKindUser BlockKind = iota
	// BlockKindAssistantSimple represents an assistant text response without tool calls.
	BlockKindAssistantSimple
	// BlockKindAssistantToolCall represents an assistant message with tool calls and matching tool responses.
	BlockKindAssistantToolCall
)

// String returns a human-readable name for the block kind.
func (k BlockKind) String() string {
	switch k {
	case BlockKindUser:
		return "user"
	case BlockKindAssistantSimple:
		return "assistant_simple"
	case BlockKindAssistantToolCall:
		return "assistant_tool_call"
	default:
		return "unknown"
	}
}

// FileRecord tracks access or modification metadata for a file path.
type FileRecord struct {
	Path      string
	Action    string // "created", "modified", "inspected"
	Timestamp time.Time
}

// ToolResult stores the execution result of an individual tool call within an Atomic Turn Block.
type ToolResult struct {
	ToolCallID string
	Name       string
	Content    string
}

// TurnBlock represents an indivisible conversational unit.
// Slicing or evicting history operates strictly on TurnBlock boundaries
// to guarantee zero orphaned tool_call_ids or malformed turn structures.
type TurnBlock struct {
	ID        string
	Kind      BlockKind
	Timestamp time.Time
	StepNumber int

	// User block payload
	UserPrompt string

	// Assistant simple payload
	AssistantSimple *openai.ChatCompletionMessage

	// Assistant tool call payload
	AssistantToolCall *openai.ChatCompletionMessage
	ToolResults       []ToolResult

	// Cached token estimate for this block
	EstimatedTokens int
}

// SessionConfig configures context watermarks and memory boundaries.
type SessionConfig struct {
	MaxContextTokens   int     // Hard token ceiling (default: 64,000)
	WorkingTailSize    int     // Number of recent Atomic Turn Blocks kept unpruned (default: 5)
	SoftWatermarkRatio float64 // Threshold ratio to trigger Tier 1 micro-pruning (default: 0.70)
	HardWatermarkRatio float64 // Threshold ratio to trigger Tier 2 macro-distillation (default: 0.85)
	DistillationModel  string  // Optional model override for summarization
}

// DefaultSessionConfig returns production defaults for SessionConfig.
func DefaultSessionConfig() SessionConfig {
	return SessionConfig{
		MaxContextTokens:   64000,
		WorkingTailSize:    5,
		SoftWatermarkRatio: 0.70,
		HardWatermarkRatio: 0.85,
	}
}

// Option applies a functional configuration to SessionConfig.
type Option func(*SessionConfig)

// WithMaxContextTokens configures the hard token ceiling.
func WithMaxContextTokens(tokens int) Option {
	return func(cfg *SessionConfig) {
		if tokens > 0 {
			cfg.MaxContextTokens = tokens
		}
	}
}

// WithWorkingTailSize configures how many recent turn blocks remain unpruned.
func WithWorkingTailSize(size int) Option {
	return func(cfg *SessionConfig) {
		if size > 0 {
			cfg.WorkingTailSize = size
		}
	}
}

// WithWatermarkRatios configures custom soft and hard watermark trigger ratios.
func WithWatermarkRatios(soft, hard float64) Option {
	return func(cfg *SessionConfig) {
		if soft > 0 && soft < 1.0 {
			cfg.SoftWatermarkRatio = soft
		}
		if hard > 0 && hard < 1.0 && hard >= cfg.SoftWatermarkRatio {
			cfg.HardWatermarkRatio = hard
		}
	}
}

// WithDistillationModel specifies an alternative model for Tier 2 distillation.
func WithDistillationModel(model string) Option {
	return func(cfg *SessionConfig) {
		cfg.DistillationModel = model
	}
}

// CompactionTier identifies which compaction stage was executed.
type CompactionTier int

// Supported compaction execution tiers.
const (
	// TierNone indicates no compaction was needed.
	TierNone CompactionTier = iota
	// Tier1MicroPrune indicates stale tool outputs were collapsed.
	Tier1MicroPrune
	// Tier2MacroDistill indicates intermediate turns were distilled via LLM.
	Tier2MacroDistill
	// Tier3LocalFallback indicates deterministic local eviction occurred.
	Tier3LocalFallback
)

// CompactionResult summarizes the outcome of a watermark check.
type CompactionResult struct {
	Tier            CompactionTier
	PreTokens       int
	PostTokens      int
	ReclaimedTokens int
	PrunedOutputs   int
	Duration        time.Duration
}
