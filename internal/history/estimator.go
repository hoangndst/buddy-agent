package history

import (
	"sync/atomic"
	"unicode/utf8"
)

const (
	// ToolCallOverhead represents framing tokens for tool calls and JSON delimiters.
	ToolCallOverhead = 16
	// MessageFramingOverhead represents per-message framing tokens (role, wrappers).
	MessageFramingOverhead = 4
	// RuneTokenRatio represents average runes per token for code, JSON, and prose.
	RuneTokenRatio = 3.5
)

// EstimateContent calculates estimated tokens for a text string in pure Go without CGO.
func EstimateContent(s string) int {
	if s == "" {
		return 0
	}
	runes := utf8.RuneCountInString(s)
	tokens := int(float64(runes) / RuneTokenRatio)
	if tokens == 0 && runes > 0 {
		return 1
	}
	return tokens
}

// EstimateBlock calculates the token footprint of an Atomic Turn Block.
func EstimateBlock(b *TurnBlock) int {
	if b == nil {
		return 0
	}

	total := MessageFramingOverhead

	switch b.Kind {
	case BlockKindUser:
		total += EstimateContent(b.UserPrompt)

	case BlockKindAssistantSimple:
		if b.AssistantSimple != nil {
			total += EstimateContent(b.AssistantSimple.Content)
		}

	case BlockKindAssistantToolCall:
		if b.AssistantToolCall != nil {
			total += EstimateContent(b.AssistantToolCall.Content)
			for _, tc := range b.AssistantToolCall.ToolCalls {
				total += EstimateContent(tc.Function.Name)
				total += EstimateContent(tc.Function.Arguments)
				total += ToolCallOverhead
			}
		}
		for _, tr := range b.ToolResults {
			total += MessageFramingOverhead + EstimateContent(tr.Content)
		}
	}

	return total
}

// UsageTracker provides closed-loop token tracking combining API ground-truth
// calibration with incremental pure-Go delta estimation.
type UsageTracker struct {
	baselineTokens atomic.Int64
	deltaTokens    atomic.Int64
}

// NewUsageTracker initializes a UsageTracker with an initial estimate.
func NewUsageTracker(initialTokens int) *UsageTracker {
	t := &UsageTracker{}
	t.baselineTokens.Store(int64(initialTokens))
	return t
}

// Calibrate sets the exact baseline ground-truth from resp.Usage.PromptTokens
// and resets the uncommitted delta to zero.
func (t *UsageTracker) Calibrate(promptTokens int64) {
	if promptTokens > 0 {
		t.baselineTokens.Store(promptTokens)
		t.deltaTokens.Store(0)
	}
}

// AddDelta records incremental tokens added or reclaimed by local turns.
func (t *UsageTracker) AddDelta(delta int) {
	if delta != 0 {
		t.deltaTokens.Add(int64(delta))
	}
}

// ResetBaseline manually overrides baseline tokens (e.g. after local compaction).
func (t *UsageTracker) ResetBaseline(tokens int) {
	t.baselineTokens.Store(int64(tokens))
	t.deltaTokens.Store(0)
}

// CurrentTokens returns the current estimated total context tokens.
func (t *UsageTracker) CurrentTokens() int {
	total := t.baselineTokens.Load() + t.deltaTokens.Load()
	if total < 0 {
		return 0
	}
	return int(total)
}
