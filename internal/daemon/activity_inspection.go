package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
	"github.com/hoangndst/buddy-agent/internal/history"
)

const (
	maxOutputRefBytes   = 1 << 20
	maxOutputStoreBytes = 16 << 20
	maxOutputPageBytes  = 128 << 10
	maxDiffResultBytes  = 48 << 10
)

type outputEntry struct {
	data      string
	total     int64
	truncated bool
	used      time.Time
}

type outputStore struct {
	mu      sync.Mutex
	entries map[string]*outputEntry
	bytes   int
}

func newOutputStore() *outputStore { return &outputStore{entries: make(map[string]*outputEntry)} }

func (s *outputStore) put(value string) (string, int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !utf8.ValidString(value) {
		value = strings.ToValidUTF8(value, "�")
	}
	total := int64(len(value))
	truncated := len(value) > maxOutputRefBytes
	if truncated {
		value = value[:utf8Boundary(value, maxOutputRefBytes)]
	}
	for s.bytes+len(value) > maxOutputStoreBytes {
		oldestID := ""
		var oldest time.Time
		for id, e := range s.entries {
			if oldestID == "" || e.used.Before(oldest) {
				oldestID, oldest = id, e.used
			}
		}
		if oldestID == "" {
			break
		}
		s.bytes -= len(s.entries[oldestID].data)
		delete(s.entries, oldestID)
	}
	id := generateWorkerID("out")
	s.entries[id] = &outputEntry{data: value, total: total, truncated: truncated, used: time.Now()}
	s.bytes += len(value)
	return id, total, truncated
}

func (s *outputStore) read(ref string, offset int64, limit int) (*protocol.OutputReadResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ref == "" || offset < 0 || limit < 0 {
		return nil, errors.New("invalid output reference or offset")
	}
	if limit <= 0 || limit > maxOutputPageBytes {
		limit = maxOutputPageBytes
	}
	e, ok := s.entries[ref]
	if !ok {
		return nil, errors.New("output unavailable; rerun the command with narrower output")
	}
	e.used = time.Now()
	start := int(offset)
	if start > len(e.data) {
		return nil, errors.New("output offset is beyond the available content")
	}
	start = utf8Boundary(e.data, start)
	end := start + limit
	if end > len(e.data) {
		end = len(e.data)
	}
	end = utf8Boundary(e.data, end)
	if end == start && start < len(e.data) {
		_, size := utf8.DecodeRuneInString(e.data[start:])
		end += size
	}
	return &protocol.OutputReadResult{Data: e.data[start:end], TotalBytes: e.total, NextOffset: int64(end), HasMore: end < len(e.data), Truncated: e.truncated}, nil
}

func (s *outputStore) clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = make(map[string]*outputEntry)
	s.bytes = 0
}

func utf8Boundary(value string, index int) int {
	if index > len(value) {
		index = len(value)
	}
	for index > 0 && index < len(value) && !utf8.RuneStart(value[index]) {
		index--
	}
	return index
}

func boundedToolPreview(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:utf8Boundary(value, limit)] + "\n… output available in activity pager"
}

// ReadOutput reads a worker-local tool-output page.
func (w *ConversationWorker) ReadOutput(params protocol.OutputReadParams) (*protocol.OutputReadResult, error) {
	if params.ConversationID == "" || params.ConversationID != w.conversationID {
		return nil, errors.New("conversation ID does not match worker")
	}
	return w.outputStore.read(params.OutputRef, params.Offset, params.Limit)
}

