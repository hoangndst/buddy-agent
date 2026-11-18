package tools

import (
	"fmt"
)

// TruncateOutput bounds text to maxChars, preserving both head and tail context.
func TruncateOutput(s string, maxChars int) string {
	if maxChars <= 0 || len(s) <= maxChars {
		return s
	}

	half := maxChars / 2
	omitted := len(s) - maxChars
	return fmt.Sprintf("%s\n\n...[TRUNCATED: %d bytes omitted to protect context window]...\n\n%s",
		s[:half], omitted, s[len(s)-half:])
}
