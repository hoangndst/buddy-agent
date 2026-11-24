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
	if strings.Contains(ansi.Strip(got), "\x1b[2J") {
		t.Fatalf("transcript retained terminal control sequence: %q", got)
	}
}
