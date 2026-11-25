package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestRenderActivityGolden(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	block := TranscriptBlock{
		Kind:      BlockToolActivity,
		Header:    "Done read_file src/main.go",
		Collapsed: true,
		Completed: true,
		Activity:  &Activity{Elapsed: 800 * time.Millisecond},
	}
	got, _ := RenderBlock(block, nil, 80, "", "", false)
	want, err := os.ReadFile(filepath.Join("testdata", "activity.golden"))
	if err != nil {
		t.Fatal(err)
	}
	if got := ansi.Strip(got) + "\n"; got != string(want) {
		t.Fatalf("activity golden mismatch\n got: %q\nwant: %q", got, string(want))
	}
}

func TestRenderBlockCleansTerminalControls(t *testing.T) {
	block := TranscriptBlock{Kind: BlockAssistantMessage, Body: "safe\x1b[2J text"}
	got, _ := RenderBlock(block, nil, 80, "", "", false)
	if strings.Contains(got, "\x1b[2J") {
		t.Fatalf("transcript retained terminal control sequence: %q", got)
	}
}

func TestRenderBlockCleansMarkdownFallback(t *testing.T) {
	t.Setenv("GLAMOUR_STYLE", "invalid-style")
	block := TranscriptBlock{Kind: BlockAssistantMessage, Body: "safe\x1b[2J text\x07\n\tnext\x00 line"}
	got, _ := RenderBlock(block, nil, 80, "", "", false)
	_, body, _ := strings.Cut(got, "\n")
	if want := "safe text\n\tnext line"; body != want {
		t.Fatalf("fallback body = %q, want %q", body, want)
	}
}

func TestRenderActivityGroupSummaryAndExpansion(t *testing.T) {
	code := 1
	group := TranscriptBlock{ID: "group", Kind: BlockActivityGroup, Collapsed: true}
	children := []TranscriptBlock{
		{ID: "running", GroupID: "group", Kind: BlockToolActivity, Header: "Running read_file src/main.go", Activity: &Activity{Tool: "read_file"}},
		{ID: "failed", GroupID: "group", Kind: BlockToolActivity, Header: "Failed bash", Body: "permission denied", Completed: true, Activity: &Activity{Tool: "bash", ExitCode: &code}},
	}
	blocks := append([]TranscriptBlock{group}, children...)
	collapsed, _ := RenderBlock(group, blocks, 80, "", "", false)
	if !strings.Contains(collapsed, "2 tool calls") || !strings.Contains(collapsed, "read_file, bash") || !strings.Contains(collapsed, "1 running") || !strings.Contains(collapsed, "1 failed") || strings.Contains(collapsed, "permission denied") {
		t.Fatalf("collapsed activity group summary = %q", collapsed)
	}
	group.Collapsed = false
	expanded, _ := RenderBlock(group, append([]TranscriptBlock{group}, children...), 80, "", "", false)
	if !strings.Contains(expanded, "read_file") || !strings.Contains(expanded, "bash") || !strings.Contains(expanded, "permission denied") {
		t.Fatalf("expanded activity group omitted child details: %q", expanded)
	}
}
