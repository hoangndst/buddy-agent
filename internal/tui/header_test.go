package tui

import (
	"strings"
	"testing"
)

func TestRenderHeaderCompactAndStandard(t *testing.T) {
	compact := RenderHeader(Header{Width: 80, Workspace: "/work/buddy", State: "running"})
	if len(compact) != 1 || !strings.Contains(compact[0], "Buddy Agent") || !strings.Contains(compact[0], "running") || !strings.Contains(compact[0], "/work/buddy") {
		t.Fatalf("compact header = %#v", compact)
	}
	standard := RenderHeader(Header{Width: 80, Workspace: "/work/buddy", Model: "test-model", GitBranch: "main", Standard: true})
	if len(standard) != 2 || !strings.Contains(standard[1], "test-model · /work/buddy · main") {
		t.Fatalf("standard header = %#v", standard)
	}
}
