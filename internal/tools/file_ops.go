package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ReadFileTool reads files with line-range pagination.
type ReadFileTool struct {
	workspace string
}

// NewReadFileTool creates a new ReadFileTool bound to workspace.
func NewReadFileTool(workspace string) *ReadFileTool {
	return &ReadFileTool{workspace: workspace}
}

// Name returns the identifier of the tool.
func (r *ReadFileTool) Name() string {
	return "read_file"
}

// Description returns a human-readable explanation of the tool.
func (r *ReadFileTool) Description() string {
	return "Read contents of a file within the workspace, optionally specifying start line and line limit."
}

// Parameters returns the JSON schema definition for tool arguments.
func (r *ReadFileTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path to the file, relative to workspace or absolute.",
			},
			"offset": map[string]any{
				"type":        "integer",
				"description": "Line number to start reading from (1-based index). Defaults to 1.",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Maximum number of lines to read. Defaults to 500.",
			},
		},
		"required": []string{"path"},
	}
}

type readFileInput struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

// Execute reads the requested line range from the target file.
func (r *ReadFileTool) Execute(_ context.Context, raw json.RawMessage) (string, error) {
	var input readFileInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	targetPath := resolvePath(r.workspace, input.Path)
	file, err := os.Open(targetPath)
	if err != nil {
		return "", fmt.Errorf("failed to open file: %w", err)
	}
	defer func() {
		_ = file.Close()
	}()

	offset := input.Offset
	if offset < 1 {
		offset = 1
	}
	limit := input.Limit
	if limit <= 0 || limit > 2000 {
		limit = 500
	}

	scanner := bufio.NewScanner(file)
	currentLine := 0
	linesRead := 0
	var sb strings.Builder

	for scanner.Scan() {
		currentLine++
		if currentLine < offset {
			continue
		}
		if linesRead >= limit {
			sb.WriteString(fmt.Sprintf("\n...[Truncated: further lines omitted, read %d lines]...", limit))
			break
		}
		sb.WriteString(fmt.Sprintf("%4d | %s\n", currentLine, scanner.Text()))
		linesRead++
	}

	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("error reading file: %w", err)
	}

	if linesRead == 0 && currentLine < offset {
		return fmt.Sprintf("File has only %d lines; offset %d exceeds line count.", currentLine, offset), nil
	}

	return sb.String(), nil
}

// WriteFileTool writes or creates files.
type WriteFileTool struct {
	workspace string
}

// NewWriteFileTool creates a new WriteFileTool bound to workspace.
func NewWriteFileTool(workspace string) *WriteFileTool {
	return &WriteFileTool{workspace: workspace}
}

// Name returns the identifier of the tool.
func (w *WriteFileTool) Name() string {
	return "write_file"
}

// Description returns a human-readable explanation of the tool.
func (w *WriteFileTool) Description() string {
	return "Create or overwrite a file with the given content."
}

// Parameters returns the JSON schema definition for tool arguments.
func (w *WriteFileTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path to the file to create or overwrite.",
			},
			"content": map[string]any{
				"type":        "string",
				"description": "Full file content to write.",
			},
		},
		"required": []string{"path", "content"},
	}
}

type writeFileInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// Execute creates or overwrites the file at the specified path.
func (w *WriteFileTool) Execute(_ context.Context, raw json.RawMessage) (string, error) {
	var input writeFileInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	targetPath := resolvePath(w.workspace, input.Path)
	dir := filepath.Dir(targetPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("failed to create directory: %w", err)
	}

	if err := os.WriteFile(targetPath, []byte(input.Content), 0644); err != nil {
		return "", fmt.Errorf("failed to write file: %w", err)
	}

	return fmt.Sprintf("Successfully wrote %d bytes to %s", len(input.Content), input.Path), nil
}

// EditFileTool performs precise block substitution.
type EditFileTool struct {
	workspace string
}

// NewEditFileTool creates a new EditFileTool bound to workspace.
func NewEditFileTool(workspace string) *EditFileTool {
	return &EditFileTool{workspace: workspace}
}

// Name returns the identifier of the tool.
func (e *EditFileTool) Name() string {
	return "edit_file"
}

// Description returns a human-readable explanation of the tool.
func (e *EditFileTool) Description() string {
	return "Replace a specific substring or code block in a file with new content."
}

// Parameters returns the JSON schema definition for tool arguments.
func (e *EditFileTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path to the file to edit.",
			},
			"target": map[string]any{
				"type":        "string",
				"description": "Exact text to find and replace.",
			},
			"replacement": map[string]any{
				"type":        "string",
				"description": "New text to substitute in place of target.",
			},
		},
		"required": []string{"path", "target", "replacement"},
	}
}

type editFileInput struct {
	Path        string `json:"path"`
	Target      string `json:"target"`
	Replacement string `json:"replacement"`
}

// Execute replaces the target block with the replacement string in the file.
func (e *EditFileTool) Execute(_ context.Context, raw json.RawMessage) (string, error) {
	var input editFileInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if input.Target == "" {
		return "", errors.New("target string cannot be empty")
	}

	targetPath := resolvePath(e.workspace, input.Path)
	data, err := os.ReadFile(targetPath)
	if err != nil {
		return "", fmt.Errorf("failed to read file: %w", err)
	}

	content := string(data)
	if !strings.Contains(content, input.Target) {
		return "", fmt.Errorf("target string not found in %s", input.Path)
	}

	newContent := strings.Replace(content, input.Target, input.Replacement, 1)
	if err := os.WriteFile(targetPath, []byte(newContent), 0644); err != nil {
		return "", fmt.Errorf("failed to save edited file: %w", err)
	}

	return fmt.Sprintf("Successfully replaced target block in %s", input.Path), nil
}

func resolvePath(workspace, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(workspace, path)
}
