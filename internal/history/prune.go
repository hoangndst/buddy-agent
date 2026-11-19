package history

import (
	"fmt"
	"strings"
)

const (
	// DefaultMinPruneBytes defines the payload threshold for micro-pruning (256 bytes).
	DefaultMinPruneBytes = 256
)

// PruneStaleToolOutputs inspects tool results in a TurnBlock and collapses payloads
// exceeding minBytes to lightweight stubs, preserving exit codes and headers.
// Operates on blocks transitioning out of the working tail into intermediate storage
// to protect provider KV prompt cache immutability.
func PruneStaleToolOutputs(b *TurnBlock, minBytes int) (int, int) {
	if b == nil || b.Kind != BlockKindAssistantToolCall || len(b.ToolResults) == 0 {
		return 0, 0
	}
	if minBytes <= 0 {
		minBytes = DefaultMinPruneBytes
	}

	preTokens := EstimateBlock(b)
	prunedCount := 0

	for i := range b.ToolResults {
		tr := &b.ToolResults[i]
		strVal := tr.Content
		if len(strVal) <= minBytes {
			continue
		}

		// Don't prune if already pruned
		if strings.HasPrefix(strVal, "[Output pruned:") {
			continue
		}

		lines := strings.Split(strVal, "\n")
		lineCount := len(lines)

		var exitCodeStr string
		var errorNotice string
		for _, line := range lines {
			lower := strings.ToLower(line)
			if strings.Contains(lower, "exit status") || strings.Contains(lower, "exit code") {
				exitCodeStr = strings.TrimSpace(line)
				break
			}
		}

		// If execution failed, retain last non-empty line as context
		if exitCodeStr != "" && !strings.Contains(exitCodeStr, " 0") {
			for j := len(lines) - 1; j >= 0; j-- {
				trimmed := strings.TrimSpace(lines[j])
				if trimmed != "" {
					errorNotice = fmt.Sprintf(" | Trailing diagnostic: %s", trimmed)
					break
				}
			}
		}

		var stub string
		if exitCodeStr != "" {
			stub = fmt.Sprintf("[Output pruned: %d lines suppressed; %s%s]", lineCount, exitCodeStr, errorNotice)
		} else {
			stub = fmt.Sprintf("[Output pruned: %d lines suppressed%s]", lineCount, errorNotice)
		}

		tr.Content = stub
		prunedCount++
	}

	if prunedCount == 0 {
		return 0, 0
	}

	postTokens := EstimateBlock(b)
	b.EstimatedTokens = postTokens
	reclaimed := preTokens - postTokens
	if reclaimed < 0 {
		reclaimed = 0
	}

	return reclaimed, prunedCount
}
