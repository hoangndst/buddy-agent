package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestComposerResizeAndCompletion(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "notes.md"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	composer := NewComposer(nil)
	composer.Commands.Register(SlashCommand{Name: "/clear"})
	composer.Commands.Register(SlashCommand{Name: "/copy"})
	if composer.Editor.Height() != 1 {
		t.Fatalf("empty composer height = %d, want 1", composer.Editor.Height())
	}
	composer.SetValue("first line\nsecond line\nthird line", 50, 24)
	if composer.Editor.Height() != 3 {
		t.Fatalf("multiline composer height = %d, want 3", composer.Editor.Height())
	}
	composer.SetValue(strings.Repeat("x", 100), 50, 24)
	if composer.Editor.Height() != 3 {
		t.Fatalf("wrapped composer height = %d, want capped height 3", composer.Editor.Height())
	}
	composer.SetValue("/cl", 80, 24)
	composer.UpdateCompletion(workspace)
	if composer.CompletionKind() != "command" || len(composer.CompletionCommands()) != 1 {
		t.Fatalf("slash completion = %q, %+v", composer.CompletionKind(), composer.CompletionCommands())
	}
	if !composer.AcceptCompletion(false) || composer.Editor.Value() != "/clear" {
		t.Fatalf("slash completion was not accepted: %q", composer.Editor.Value())
	}
	composer.SetValue("@not", 80, 24)
	composer.UpdateCompletion(workspace)
	if composer.CompletionKind() != "file" || len(composer.CompletionFiles()) != 1 || composer.CompletionFiles()[0] != "notes.md" {
		t.Fatalf("file completion = %q, %+v", composer.CompletionKind(), composer.CompletionFiles())
	}
	if !composer.AcceptCompletion(false) || composer.Editor.Value() != "notes.md" {
		t.Fatalf("file completion was not accepted: %q", composer.Editor.Value())
	}
}

func TestComposerHistoryRestoresDraft(t *testing.T) {
	history, err := NewInputHistoryRing(filepath.Join(t.TempDir(), "history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	composer := NewComposer(history)
	if err := composer.AddHistory("older"); err != nil {
		t.Fatal(err)
	}
	if err := composer.AddHistory("newer"); err != nil {
		t.Fatal(err)
	}
	if got, ok := composer.Previous("draft"); !ok || got != "newer" {
		t.Fatalf("history previous = %q, %t; want newer", got, ok)
	}
	if got, ok := composer.Next(); !ok || got != "draft" {
		t.Fatalf("history next = %q, %t; want restored draft", got, ok)
	}
}
