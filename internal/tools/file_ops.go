package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
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
				"description": "Path to the file, relative to workspace.",
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

	targetPath, err := resolvePath(r.workspace, input.Path)
	if err != nil {
		return "", err
	}

	info, err := os.Stat(targetPath)
	if err != nil {
		return "", fmt.Errorf("failed to open file: %w", err)
	}
	if info.IsDir() {
		return fmt.Sprintf("Path %q is a directory. Use list_directory to inspect its contents.", input.Path), nil
	}

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

	reader := bufio.NewReader(file)
	currentLine := 0
	linesRead := 0
	var sb strings.Builder

	for {
		line, readErr := reader.ReadString('\n')
		if len(line) > 0 {
			currentLine++
			if currentLine >= offset {
				if linesRead >= limit {
					sb.WriteString(fmt.Sprintf("\n...[Truncated: further lines omitted, read %d lines]...", limit))
					break
				}
				trimmedLine := strings.TrimRight(line, "\r\n")
				sb.WriteString(fmt.Sprintf("%4d | %s\n", currentLine, trimmedLine))
				linesRead++
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return "", fmt.Errorf("error reading file: %w", readErr)
		}
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

	targetPath, err := resolvePath(w.workspace, input.Path)
	if err != nil {
		return "", err
	}

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

	targetPath, err := resolvePath(e.workspace, input.Path)
	if err != nil {
		return "", err
	}

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

// ListDirectoryTool lists entries in a workspace directory.
type ListDirectoryTool struct {
	workspace string
}

// NewListDirectoryTool creates a new ListDirectoryTool bound to workspace.
func NewListDirectoryTool(workspace string) *ListDirectoryTool {
	return &ListDirectoryTool{workspace: workspace}
}

// Name returns the identifier of the tool.
func (l *ListDirectoryTool) Name() string {
	return "list_directory"
}

// Description returns an explanation of the tool.
func (l *ListDirectoryTool) Description() string {
	return "List files and subdirectories within a directory in the workspace."
}

// Parameters returns the JSON schema definition for tool arguments.
func (l *ListDirectoryTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{
				"type":        "string",
				"description": "Path to the directory, relative to workspace. Defaults to '.' (workspace root).",
				"default":     ".",
			},
		},
	}
}

type listDirInput struct {
	Path string `json:"path"`
}

// Execute returns a formatted listing of the directory contents.
func (l *ListDirectoryTool) Execute(_ context.Context, raw json.RawMessage) (string, error) {
	var input listDirInput
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &input)
	}
	if input.Path == "" {
		input.Path = "."
	}

	targetPath, err := resolvePath(l.workspace, input.Path)
	if err != nil {
		return "", err
	}

	entries, err := os.ReadDir(targetPath)
	if err != nil {
		return "", fmt.Errorf("failed to read directory: %w", err)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Directory %s (%d entries):\n", input.Path, len(entries)))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if entry.IsDir() {
			sb.WriteString(fmt.Sprintf("  [DIR]  %s/\n", entry.Name()))
		} else {
			sb.WriteString(fmt.Sprintf("  [FILE] %s (%d bytes)\n", entry.Name(), info.Size()))
		}
	}
	return sb.String(), nil
}

// SearchFilesTool performs substring or keyword search across files in the workspace.
type SearchFilesTool struct {
	workspace string
}

// NewSearchFilesTool creates a new SearchFilesTool bound to workspace.
func NewSearchFilesTool(workspace string) *SearchFilesTool {
	return &SearchFilesTool{workspace: workspace}
}

// Name returns the identifier of the tool.
func (s *SearchFilesTool) Name() string {
	return "search_files"
}

// Description returns an explanation of the tool.
func (s *SearchFilesTool) Description() string {
	return "Search for a text pattern or keyword across files in the workspace."
}

// Parameters returns the JSON schema definition for tool arguments.
func (s *SearchFilesTool) Parameters() any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "The text pattern or keyword to search for.",
			},
			"path": map[string]any{
				"type":        "string",
				"description": "Subdirectory to restrict search within. Defaults to '.' (entire workspace).",
				"default":     ".",
			},
			"max_results": map[string]any{
				"type":        "integer",
				"description": "Maximum number of matching lines to return. Defaults to 50.",
				"default":     50,
			},
		},
		"required": []string{"query"},
	}
}

