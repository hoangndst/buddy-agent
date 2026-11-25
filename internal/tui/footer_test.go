package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestRenderFooterKeepsComposerAndStatusCompact(t *testing.T) {
	rows := RenderFooter(Footer{
		Width: 80, Height: 5, Queued: 2, Steering: 1, Composer: "› Ask Buddy anything…", State: "ready",
	})
	plain := ansi.Strip(strings.Join(rows, "\n"))
	for _, want := range []string{"2 queued · 1 steering", "Ask Buddy anything…", "Connected"} {
		if !strings.Contains(plain, want) {
			t.Errorf("footer missing %q: %q", want, plain)
		}
	}
}

func TestRenderStandardFooterFramesComposer(t *testing.T) {
	rows := RenderFooter(Footer{Width: 24, Height: 8, Standard: true, Composer: "› Ask Buddy anything…", State: "ready"})
	plain := strings.Split(ansi.Strip(strings.Join(rows, "\n")), "\n")
	if len(plain) < 4 || !strings.HasPrefix(plain[0], "────────────────") || !strings.HasPrefix(plain[2], "────────────────") {
		t.Fatalf("standard composer is not framed by separators: %q", plain)
	}
}

func TestRenderEmergencyApprovalKeepsDecisionRow(t *testing.T) {
	rows := RenderFooter(Footer{Width: 10, Height: 1, Emergency: true, PendingApproval: true, ApprovalKeys: "1 3 4"})
	if len(rows) != 1 || rows[0] != "1 3 4" {
		t.Fatalf("emergency approval rows = %q", rows)
	}
}
