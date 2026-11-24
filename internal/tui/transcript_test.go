package tui

import (
	"fmt"
	"strings"
	"testing"
)

func TestTranscriptVisibleWindowCachesAndAnchors(t *testing.T) {
	var transcript Transcript
	for i := range 50 {
		transcript.Append(TranscriptBlock{ID: fmt.Sprintf("block-%d", i), Kind: BlockAssistantMessage, Body: fmt.Sprintf("Message %d", i)}, 80)
	}
	transcript.SetRenderState("", "", false)
	before := strings.Join(transcript.VisibleLines(8, 80), "\n")
	if strings.Contains(before, "Message 0") || !strings.Contains(before, "Message 49") {
		t.Fatalf("visible window does not show only transcript tail: %q", before)
	}
	if len(transcript.wrapCache) >= len(transcript.Blocks) {
		t.Fatalf("cached %d wrapped blocks for %d transcript blocks", len(transcript.wrapCache), len(transcript.Blocks))
	}

	transcript.ScrollOffset = 5
	before = strings.Join(transcript.VisibleLines(8, 80), "\n")
	transcript.Append(TranscriptBlock{ID: "new", Kind: BlockAssistantMessage, Body: "A new response"}, 80)
	after := strings.Join(transcript.VisibleLines(8, 80), "\n")
	if before != after {
		t.Fatalf("appending moved the inspected history\nbefore: %q\nafter: %q", before, after)
	}
}