type searchFilesInput struct {
	Query      string `json:"query"`
	Path       string `json:"path"`
	MaxResults int    `json:"max_results"`
}

// Execute scans files for the target query string.
func (s *SearchFilesTool) Execute(_ context.Context, raw json.RawMessage) (string, error) {
	var input searchFilesInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(input.Query) == "" {
		return "", errors.New("search query cannot be empty")
	}
	if input.Path == "" {
		input.Path = "."
	}
	maxResults := input.MaxResults
	if maxResults <= 0 || maxResults > 200 {
		maxResults = 50
	}

	startPath, err := resolvePath(s.workspace, input.Path)
	if err != nil {
		return "", err
	}

	absWorkspace, _ := filepath.Abs(s.workspace)
	cleanStart := filepath.Clean(startPath)
	var sb strings.Builder
	count := 0

	walkErr := filepath.Walk(startPath, func(path string, info os.FileInfo, err error) error {
		if err != nil || count >= maxResults {
			return nil
		}
		if info.IsDir() {
			if filepath.Clean(path) != cleanStart {
				name := info.Name()
				if strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" || name == "bin" {
					return filepath.SkipDir
				}
			}
			return nil
		}
		// Skip binary files or large files > 2MB
		if info.Size() > 2*1024*1024 || info.Size() == 0 {
			return nil
		}

		relPath, _ := filepath.Rel(absWorkspace, path)
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer func() { _ = f.Close() }()

		reader := bufio.NewReader(f)
		lineNum := 0
		for {
			line, readErr := reader.ReadString('\n')
			if len(line) > 0 {
				lineNum++
				if strings.Contains(line, input.Query) {
					snippet := extractSnippet(line, input.Query, 300)
					sb.WriteString(fmt.Sprintf("%s:%d: %s\n", relPath, lineNum, snippet))
					count++
					if count >= maxResults {
						return nil
					}
				}
			}
			if readErr != nil {
				if errors.Is(readErr, io.EOF) {
					break
				}
				return fmt.Errorf("reading %s: %w", relPath, readErr)
			}
		}
		return nil
	})
	if walkErr != nil {
		return "", fmt.Errorf("search failed: %w", walkErr)
	}

	if count == 0 {
		return fmt.Sprintf("No matches found for %q in %s", input.Query, input.Path), nil
	}
	return sb.String(), nil
}

// extractSnippet extracts a contextual snippet around query from a line, preserving UTF-8 integrity.
func extractSnippet(line, query string, maxContext int) string {
	idx := strings.Index(line, query)
	if idx < 0 {
		return ""
	}
	if len(line) <= maxContext {
		return strings.TrimSpace(line)
	}

	start := idx - maxContext/2
	if start < 0 {
		start = 0
	} else {
		for start < len(line) && !utf8.RuneStart(line[start]) {
			start++
		}
	}

	end := idx + len(query) + maxContext/2
	if end >= len(line) {
		end = len(line)
	} else {
		for end < len(line) && !utf8.RuneStart(line[end]) {
			end++
		}
	}

	snippet := strings.TrimSpace(line[start:end])
	prefix := ""
	if start > 0 {
		prefix = "..."
	}
	suffix := ""
	if end < len(line) {
		suffix = "..."
	}
	return prefix + snippet + suffix
}

// resolvePath canonicalizes target path and ensures it stays strictly within the workspace.
func resolvePath(workspace, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("path cannot be empty")
	}

	absWorkspace, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("failed to resolve workspace directory: %w", err)
	}
	absWorkspace = filepath.Clean(absWorkspace)

	var target string
	if filepath.IsAbs(path) {
		target = filepath.Clean(path)
	} else {
		target = filepath.Clean(filepath.Join(absWorkspace, path))
	}

	rel, err := filepath.Rel(absWorkspace, target)
	if err != nil || strings.HasPrefix(rel, "..") || rel == ".." {
		return "", fmt.Errorf("access denied: path %q is outside workspace directory", path)
	}

	return target, nil
}
