package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hoangndst/buddy-agent/internal/memory"
)

func TestRegistryAndDispatch(t *testing.T) {
	reg := NewRegistry(500)
	bash := NewBashTool(t.TempDir(), 5*time.Second)
	reg.Register(bash)

	openaiTools := reg.OpenAITools()
	if len(openaiTools) != 1 {
		t.Fatalf("expected 1 openai tool, got %d", len(openaiTools))
	}
	if openaiTools[0].Function.Name != "bash" {
		t.Errorf("expected bash tool, got %s", openaiTools[0].Function.Name)
	}

	// Dispatch existing tool
	ctx := context.Background()
	out := reg.Dispatch(ctx, "bash", json.RawMessage(`{"command": "echo hello"}`))
	if !strings.Contains(out, "hello") {
		t.Errorf("expected output to contain hello, got: %s", out)
	}

	// Dispatch non-existent tool
	missingOut := reg.Dispatch(ctx, "unknown_tool", json.RawMessage(`{}`))
	if !strings.Contains(missingOut, "not registered") {
		t.Errorf("expected not registered error, got: %s", missingOut)
	}
}

func TestFileOps(t *testing.T) {
	tempDir := t.TempDir()
	ctx := context.Background()

	writeTool := NewWriteFileTool(tempDir)
	readTool := NewReadFileTool(tempDir)
	editTool := NewEditFileTool(tempDir)

	// 1. Write File
	filePath := "test.txt"
	writeArgs, _ := json.Marshal(map[string]string{
		"path":    filePath,
		"content": "line 1\nline 2 target\nline 3",
	})
	if out, err := writeTool.Execute(ctx, writeArgs); err != nil {
		t.Fatalf("write failed: %v, out: %s", err, out)
	}

	// 2. Read File
	readArgs, _ := json.Marshal(map[string]any{
		"path":   filePath,
		"offset": 2,
		"limit":  1,
	})
	readOut, err := readTool.Execute(ctx, readArgs)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if !strings.Contains(readOut, "line 2 target") || strings.Contains(readOut, "line 1") {
		t.Errorf("unexpected read output: %s", readOut)
	}

	// 3. Edit File
	editArgs, _ := json.Marshal(map[string]string{
		"path":        filePath,
		"target":      "line 2 target",
		"replacement": "line 2 replaced",
	})
	if out, err := editTool.Execute(ctx, editArgs); err != nil {
		t.Fatalf("edit failed: %v, out: %s", err, out)
	}

	// 4. Verify edit
	data, _ := os.ReadFile(filepath.Join(tempDir, filePath))
	if !strings.Contains(string(data), "line 2 replaced") {
		t.Errorf("file was not updated properly: %s", string(data))
	}
}

func TestMemoryTools(t *testing.T) {
	tempDir := t.TempDir()
	mgr := memory.NewManager(tempDir)
	ctx := context.Background()

	memTool := NewSaveMemoryTool(mgr)
	skillTool := NewCreateSkillTool(mgr)

	// Test SaveMemory
	factArgs, _ := json.Marshal(map[string]string{"fact": "User prefers tabs over spaces."})
	if _, err := memTool.Execute(ctx, factArgs); err != nil {
		t.Fatalf("save memory failed: %v", err)
	}

	// Test CreateSkill
	skillArgs, _ := json.Marshal(map[string]string{
		"name":         "test-skill",
		"instructions": "Run go test before committing.",
	})
	if _, err := skillTool.Execute(ctx, skillArgs); err != nil {
		t.Fatalf("create skill failed: %v", err)
	}

	skills, err := mgr.LoadSkills()
	if err != nil || len(skills) != 1 {
		t.Fatalf("expected 1 skill loaded, got %d", len(skills))
	}
}
