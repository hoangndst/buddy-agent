package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hoangndst/buddy-agent/internal/memory"
)

type dummyTool struct {
	name        string
	description string
	params      any
}

func (d *dummyTool) Name() string        { return d.name }
func (d *dummyTool) SetName(name string) { d.name = name }
func (d *dummyTool) Description() string { return d.description }
func (d *dummyTool) Parameters() any     { return d.params }
func (d *dummyTool) Execute(_ context.Context, _ json.RawMessage) (string, error) {
	return "ok", nil
}

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

func TestRegistry_CollisionHandling(t *testing.T) {
	reg := NewRegistry(1000)
	t1 := &dummyTool{name: "duplicate_tool", description: "first tool"}
	t2 := &dummyTool{name: "duplicate_tool", description: "second tool"}

	reg.Register(t1)
	reg.Register(t2)

	toolsList := reg.OpenAITools()
	if len(toolsList) != 2 {
		t.Fatalf("expected both tools to be preserved in registry, got %d", len(toolsList))
	}

	names := []string{toolsList[0].Function.Name, toolsList[1].Function.Name}
	if names[0] == names[1] {
		t.Fatalf("tools must have distinct names after collision handling, got %v", names)
	}
}

func TestDeterministicToolOrderingAndSerialization(t *testing.T) {
	reg := NewRegistry(1000)
	reg.Register(&dummyTool{name: "zebra", description: "zebra tool"})
	reg.Register(&dummyTool{name: "alpha", description: "alpha tool"})
	reg.Register(&dummyTool{name: "beta", description: "beta tool"})

	toolsList := reg.OpenAITools()
	if len(toolsList) != 3 {
		t.Fatalf("expected 3 tools, got %d", len(toolsList))
	}

	if toolsList[0].Function.Name != "alpha" ||
		toolsList[1].Function.Name != "beta" ||
		toolsList[2].Function.Name != "zebra" {
		t.Errorf("tools are not sorted alphabetically: %s, %s, %s",
			toolsList[0].Function.Name, toolsList[1].Function.Name, toolsList[2].Function.Name)
	}

	// Verify byte-for-byte serialization idempotency
	firstJSON, err := json.Marshal(toolsList)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	for i := 0; i < 10; i++ {
		repeatTools := reg.OpenAITools()
		repeatJSON, err := json.Marshal(repeatTools)
		if err != nil {
			t.Fatalf("repeat marshal failed: %v", err)
		}
		if !bytes.Equal(firstJSON, repeatJSON) {
			t.Fatalf("iteration %d produced divergent tool JSON bytes", i)
		}
	}
}

func TestCanonicalizeParameters(t *testing.T) {
	// 1. Nil parameter
	nilParams := CanonicalizeParameters(nil)
	if nilParams["type"] != "object" || nilParams["properties"] == nil {
		t.Errorf("expected default object schema for nil input, got: %v", nilParams)
	}

	// 2. Struct parameter
	type customSchema struct {
		Type       string                 `json:"type"`
		Properties map[string]interface{} `json:"properties"`
	}
	s := customSchema{
		Type: "object",
		Properties: map[string]interface{}{
			"query": map[string]string{"type": "string"},
		},
	}
	structParams := CanonicalizeParameters(s)
	if structParams["type"] != "object" {
		t.Errorf("expected type object, got: %v", structParams["type"])
	}

	// 3. Map without type
	rawMap := map[string]any{
		"properties": map[string]any{
			"count": map[string]any{"type": "integer"},
		},
	}
	mapParams := CanonicalizeParameters(rawMap)
	if mapParams["type"] != "object" {
		t.Errorf("expected type to default to object, got: %v", mapParams["type"])
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

func TestFileOpsPathConfinement(t *testing.T) {
	tempDir := t.TempDir()
	ctx := context.Background()

	writeTool := NewWriteFileTool(tempDir)
	readTool := NewReadFileTool(tempDir)
	editTool := NewEditFileTool(tempDir)

	traversalPaths := []string{
		"../outside.txt",
		"../../outside.txt",
		"/tmp/buddy_test_outside.txt",
		"nested/../../outside.txt",
	}

	for _, p := range traversalPaths {
		writeArgs, _ := json.Marshal(map[string]string{
			"path":    p,
			"content": "illegal content",
		})
		if _, err := writeTool.Execute(ctx, writeArgs); err == nil {
			t.Errorf("expected write to fail for path %q", p)
		}

		readArgs, _ := json.Marshal(map[string]string{
			"path": p,
		})
		if _, err := readTool.Execute(ctx, readArgs); err == nil {
			t.Errorf("expected read to fail for path %q", p)
		}

		editArgs, _ := json.Marshal(map[string]string{
			"path":        p,
			"target":      "foo",
			"replacement": "bar",
		})
		if _, err := editTool.Execute(ctx, editArgs); err == nil {
			t.Errorf("expected edit to fail for path %q", p)
		}
	}
}

func TestReadFileLongLines(t *testing.T) {
	tempDir := t.TempDir()
	ctx := context.Background()
	readTool := NewReadFileTool(tempDir)

	// Create a file with a single line > 128KB (default bufio.Scanner limit is 64KB)
	longLine := strings.Repeat("x", 128*1024)
	testFile := "longline.txt"
	if err := os.WriteFile(filepath.Join(tempDir, testFile), []byte(longLine+"\n"), 0644); err != nil {
		t.Fatal(err)
	}

	readArgs, _ := json.Marshal(map[string]any{
		"path": testFile,
	})
	out, err := readTool.Execute(ctx, readArgs)
	if err != nil {
		t.Fatalf("expected read of long line to succeed, got error: %v", err)
	}
	if !strings.Contains(out, "xxxx") {
		t.Errorf("expected output to contain file content")
	}
}

func TestBashTool_ExecutionAndOptions(t *testing.T) {
	tempDir := t.TempDir()
	bash := NewBashTool(tempDir, 5*time.Second, WithMaxOutputBytes(8000))

	if bash.maxOutputBytes != 8000 {
		t.Errorf("expected maxOutputBytes 8000, got %d", bash.maxOutputBytes)
	}

	ctx := context.Background()

	// Empty command error
	_, err := bash.Execute(ctx, json.RawMessage(`{"command": ""}`))
	if err == nil {
		t.Errorf("expected error on empty command")
	}

	// Normal command execution
	res, err := bash.Execute(ctx, json.RawMessage(`{"command": "echo 'running bash tool'"}`))
	if err != nil {
		t.Fatalf("unexpected bash execution error: %v", err)
	}
	if !strings.Contains(res, "running bash tool") {
		t.Errorf("expected output to contain command message, got: %s", res)
	}
	if !strings.Contains(res, "Exit Code: 0") {
		t.Errorf("expected output to contain Exit Code: 0, got: %s", res)
	}
}

