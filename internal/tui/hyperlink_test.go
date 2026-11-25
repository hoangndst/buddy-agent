package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestFormatFileHyperlink(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Run("empty path returns empty string", func(t *testing.T) {
		if got := FormatFileHyperlink("", 0); got != "" {
			t.Fatalf("expected empty string, got %q", got)
		}
	})

	t.Run("dumb terminal returns plain text", func(t *testing.T) {
		t.Setenv("TERM", "dumb")
		got := FormatFileHyperlink("internal/cmd/main.go", 42)
		expected := "internal/cmd/main.go:42"
		if got != expected {
			t.Fatalf("expected %q, got %q", expected, got)
		}
	})

	t.Run("compatible terminal returns OSC 8 escape sequence", func(t *testing.T) {
		t.Setenv("TERM", "xterm-256color")
		got := FormatFileHyperlink("internal/cmd/main.go", 42)
		if !strings.HasPrefix(got, "\x1b]8;;file://") {
			t.Fatalf("expected OSC 8 escape prefix, got %q", got)
		}
		if !strings.Contains(got, "#L42\x1b\\internal/cmd/main.go:42\x1b]8;;\x1b\\") {
			t.Fatalf("expected line fragment and terminator in hyperlink, got %q", got)
		}
	})

	t.Run("without line number", func(t *testing.T) {
		t.Setenv("TERM", "xterm-256color")
		got := FormatFileHyperlink("README.md", 0)
		if strings.Contains(got, "#L") {
			t.Fatalf("hyperlink should not contain line fragment, got %q", got)
		}
		if !strings.Contains(got, "\x1b\\README.md\x1b]8;;\x1b\\") {
			t.Fatalf("expected plain filename display, got %q", got)
		}
	})

	t.Run("with explicit workspace directory", func(t *testing.T) {
		t.Setenv("TERM", "xterm-256color")
		got := FormatWorkspaceFileHyperlink("/custom/workspace", "sub/file.go", 10)
		expectedTarget := "file:///custom/workspace/sub/file.go#L10"
		if !strings.Contains(got, expectedTarget) {
			t.Fatalf("expected target %q in %q", expectedTarget, got)
		}
	})

	t.Run("escapes reserved path characters", func(t *testing.T) {
		t.Setenv("TERM", "xterm-256color")
		got := FormatWorkspaceFileHyperlink("/custom/work space", "notes#draft?100%.md", 10)
		if !strings.Contains(got, "file:///custom/work%20space/notes%23draft%3F100%25.md#L10") {
			t.Fatalf("expected escaped file URI, got %q", got)
		}
	})
}

func TestHyperlinkAtFindsRenderedMarkdownLink(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	rendered, err := RenderMarkdown("[Open Buddy](https://example.com/docs)", 80)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Split(strings.TrimLeft(rendered, "\n"), "\n")[0]
	plain := ansi.Strip(line)
	column := strings.Index(plain, "Open Buddy") + 2
	if got := hyperlinkAt(line, column); got != "https://example.com/docs" {
		t.Fatalf("link at column %d = %q, want URL; output %q", column, got, line)
	}
	if got := hyperlinkAt(line, strings.Index(plain, "Open Buddy")-1); got != "" {
		t.Fatalf("column outside link unexpectedly matched %q", got)
	}
}

func TestStripHyperlinks(t *testing.T) {
	formatted := "\x1b]8;;file:///path/to/file.go#L10\x1b\\file.go:10\x1b]8;;\x1b\\"
	stripped := StripHyperlinks(formatted)
	if stripped != "file.go:10" {
		t.Fatalf("expected 'file.go:10', got %q", stripped)
	}

	plain := "plain text without links"
	if StripHyperlinks(plain) != plain {
		t.Fatalf("expected %q, got %q", plain, StripHyperlinks(plain))
	}
}
