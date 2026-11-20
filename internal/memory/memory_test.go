package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMemoryManager(t *testing.T) {
	tempDir := t.TempDir()
	mgr := NewManager(tempDir)

	// Test 1: Fallback when SOUL.md does not exist
	prompt, err := mgr.LoadSystemPrompt()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(prompt, "Buddy") || !strings.Contains(prompt, "autonomous agent harness") {
		t.Errorf("expected prompt to contain 'Buddy' and 'autonomous agent harness', got: %s", prompt)
	}

	// Test 2: Create SOUL.md, USER.md, MEMORY.md
	if err := os.WriteFile(filepath.Join(tempDir, "SOUL.md"), []byte("Core Persona Text"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tempDir, "USER.md"), []byte("User Profile Info"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := mgr.AppendMemory("Key architecture note"); err != nil {
		t.Fatalf("failed to append memory: %v", err)
	}

	// Test 3: Save and load skill
	if err := mgr.SaveSkill("git-workflow", "Always use conventional commits."); err != nil {
		t.Fatalf("failed to save skill: %v", err)
	}

	prompt, err = mgr.LoadSystemPrompt()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(prompt, "Core Persona Text") {
		t.Errorf("missing SOUL content in prompt")
	}
	if !strings.Contains(prompt, "User Profile Info") {
		t.Errorf("missing USER content in prompt")
	}
	if !strings.Contains(prompt, "Key architecture note") {
		t.Errorf("missing MEMORY content in prompt")
	}
	if !strings.Contains(prompt, "git-workflow") {
		t.Errorf("missing skill in prompt")
	}
}

func TestSaveSkillPathConfinement(t *testing.T) {
	tempDir := t.TempDir()
	mgr := NewManager(tempDir)

	traversalNames := []string{
		"../outside",
		"../../outside",
		"nested/skill",
		`nested\skill`,
		"../../../etc/passwd",
	}

	for _, name := range traversalNames {
		err := mgr.SaveSkill(name, "malicious instructions")
		if err == nil {
			t.Errorf("expected SaveSkill to fail for traversal name %q", name)
		}
	}
}
