package daemon

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/hoangndst/buddy-agent/internal/daemon/protocol"
	"github.com/hoangndst/buddy-agent/internal/history"
)

func TestOutputStorePagingAndEviction(t *testing.T) {
	store := newOutputStore()
	ref, total, truncated := store.put(strings.Repeat("界", maxOutputRefBytes))
	if !truncated || total <= maxOutputRefBytes {
		t.Fatalf("expected bounded output, total=%d truncated=%v", total, truncated)
	}
	page, err := store.read(ref, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Data) != len("界") || page.NextOffset != int64(len("界")) || !page.HasMore || !page.Truncated {
		t.Fatalf("invalid UTF-8 page: %+v", page)
	}
	for i := 0; i < 16; i++ {
		store.put(strings.Repeat("x", maxOutputRefBytes))
	}
	if _, err := store.read(ref, 0, 1); err == nil || !strings.Contains(err.Error(), "rerun") {
		t.Fatalf("evicted reference error = %v", err)
	}
	store.clear()
	if len(store.entries) != 0 || store.bytes != 0 {
		t.Fatalf("clear retained output: %+v", store)
	}
}

func TestInspectWorkspaceDiffAttribution(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	workspace := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", workspace}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "test@example.invalid")
	git("config", "user.name", "Test")
	for _, path := range []string{"dirty.txt", "buddy.txt", "unknown.txt"} {
		if err := os.WriteFile(filepath.Join(workspace, path), []byte("base\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	git("commit", "-qm", "base")
	for _, path := range []string{"dirty.txt", "buddy.txt", "unknown.txt"} {
		if err := os.WriteFile(filepath.Join(workspace, path), []byte("changed\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	tracker := history.NewStateTracker("task", nil)
	tracker.SetInitialDirtyPaths([]string{"dirty.txt"}, true)
	tracker.RecordFileAccess("buddy.txt", "modified")
	tracker.RecordFileAccess("dirty.txt", "modified")
	result, err := inspectWorkspaceDiff(context.Background(), workspace, tracker.ExportData())
	if err != nil {
		t.Fatal(err)
	}
	byPath := make(map[string]protocol.DiffFileEntry)
	for _, file := range result.Files {
		byPath[file.Path] = file
	}
	if !byPath["dirty.txt"].WasDirtyAtStart || !byPath["dirty.txt"].IsBuddyEdit {
		t.Fatalf("dirty + Buddy attribution = %+v", byPath["dirty.txt"])
	}
	if byPath["buddy.txt"].WasDirtyAtStart || !byPath["buddy.txt"].IsBuddyEdit || byPath["buddy.txt"].AttributionUnknown {
		t.Fatalf("Buddy attribution = %+v", byPath["buddy.txt"])
	}
	if !byPath["unknown.txt"].AttributionUnknown {
		t.Fatalf("unknown attribution = %+v", byPath["unknown.txt"])
	}
}

func TestInspectWorkspaceDiffFromRepositorySubdirectory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	workspace := filepath.Join(root, "nested", "workspace")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	write := func(path, contents string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(contents+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q")
	git("config", "user.email", "test@example.invalid")
	git("config", "user.name", "Test")
	inside := filepath.Join(workspace, "tracked.txt")
	outside := filepath.Join(root, "outside.txt")
	write(inside, "before")
	write(outside, "before")
	git("add", ".")
	git("commit", "-qm", "base")
	write(inside, "dirty at start")
	write(outside, "outside change")
	paths, omitted, err := gitChangedPaths(context.Background(), workspace)
	if err != nil || omitted != 0 || len(paths) != 1 || paths[0] != "tracked.txt" {
		t.Fatalf("workspace baseline paths=%q omitted=%d err=%v", paths, omitted, err)
	}
	tracker := history.NewStateTracker("task", nil)
	tracker.SetInitialDirtyPaths(paths, true)
	tracker.RecordFileAccess("tracked.txt", "modified")
	write(inside, "after")
	result, err := inspectWorkspaceDiff(context.Background(), workspace, tracker.ExportData())
	if err != nil || len(result.Files) != 1 || result.Warning != "" {
		t.Fatalf("workspace diff=%+v err=%v", result, err)
	}
	file := result.Files[0]
	if file.Path != "tracked.txt" || !file.WasDirtyAtStart || !file.IsBuddyEdit || file.AttributionUnknown || !strings.Contains(file.Diff, "+after\n") {
		t.Fatalf("workspace diff entry=%+v", file)
	}
}

func TestInspectWorkspaceDiffIncludesBothSidesOfStagedRename(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	workspace := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", workspace}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "test@example.invalid")
	git("config", "user.name", "Test")
	git("config", "status.renames", "true")
	common := strings.Repeat("unchanged common text\n", 5)
	if err := os.WriteFile(filepath.Join(workspace, "source.txt"), []byte("old line\n"+common), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "source.txt")
	git("commit", "-qm", "base")
	git("mv", "source.txt", "destination.txt")
	if err := os.WriteFile(filepath.Join(workspace, "destination.txt"), []byte("new line\n"+common), 0600); err != nil {
		t.Fatal(err)
	}
	tracker := history.NewStateTracker("task", nil)
	tracker.SetInitialDirtyPaths([]string{"source.txt"}, true)
	tracker.RecordFileAccess("destination.txt", "modified")
	result, err := inspectWorkspaceDiff(context.Background(), workspace, tracker.ExportData())
	if err != nil || len(result.Files) != 1 || result.Warning != "" {
		t.Fatalf("rename diff=%+v err=%v", result, err)
	}
	file := result.Files[0]
	if file.Path != "destination.txt" || !file.WasDirtyAtStart || !file.IsBuddyEdit || file.AttributionUnknown {
		t.Fatalf("rename path and attribution=%+v", file)
	}
	for _, want := range []string{"rename from source.txt\n", "rename to destination.txt\n", "-old line\n", "+new line\n"} {
		if !strings.Contains(file.Diff, want) {
			t.Errorf("rename patch missing %q: %s", want, file.Diff)
		}
	}
	if result.Stats != (protocol.DiffStats{FilesChanged: 1, Deletions: 1, Additions: 1}) {
		t.Fatalf("rename stats=%+v", result.Stats)
	}
}

func TestInspectWorkspaceDiffBeforeFirstCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	workspace := t.TempDir()
	if out, err := exec.Command("git", "-C", workspace, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	path := filepath.Join(workspace, "staged.txt")
	if err := os.WriteFile(path, []byte("staged content\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", workspace, "add", "staged.txt").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v: %s", err, out)
	}
	if err := os.WriteFile(path, []byte("staged content\nworktree edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := inspectWorkspaceDiff(context.Background(), workspace, history.StateTrackerData{})
	if err != nil || len(result.Files) != 1 || result.Warning != "" {
		t.Fatalf("initial repository diff=%+v err=%v", result, err)
	}
	file := result.Files[0]
	if file.Path != "staged.txt" || !strings.Contains(file.Diff, "+staged content\n+worktree edit\n") {
		t.Fatalf("initial repository diff entry=%+v", file)
	}
	if result.Stats != (protocol.DiffStats{FilesChanged: 1, Additions: 2}) {
		t.Fatalf("initial repository diff stats=%+v", result.Stats)
	}
}

func TestInspectWorkspaceDiffBoundsNonGitFallback(t *testing.T) {
	workspace := t.TempDir()
	tracker := history.NewStateTracker("task", nil)
	for i := 0; i < 1000; i++ {
		tracker.RecordFileAccess(strings.Repeat("x", 80)+string(rune('a'+i%26))+strings.Repeat("y", 80)+string(rune(i)), "modified")
	}
	result, err := inspectWorkspaceDiff(context.Background(), workspace, tracker.ExportData())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > maxDiffResultBytes || !result.Truncated || result.OmittedPaths == 0 {
		t.Fatalf("non-Git fallback exceeded cap or omitted no paths: bytes=%d result=%+v", len(encoded), result)
	}
}

func TestDiffPathTruncatesAtUTF8Boundary(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	workspace := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", workspace}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "test@example.invalid")
	git("config", "user.name", "Test")
	path := filepath.Join(workspace, "unicode.txt")
	if err := os.WriteFile(path, []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "unicode.txt")
	git("commit", "-qm", "base")
	if err := os.WriteFile(path, []byte(strings.Repeat("界", 100000)), 0600); err != nil {
		t.Fatal(err)
	}
	diff, binary, truncated, err := diffChange(context.Background(), workspace, "HEAD", gitChange{path: "unicode.txt"})
	if err != nil || binary || !truncated || !utf8.ValidString(diff) {
		t.Fatalf("diff=%q binary=%v truncated=%v err=%v", diff[len(diff)-min(20, len(diff)):], binary, truncated, err)
	}
}

func TestGitChangedPathsAndSafeDiffPath(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	workspace := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", workspace}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q")
	git("config", "user.email", "test@example.invalid")
	git("config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(workspace, "tracked.txt"), []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "tracked.txt")
	git("commit", "-qm", "initial")
	if err := os.WriteFile(filepath.Join(workspace, "tracked.txt"), []byte("Binary files a and b differ\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "new.txt"), []byte("untracked\n"), 0600); err != nil {
		t.Fatal(err)
	}
	paths, omitted, err := gitChangedPaths(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if omitted != 0 || len(paths) != 2 {
		t.Fatalf("paths=%q omitted=%d", paths, omitted)
	}
	for _, path := range paths {
		diff, binary, _, err := diffChange(context.Background(), workspace, "HEAD", gitChange{path: path})
		if err != nil {
			t.Fatal(err)
		}
		if binary || !strings.Contains(diff, path) {
			t.Fatalf("unexpected diff for %s: binary=%v diff=%q", path, binary, diff)
		}
		if path == "tracked.txt" && !strings.Contains(diff, "+Binary files a and b differ\n") {
			t.Fatalf("tracked text did not appear in its diff: %q", diff)
		}
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "linked")); err != nil {
		t.Fatal(err)
	}
	diff, _, _, err := diffChange(context.Background(), workspace, "HEAD", gitChange{path: "linked"})
	if err != nil || !strings.Contains(diff, "Symlink omitted") {
		t.Fatalf("symlink diff=%q err=%v", diff, err)
	}
}
