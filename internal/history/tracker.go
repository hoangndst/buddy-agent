package history

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// StateTracker maintains concrete project invariants, touched file paths,
// and recent errors independently of LLM narrative summaries.
type StateTracker struct {
	mu            sync.RWMutex
	RootGoal      string
	Invariants    []string
	ModifiedFiles map[string]FileRecord
	RecentErrors  []string
}

// NewStateTracker initializes a new StateTracker with the root task goal.
func NewStateTracker(rootGoal string, invariants []string) *StateTracker {
	invCopy := make([]string, len(invariants))
	copy(invCopy, invariants)

	return &StateTracker{
		RootGoal:      rootGoal,
		Invariants:    invCopy,
		ModifiedFiles: make(map[string]FileRecord),
		RecentErrors:  make([]string, 0),
	}
}

// RecordFileAccess records that a file was created, modified, or inspected.
func (st *StateTracker) RecordFileAccess(path, action string) {
	if path == "" {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()

	st.ModifiedFiles[path] = FileRecord{
		Path:      path,
		Action:    action,
		Timestamp: time.Now(),
	}
}

// RecordError appends an error snippet to the recent errors log (capped at 5).
func (st *StateTracker) RecordError(errStr string) {
	trimmed := strings.TrimSpace(errStr)
	if trimmed == "" {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()

	st.RecentErrors = append(st.RecentErrors, trimmed)
	if len(st.RecentErrors) > 5 {
		st.RecentErrors = st.RecentErrors[len(st.RecentErrors)-5:]
	}
}

// SetRootGoal updates the root task objective.
func (st *StateTracker) SetRootGoal(goal string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.RootGoal = goal
}

// AddInvariant appends a critical constraint or invariant.
func (st *StateTracker) AddInvariant(inv string) {
	trimmed := strings.TrimSpace(inv)
	if trimmed == "" {
		return
	}
	st.mu.Lock()
	defer st.mu.Unlock()

	for _, existing := range st.Invariants {
		if existing == trimmed {
			return
		}
	}
	st.Invariants = append(st.Invariants, trimmed)
}

// HasDynamicUpdates returns true if any touched files or recent errors have been recorded.
func (st *StateTracker) HasDynamicUpdates() bool {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return len(st.ModifiedFiles) > 0 || len(st.RecentErrors) > 0
}

// RenderEpisodicState produces a deterministic Markdown document representing
// the persistent state table.
func (st *StateTracker) RenderEpisodicState() string {
	st.mu.RLock()
	defer st.mu.RUnlock()

	var sb strings.Builder

	sb.WriteString("## Task Context & Episodic Progress State\n\n")

	sb.WriteString("### Root Objective\n")
	if st.RootGoal != "" {
		sb.WriteString(st.RootGoal)
		sb.WriteString("\n\n")
	} else {
		sb.WriteString("(None specified)\n\n")
	}

	if len(st.Invariants) > 0 {
		sb.WriteString("### System Invariants & Constraints\n")
		for _, inv := range st.Invariants {
			sb.WriteString(fmt.Sprintf("- %s\n", inv))
		}
		sb.WriteString("\n")
	}

	if len(st.ModifiedFiles) > 0 {
		sb.WriteString("### Touched Files Registry\n")
		sb.WriteString("| File | Action | Last Updated |\n")
		sb.WriteString("| :--- | :--- | :--- |\n")

		// Sort paths for deterministic prompt output
		paths := make([]string, 0, len(st.ModifiedFiles))
		for p := range st.ModifiedFiles {
			paths = append(paths, p)
		}
		sort.Strings(paths)

		for _, p := range paths {
			rec := st.ModifiedFiles[p]
			sb.WriteString(fmt.Sprintf("| `%s` | %s | %s |\n", rec.Path, rec.Action, rec.Timestamp.Format("15:04:05")))
		}
		sb.WriteString("\n")
	}

	if len(st.RecentErrors) > 0 {
		sb.WriteString("### Active Errors & Blockers\n")
		for _, err := range st.RecentErrors {
			sb.WriteString(fmt.Sprintf("- %s\n", err))
		}
		sb.WriteString("\n")
	}

	return strings.TrimSpace(sb.String())
}

// Clone creates an isolated copy of the StateTracker.
func (st *StateTracker) Clone() *StateTracker {
	st.mu.RLock()
	defer st.mu.RUnlock()

	invCopy := make([]string, len(st.Invariants))
	copy(invCopy, st.Invariants)

	errCopy := make([]string, len(st.RecentErrors))
	copy(errCopy, st.RecentErrors)

	filesCopy := make(map[string]FileRecord, len(st.ModifiedFiles))
	for k, v := range st.ModifiedFiles {
		filesCopy[k] = v
	}

	return &StateTracker{
		RootGoal:      st.RootGoal,
		Invariants:    invCopy,
		ModifiedFiles: filesCopy,
		RecentErrors:  errCopy,
	}
}

// StateTrackerData holds serializable state for StateTracker.
type StateTrackerData struct {
	RootGoal      string                `json:"root_goal"`
	Invariants    []string              `json:"invariants"`
	ModifiedFiles map[string]FileRecord `json:"modified_files"`
	RecentErrors  []string              `json:"recent_errors"`
}

// ExportData exports a thread-safe snapshot of the state tracker's contents.
func (st *StateTracker) ExportData() StateTrackerData {
	st.mu.RLock()
	defer st.mu.RUnlock()

	modCopy := make(map[string]FileRecord, len(st.ModifiedFiles))
	for k, v := range st.ModifiedFiles {
		modCopy[k] = v
	}
	invCopy := make([]string, len(st.Invariants))
	copy(invCopy, st.Invariants)
	errCopy := make([]string, len(st.RecentErrors))
	copy(errCopy, st.RecentErrors)

	return StateTrackerData{
		RootGoal:      st.RootGoal,
		Invariants:    invCopy,
		ModifiedFiles: modCopy,
		RecentErrors:  errCopy,
	}
}

// RestoreStateTracker initializes a StateTracker from previously exported data.
func RestoreStateTracker(data StateTrackerData) *StateTracker {
	st := NewStateTracker(data.RootGoal, data.Invariants)
	for k, v := range data.ModifiedFiles {
		st.ModifiedFiles[k] = v
	}
	st.RecentErrors = append(st.RecentErrors, data.RecentErrors...)
	return st
}
