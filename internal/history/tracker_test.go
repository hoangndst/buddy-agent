package history

import (
	"encoding/json"
	"testing"
)

func TestStateTrackerPreservesGitBaseline(t *testing.T) {
	tracker := NewStateTracker("task", nil)
	tracker.SetInitialDirtyPaths([]string{"already-dirty.txt"}, true)
	tracker.RecordFileAccess("buddy.txt", "modified")
	encoded, err := json.Marshal(tracker.ExportData())
	if err != nil {
		t.Fatal(err)
	}
	var data StateTrackerData
	if err := json.Unmarshal(encoded, &data); err != nil {
		t.Fatal(err)
	}
	restored := RestoreStateTracker(data).ExportData()
	if !restored.BaselineKnown || len(restored.InitialDirtyPaths) != 1 {
		t.Fatalf("baseline not restored: %+v", restored)
	}
	if _, ok := restored.InitialDirtyPaths["already-dirty.txt"]; !ok {
		t.Fatal("original dirty path missing")
	}
	if rec, ok := restored.ModifiedFiles["buddy.txt"]; !ok || rec.Action != "modified" {
		t.Fatalf("tracked Buddy edit missing: %+v", restored.ModifiedFiles)
	}
}
