package tools

import (
	"fmt"
)

// TruncateOutput bounds text to maxChars, preserving both head and tail context.
// It guarantees that the returned output length never exceeds maxChars.
func TruncateOutput(s string, maxChars int) string {
	if maxChars <= 0 || len(s) <= maxChars {
		return s
	}

	sampleMarker := fmt.Sprintf("\n\n...[TRUNCATED: %d bytes omitted to protect context window]...\n\n", len(s))
	if maxChars <= len(sampleMarker) {
		const fallback = "...[TRUNCATED]..."
		if maxChars <= len(fallback) {
			return fallback[:maxChars]
		}
		rem := maxChars - len(fallback)
		return s[:rem] + fallback
	}

	contentBudget := maxChars - len(sampleMarker)
	headChars := contentBudget / 3
	tailChars := contentBudget - headChars

	omitted := len(s) - (headChars + tailChars)
	marker := fmt.Sprintf("\n\n...[TRUNCATED: %d bytes omitted to protect context window]...\n\n", omitted)

	res := s[:headChars] + marker + s[len(s)-tailChars:]
	if len(res) > maxChars {
		return res[:maxChars]
	}
	return res
}
