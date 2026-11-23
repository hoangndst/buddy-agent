package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInputHistoryRingPersistenceNavigationAndLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "history.jsonl")
	h, err := NewInputHistoryRing(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Add("first"); err != nil {
		t.Fatal(err)
	}
	if err := h.Add("first"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < inputHistoryLimit; i++ {
		if err := h.Add(fmt.Sprintf("prompt-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.entries) != inputHistoryLimit {
		t.Fatalf("history has %d entries, want %d", len(h.entries), inputHistoryLimit)
	}
	if got, ok := h.Previous("draft"); !ok || got != "prompt-499" {
		t.Fatalf("previous = %q, %v", got, ok)
	}
	if got, ok := h.Next(); !ok || got != "draft" {
		t.Fatalf("next should restore draft, got %q, %v", got, ok)
	}
	reloaded, err := NewInputHistoryRing(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.entries) != inputHistoryLimit {
		t.Fatalf("reloaded history has %d entries", len(reloaded.entries))
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(contents), "\n"); lines != inputHistoryLimit {
		t.Fatalf("persisted %d history entries, want %d", lines, inputHistoryLimit)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
