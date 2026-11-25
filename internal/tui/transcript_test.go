package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestTranscriptVisibleWindowCachesAndAnchors(t *testing.T) {
	var transcript Transcript
	for i := range 50 {
		transcript.Append(TranscriptBlock{ID: fmt.Sprintf("block-%d", i), Kind: BlockAssistantMessage, Body: fmt.Sprintf("Message %d", i)}, 80)
	}
	transcript.SetRenderState("", "", false)
	before := strings.Join(transcript.VisibleLines(8, 80), "\n")
	plainBefore := ansi.Strip(before)
	if strings.Contains(plainBefore, "Message 0") || !strings.Contains(plainBefore, "Message 49") {
		t.Fatalf("visible window does not show only transcript tail: %q", before)
	}
	if len(transcript.wrapCache) >= len(transcript.Blocks) {
		t.Fatalf("cached %d wrapped blocks for %d transcript blocks", len(transcript.wrapCache), len(transcript.Blocks))
	}
	if len(transcript.lineCountCache) > 8 {
		t.Fatalf("measured %d blocks for an 8-row viewport", len(transcript.lineCountCache))
	}

	transcript.ScrollOffset = 5
	before = strings.Join(transcript.VisibleLines(8, 80), "\n")
	transcript.Append(TranscriptBlock{ID: "new", Kind: BlockAssistantMessage, Body: "A new response"}, 80)
	after := strings.Join(transcript.VisibleLines(8, 80), "\n")
	if before != after {
		t.Fatalf("appending moved the inspected history\nbefore: %q\nafter: %q", before, after)
	}

	transcript.ScrollOffset = 7
	transcript.Replace([]TranscriptBlock{{ID: "recovered", Kind: BlockAssistantMessage, Body: "Recovered response"}})
	if transcript.ScrollOffset != 7 || len(transcript.Blocks) != 1 || transcript.Blocks[0].ID != "recovered" {
		t.Fatalf("replace changed reading position or failed to install blocks: offset=%d blocks=%+v", transcript.ScrollOffset, transcript.Blocks)
	}
}

func TestTranscriptGroupGrowthPreservesHistoricalAnchor(t *testing.T) {
	var transcript Transcript
	for i := range 10 {
		transcript.Append(TranscriptBlock{ID: fmt.Sprintf("old-%d", i), Kind: BlockAssistantMessage, Body: "old message"}, 80)
	}
	transcript.Append(TranscriptBlock{ID: "group", Kind: BlockActivityGroup, Collapsed: false}, 80)
	transcript.ScrollOffset = 5
	before := transcript.LineCount(transcript.Blocks[len(transcript.Blocks)-1], 80)
	transcript.Append(TranscriptBlock{ID: "call", GroupID: "group", Kind: BlockToolActivity, Header: "Done read_file", Completed: true}, 80)
	after := transcript.LineCount(transcript.Blocks[len(transcript.Blocks)-2], 80)
	if transcript.ScrollOffset != 5+after-before {
		t.Fatalf("group growth moved the historical anchor incorrectly: offset=%d before=%d after=%d", transcript.ScrollOffset, before, after)
	}
}
