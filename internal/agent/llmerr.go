package agent

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/openai/openai-go"
)

var (
	// Regex patterns to extract numeric token limits from provider context error messages.
	limitRegexes = []*regexp.Regexp{
		regexp.MustCompile(`(?i)(?:maximum context length is|max context length is|context length of|context window is|model's maximum context length is)\s*(\d+)`),
		regexp.MustCompile(`(?i)(?:limit of|allowed)\s*(\d+)\s*tokens`),
		regexp.MustCompile(`(?i)(\d+)\s*tokens?\s*(?:maximum|max|limit)`),
	}
)

// ParseContextLengthError inspects an error returned from an LLM completion call.
// It returns matched=true if the error indicates a context window overflow (HTTP 400 context_length_exceeded, etc.).
// If a specific token ceiling is parsed from the error message, targetTokens will be > 0.
// If the error is an overflow but no explicit limit is present, targetTokens will be 0 (signaling a 0.75x fallback downscale).
func ParseContextLengthError(err error) (targetTokens int, matched bool) {
	if err == nil {
		return 0, false
	}

	var errMsg string
	var errCode string

	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		errCode = apiErr.Code
		errMsg = apiErr.Message
	}

	if errMsg == "" {
		errMsg = err.Error()
	}

	// First attempt: try regex extraction on error message
	for _, re := range limitRegexes {
		matches := re.FindStringSubmatch(errMsg)
		if len(matches) > 1 {
			if limit, parseErr := strconv.Atoi(matches[1]); parseErr == nil && limit > 0 {
				return limit, true
			}
		}
	}

	lowerMsg := strings.ToLower(errMsg)
	lowerCode := strings.ToLower(errCode)

	// Second attempt: check for explicit error code or key context overflow phrases without explicit limit
	isContextError := lowerCode == "context_length_exceeded" ||
		strings.Contains(lowerMsg, "context_length_exceeded") ||
		strings.Contains(lowerMsg, "maximum context length") ||
		strings.Contains(lowerMsg, "max context length") ||
		strings.Contains(lowerMsg, "context length") ||
		strings.Contains(lowerMsg, "context window") ||
		strings.Contains(lowerMsg, "context limit") ||
		strings.Contains(lowerMsg, "prompt is too long") ||
		strings.Contains(lowerMsg, "exceeds maximum allowed tokens") ||
		strings.Contains(lowerMsg, "prompt tokens exceed") ||
		strings.Contains(lowerMsg, "tokens maximum") ||
		strings.Contains(lowerMsg, "token limit")

	if isContextError {
		return 0, true
	}

	return 0, false
}
