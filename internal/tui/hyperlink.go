package tui

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

var osc8Pattern = regexp.MustCompile(`\x1b\]8;[^;]*;[^\x07\x1b]*(\x1b\\|\x07)([^\x07\x1b]*)\x1b\]8;;(\x1b\\|\x07)`)
var osc8SegmentPattern = regexp.MustCompile(`\x1b\]8;[^;]*;([^\x07\x1b]*)(\x1b\\|\x07)(.*?)\x1b\]8;;(\x1b\\|\x07)`)

// FormatFileHyperlink formats a file path and optional line number into an OSC 8 terminal hyperlink:
// \x1b]8;;file:///absolute/path#L42\x1b\path:42\x1b]8;;\x1b\
// In dumb terminals or non-interactive environments, it returns a sanitized display string without escape codes.
func FormatFileHyperlink(path string, line int) string {
	return FormatWorkspaceFileHyperlink("", path, line)
}

// FormatWorkspaceFileHyperlink formats a file path relative to a workspace and optional line number into an OSC 8 terminal hyperlink.
// When path is relative and workspace is specified, it is resolved against workspace; otherwise it falls back to the current working directory.
func FormatWorkspaceFileHyperlink(workspace, path string, line int) string {
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
		baseDir := workspace
		if baseDir == "" {
			if cwd, err := os.Getwd(); err == nil {
				baseDir = cwd
			}
		}
		if baseDir != "" {
			absPath = filepath.Join(baseDir, path)
		}
	}
	absPath = filepath.Clean(absPath)

	targetURI := (&url.URL{Scheme: "file", Path: absPath}).String()
	if line > 0 {
		targetURI = fmt.Sprintf("%s#L%d", targetURI, line)
	}

	return fmt.Sprintf("\x1b]8;;%s\x1b\\%s\x1b]8;;\x1b\\", targetURI, display)
}

// StripHyperlinks removes OSC 8 escape codes from formatted strings, returning pure text.
func StripHyperlinks(s string) string {
	return osc8Pattern.ReplaceAllString(s, "$2")
}

func hyperlinkAt(line string, column int) string {
	for _, match := range osc8SegmentPattern.FindAllStringSubmatchIndex(line, -1) {
		start := lipgloss.Width(ansi.Strip(line[:match[0]]))
		label := line[match[6]:match[7]]
		if column >= start && column < start+lipgloss.Width(ansi.Strip(label)) {
			return line[match[2]:match[3]]
		}
	}
	return ""
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
