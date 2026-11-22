package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var osc8Pattern = regexp.MustCompile(`\x1b\]8;;[^\x1b]*\x1b\\([^\x1b]*)\x1b\]8;;\x1b\\`)

// FormatFileHyperlink formats a file path and optional line number into an OSC 8 terminal hyperlink:
// \x1b]8;;file:///absolute/path#L42\x1b\path:42\x1b]8;;\x1b\
// In dumb terminals or non-interactive environments, it returns a sanitized display string without escape codes.
func FormatFileHyperlink(path string, line int) string {
	if path == "" {
		return ""
	}

	display := path
	if line > 0 {
		display = fmt.Sprintf("%s:%d", path, line)
	}

	if !supportsHyperlinks() {
		return display
	}

	absPath := path
	if !filepath.IsAbs(path) {
		if cwd, err := os.Getwd(); err == nil {
			absPath = filepath.Join(cwd, path)
		}
	}
	absPath = filepath.Clean(absPath)

	targetURI := fmt.Sprintf("file://%s", absPath)
	if line > 0 {
		targetURI = fmt.Sprintf("%s#L%d", targetURI, line)
	}

	return fmt.Sprintf("\x1b]8;;%s\x1b\\%s\x1b]8;;\x1b\\", targetURI, display)
}

// StripHyperlinks removes OSC 8 escape codes from formatted strings, returning pure text.
func StripHyperlinks(s string) string {
	return osc8Pattern.ReplaceAllString(s, "$1")
}

func supportsHyperlinks() bool {
	term := os.Getenv("TERM")
	if term == "" || term == "dumb" {
		return false
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	// Common terminals that support OSC 8 hyperlinks.
	if os.Getenv("TERM_PROGRAM") != "" || os.Getenv("WT_SESSION") != "" ||
		strings.Contains(term, "xterm") || strings.Contains(term, "alacritty") ||
		strings.Contains(term, "kitty") || strings.Contains(term, "screen") ||
		strings.Contains(term, "tmux") {
		return true
	}
	return true
}
