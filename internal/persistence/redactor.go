package persistence

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// MaxStoredToolOutputBytes defines the hard ceiling (64 KB) for persisted tool output.
const MaxStoredToolOutputBytes = 64 * 1024

var secretPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	// Authorization Bearer tokens
	{regexp.MustCompile(`(?i)bearer\s+[a-zA-Z0-9_\-\.]{15,}`), "Bearer [REDACTED]"},
	// GitHub personal access tokens
	{regexp.MustCompile(`ghp_[a-zA-Z0-9]{20,}`), "ghp_[REDACTED]"},
	// OpenAI API keys (including project/service account prefixes)
	{regexp.MustCompile(`sk-[a-zA-Z0-9_\-]{32,}`), "sk-[REDACTED]"},
	// AWS Access Key IDs
	{regexp.MustCompile(`AKIA[0-9A-Z]{16}`), "AKIA[REDACTED]"},
	// Private Keys (RSA, OpenSSH, EC, etc.)
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`), "[PRIVATE KEY REDACTED]"},
	// Passwords in connection URIs (e.g. postgres://user:pass@host)
	// Restrict strictly to URI userinfo boundaries (no slashes, quotes, whitespace, or JSON delimiters).
	{regexp.MustCompile(`://([^\s/:"'\\{},[\]]+):([^\s/@"'\\{},[\]]+)@`), "://$1:[REDACTED]@"},
	// Common environment variable assignment patterns with sensitive names
	{regexp.MustCompile(`(?i)(api_key|apikey|secret_key|private_key|auth_token|access_token|password)=([^\s"']+)`), "$1=[REDACTED]"},
}

// RedactSecrets scrubs sensitive API keys, tokens, and credentials from text.
func RedactSecrets(input string) string {
	out := input
	for _, p := range secretPatterns {
		out = p.re.ReplaceAllString(out, p.repl)
	}
	return out
}

// RedactJSON parses a JSON string, recursively scrubs secrets from all
// decoded string values, and re-encodes back to JSON to maintain valid JSON syntax.
// If input is not valid JSON, it falls back to RedactSecrets.
func RedactJSON(input string) string {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return input
	}
	var v any
	if err := json.Unmarshal([]byte(trimmed), &v); err != nil {
		return RedactSecrets(input)
	}
	redacted := redactJSONValue(v)
	encoded, err := json.Marshal(redacted)
	if err != nil {
		return RedactSecrets(input)
	}
	return string(encoded)
}

func redactJSONValue(v any) any {
	switch val := v.(type) {
	case string:
		return RedactSecrets(val)
	case map[string]any:
		res := make(map[string]any, len(val))
		for k, item := range val {
			res[k] = redactJSONValue(item)
		}
		return res
	case []any:
		res := make([]any, len(val))
		for i, item := range val {
			res[i] = redactJSONValue(item)
		}
		return res
	default:
		return val
	}
}

// SanitizeToolOutput redacts secrets and clamps tool execution outputs
// to MaxStoredToolOutputBytes safely at UTF-8 rune boundaries.
func SanitizeToolOutput(output string) string {
	redacted := RedactSecrets(output)
	if len(redacted) <= MaxStoredToolOutputBytes {
		return redacted
	}

	cutoff := MaxStoredToolOutputBytes
	for cutoff > 0 && !utf8.RuneStart(redacted[cutoff]) {
		cutoff--
	}

	return redacted[:cutoff] + fmt.Sprintf("\n... [Output truncated for persistence: %d bytes total]", len(redacted))
}