// InspectDiff returns bounded Git changes for the conversation workspace.
func (w *ConversationWorker) InspectDiff(ctx context.Context) (result *protocol.DiffInspectResult, retErr error) {
	started := time.Now()
	defer func() {
		encodedBytes := 0
		if result != nil {
			if data, err := json.Marshal(result); err == nil {
				encodedBytes = len(data)
			}
		}
		outcome := "ok"
		if retErr != nil {
			outcome = "error"
		}
		files := 0
		if result != nil {
			files = len(result.Files)
		}
		log.Printf("buddy diff.inspect duration_ms=%d file_count=%d byte_count=%d outcome=%s", time.Since(started).Milliseconds(), files, encodedBytes, outcome)
	}()
	if w.workspaceDir == "" {
		return nil, errors.New("conversation workspace is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	state := w.runtime.History().Tracker().ExportData()
	return inspectWorkspaceDiff(ctx, w.workspaceDir, state)
}

func inspectWorkspaceDiff(ctx context.Context, workspace string, state history.StateTrackerData) (*protocol.DiffInspectResult, error) {
	changes, omitted, err := gitChanges(ctx, workspace)
	result := &protocol.DiffInspectResult{Files: make([]protocol.DiffFileEntry, 0), OmittedPaths: omitted}
	if err != nil {
		if len(state.ModifiedFiles) == 0 {
			result.Warning = "Git status is unavailable; attribution cannot be determined"
			result.Truncated = true
			return result, nil
		}
		result.Warning = "Git diff is unavailable; showing Buddy tool activity only"
		result.OmittedPaths += len(changes)
		for path := range state.ModifiedFiles {
			if !utf8.ValidString(path) {
				result.OmittedPaths++
				continue
			}
			result.Files = append(result.Files, protocol.DiffFileEntry{Path: path, Diff: "Buddy used a structured file tool; Git attribution is unavailable.", IsBuddyEdit: true, AttributionUnknown: true})
		}
		result.Stats.FilesChanged = len(result.Files)
		boundDiffResult(result)
		return result, nil
	}
	if len(changes) == 0 {
		return result, nil
	}
	base, err := gitDiffBase(ctx, workspace)
	if err != nil {
		result.Warning = "Git diff is unavailable; unable to determine the comparison base"
		result.OmittedPaths += len(changes)
		result.Truncated = true
		return result, nil
	}
	for _, change := range changes {
		if err := ctx.Err(); err != nil {
			result.Warning = "Git inspection timed out; showing partial results"
			result.Truncated = true
			break
		}
		buddy := state.ModifiedFiles[change.path].Action == "modified"
		_, dirtyAtStart := state.InitialDirtyPaths[change.path]
		if change.renameFrom != "" {
			_, sourceDirty := state.InitialDirtyPaths[change.renameFrom]
			dirtyAtStart = dirtyAtStart || sourceDirty
			buddy = buddy || state.ModifiedFiles[change.renameFrom].Action == "modified"
		}
		entry := protocol.DiffFileEntry{Path: change.path, WasDirtyAtStart: dirtyAtStart, IsBuddyEdit: buddy, AttributionUnknown: !state.BaselineKnown || (!dirtyAtStart && !buddy)}
		if !state.BaselineKnown {
			entry.WasDirtyAtStart = false
		}
		data, binary, truncated, diffErr := diffChange(ctx, workspace, base, change)
		if diffErr != nil {
			entry.AttributionUnknown = true
			result.Warning = "Some Git changes could not be read safely"
			entry.Diff = "[Change unavailable: file could not be read safely]"
			entry.Truncated = true
			result.Files = append(result.Files, entry)
			continue
		}
		entry.Diff, entry.IsBinary, entry.Truncated = data, binary, truncated
		countDiffStats(&result.Stats, data)
		result.Files = append(result.Files, entry)
	}
	result.Stats.FilesChanged = len(result.Files)
	boundDiffResult(result)
	return result, nil
}

func boundDiffResult(result *protocol.DiffInspectResult) {
	for len(result.Files) > 0 {
		encoded, _ := json.Marshal(result)
		if len(encoded) <= maxDiffResultBytes {
			break
		}
		result.Truncated = true
		if len(result.Files) > 1 {
			removed := len(result.Files) - (len(result.Files)+1)/2
			result.Files = result.Files[:len(result.Files)-removed]
			result.OmittedPaths += removed
			continue
		}
		last := &result.Files[len(result.Files)-1]
		if len(last.Diff) > 1024 {
			last.Diff = last.Diff[:utf8Boundary(last.Diff, len(last.Diff)/2)] + "\n[diff truncated]"
			last.Truncated = true
		} else {
			result.OmittedPaths++
			result.Files = result.Files[:len(result.Files)-1]
		}
	}
	result.Stats.FilesChanged = len(result.Files)
	result.Stats.Additions, result.Stats.Deletions = 0, 0
	for _, file := range result.Files {
		countDiffStats(&result.Stats, file.Diff)
	}
}

type cappedBuffer struct {
	b         []byte
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	room := b.limit - len(b.b)
	if room > 0 {
		if room > n {
			room = n
		}
		b.b = append(b.b, p[:room]...)
	}
	if room < n {
		b.truncated = true
	}
	return n, nil
}

type gitChange struct {
	path       string
	renameFrom string
}

func gitChangedPaths(ctx context.Context, workspace string) ([]string, int, error) {
	changes, omitted, err := gitChanges(ctx, workspace)
	paths := make([]string, 0, len(changes))
	for _, change := range changes {
		paths = append(paths, change.path)
		if change.renameFrom != "" {
			paths = append(paths, change.renameFrom)
		}
	}
	return paths, omitted, err
}

func gitChanges(ctx context.Context, workspace string) ([]gitChange, int, error) {
	prefixOutput, err := exec.CommandContext(ctx, "git", "-C", workspace, "rev-parse", "--show-prefix").Output()
	if err != nil {
		return nil, 0, err
	}
	prefix := strings.TrimSuffix(string(prefixOutput), "\n")
	cmd := exec.CommandContext(ctx, "git", "-C", workspace, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	buf := &cappedBuffer{limit: 2 << 20}
	cmd.Stdout = buf
	if err := cmd.Run(); err != nil {
		return nil, 0, err
	}
	if buf.truncated {
		return nil, 1, errors.New("git status output exceeded the inspection limit")
	}
	parts := strings.Split(string(buf.b), "\x00")
	changes := make([]gitChange, 0, len(parts))
	omitted := 0
	workspacePath := func(path string) string {
		relative, inside := strings.CutPrefix(path, prefix)
		if !inside {
			return ""
		}
		if !utf8.ValidString(relative) {
			omitted++
			return ""
		}
		return relative
	}
	for i := 0; i < len(parts); i++ {
		item := parts[i]
		if len(item) < 4 {
			continue
		}
		change := gitChange{path: workspacePath(item[3:])}
		if strings.ContainsAny(item[:2], "RC") && i+1 < len(parts) {
			i++
			if strings.Contains(item[:2], "R") {
				change.renameFrom = workspacePath(parts[i])
			}
		}
		if change.path == "" {
			change.path, change.renameFrom = change.renameFrom, ""
		}
		if change.path != "" {
			changes = append(changes, change)
		}
	}
	return changes, omitted, nil
}

func gitDiffBase(ctx context.Context, workspace string) (string, error) {
	base, err := exec.CommandContext(ctx, "git", "-C", workspace, "rev-parse", "--verify", "--quiet", "HEAD").Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		base, err = exec.CommandContext(ctx, "git", "-C", workspace, "hash-object", "-t", "tree", "--stdin").Output()
	}
	return strings.TrimSpace(string(base)), err
}

func safeDiffFile(workspace, path string) (string, os.FileInfo, error) {
	if filepath.IsAbs(path) || filepath.Clean(path) == ".." || strings.HasPrefix(filepath.Clean(path), ".."+string(filepath.Separator)) {
		return "", nil, errors.New("path escapes workspace")
	}
	full := filepath.Join(workspace, filepath.FromSlash(path))
	rel, err := filepath.Rel(workspace, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", nil, errors.New("path escapes workspace")
	}
	info, _ := os.Lstat(full)
	if info != nil && info.Mode()&os.ModeSymlink != 0 {
		return full, info, nil
	}
	if resolved, err := filepath.EvalSymlinks(full); err == nil {
		root, rootErr := filepath.EvalSymlinks(workspace)
		rel, relErr := filepath.Rel(root, resolved)
		if rootErr != nil || relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", nil, errors.New("path resolves outside workspace")
		}
	}
	return full, info, nil
}

func diffChange(ctx context.Context, workspace, base string, change gitChange) (string, bool, bool, error) {
	path := change.path
	full, info, err := safeDiffFile(workspace, path)
	if err != nil {
		return "", false, false, err
	}
	if info != nil && info.Mode()&os.ModeSymlink != 0 {
		return "[Symlink omitted]\n", false, false, nil
	}
	if change.renameFrom == "" && info != nil && info.Mode().IsRegular() && !isTracked(ctx, workspace, path) {
		f, err := os.Open(full)
		if err != nil {
			return "", false, false, err
		}
		defer func() { _ = f.Close() }()
		openedInfo, err := f.Stat()
		if err != nil || !os.SameFile(info, openedInfo) {
			return "", false, false, errors.New("untracked file changed during inspection")
		}
		b, readErr := io.ReadAll(io.LimitReader(f, (64<<10)+1))
		if readErr != nil {
			return "", false, false, readErr
		}
		byteTruncated := len(b) > 64<<10
		if byteTruncated {
			b = b[:utf8Boundary(string(b), 64<<10)]
		}
		if strings.IndexByte(string(b), 0) >= 0 || !utf8.Valid(b) {
			return "[Binary File Omitted]\n", true, false, nil
		}
		text := strings.ToValidUTF8(string(b), "�")
		lines := strings.Split(text, "\n")
		if text == "" || strings.HasSuffix(text, "\n") {
			lines = lines[:len(lines)-1]
		}
		truncated := len(lines) > 500 || byteTruncated
		if len(lines) > 500 {
			lines = lines[:500]
		}
		return "--- /dev/null\n+++ b/" + path + "\n" + prefixLines(lines, "+") + truncationSuffix(truncated), false, truncated, nil
	}
	buf := &cappedBuffer{limit: 256 << 10}
	args := []string{"-C", workspace, "diff", "--no-ext-diff", "--unified=3", "--find-renames", base, "--", ":(literal)" + path}
	if change.renameFrom != "" {
		_, source, err := safeDiffFile(workspace, change.renameFrom)
		if err != nil {
			return "", false, false, err
		}
		if source != nil && source.Mode()&os.ModeSymlink != 0 {
			return "[Symlink omitted]\n", false, false, nil
		}
		args = append(args, ":(literal)"+change.renameFrom)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stdout = buf
	if err := cmd.Run(); err != nil {
		return "", false, false, err
	}
	diff := string(buf.b)
	if buf.truncated && !utf8.ValidString(diff) {
		for !utf8.ValidString(diff) {
			diff = diff[:len(diff)-1]
		}
	}
	if strings.IndexByte(diff, 0) >= 0 || !utf8.ValidString(diff) {
		return "[Binary File Omitted]\n", true, false, nil
	}
	lines := strings.Split(diff, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "Binary files ") && strings.HasSuffix(line, " differ") {
			return "[Binary File Omitted]\n", true, false, nil
		}
	}
	truncated := buf.truncated || len(lines) > 500
	if len(lines) > 500 {
		lines = lines[:500]
	}
	return strings.Join(lines, "\n") + truncationSuffix(truncated), false, truncated, nil
}

func isTracked(ctx context.Context, workspace, path string) bool {
	return exec.CommandContext(ctx, "git", "-C", workspace, "ls-files", "--error-unmatch", "--", ":(literal)"+path).Run() == nil
}
func prefixLines(lines []string, prefix string) string {
	for i := range lines {
		lines[i] = prefix + lines[i]
	}
	return strings.Join(lines, "\n")
}
func truncationSuffix(truncated bool) string {
	if truncated {
		return "\n[diff truncated]"
	}
	return ""
}
func countDiffStats(stats *protocol.DiffStats, diff string) {
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			stats.Additions++
		}
		if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
			stats.Deletions++
		}
	}
}
