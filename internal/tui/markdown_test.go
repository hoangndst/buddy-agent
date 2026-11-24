package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestRenderMarkdown(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("TERM", "dumb")
	rendered, err := RenderMarkdown("# Header\n\n**bold** and `code`\n\n- item", 60)
	if err != nil {
		t.Fatal(err)
	}
	plain := ansi.Strip(rendered)
	for _, want := range []string{"Header", "bold", "code", "item"} {
		if !strings.Contains(plain, want) {
			t.Errorf("rendered Markdown missing %q: %q", want, plain)
		}
	}
	if strings.Contains(plain, "**") || strings.Contains(plain, "`code`") {
		t.Fatalf("no-color output retained Markdown control syntax: %q", plain)
	}
}

func TestRenderMarkdownGolden(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("TERM", "dumb")
	t.Setenv("GLAMOUR_STYLE", "dark")
	got, err := RenderMarkdown("# Buddy\n\nReady **now**.\n\n- Inspect files\n- Explain changes", 40)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "conversation.golden"))
	if err != nil {
		t.Fatal(err)
	}
	if got+"\n" != string(want) {
		t.Fatalf("Markdown golden mismatch\n got: %q\nwant: %q", got+"\n", string(want))
	}
}

func TestRenderMarkdownHighlightsCode(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm-256color")
	rendered, err := RenderMarkdown("```go\nfmt.Println(\"hi\")\n```", 50)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered, "\x1b[") {
		t.Fatalf("syntax highlighted output has no terminal styling: %q", rendered)
	}
}

func TestRenderMarkdownTablesAndCode(t *testing.T) {
	rendered, err := RenderMarkdown("| Name | Value |\n| --- | --- |\n| buddy | 42 |\n\n```go\nfmt.Println(\"hi\")\n```", 50)
	if err != nil {
		t.Fatal(err)
	}
	plain := ansi.Strip(rendered)
	for _, want := range []string{"Name", "buddy", "fmt.Println"} {
		if !strings.Contains(plain, want) {
			t.Errorf("rendered Markdown missing %q: %q", want, plain)
		}
	}
}
