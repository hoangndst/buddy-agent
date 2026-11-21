package persistence

import (
	"fmt"
	"regexp"
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
	// OpenAI API keys
	{regexp.MustCompile(`sk-[a-zA-Z0-9]{32,}`), "sk-[REDACTED]"},
	// AWS Access Key IDs
	{regexp.MustCompile(`AKIA[0-9A-Z]{16}`), "AKIA[REDACTED]"},
	// Private Keys (RSA, OpenSSH, EC, etc.)
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`), "[PRIVATE KEY REDACTED]"},
	// Passwords in connection URIs (e.g. postgres://user:pass@host)
	{regexp.MustCompile(`://([^:]+):([^@]+)@`), "://$1:[REDACTED]@"},
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
