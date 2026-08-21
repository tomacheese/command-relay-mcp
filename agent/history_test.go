package agent

import (
	"path/filepath"
	"testing"
	"time"
)

func TestHistoryStore_RecordAndRetrieve(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "history.db")
	store, err := OpenHistoryStore(dbPath)
	if err != nil {
		t.Fatalf("OpenHistoryStore: %v", err)
	}
	defer store.Close()

	start := ExecutionStart{
		ExecutionID: "exec-1",
		ProcessID:   "proc-1",
		DeviceID:    "pine",
		Mode:        "write",
		Command:     "echo hi",
		Cwd:         "/tmp",
		StartedAt:   time.Now().Truncate(time.Second),
	}
	if err := store.RecordStart(start); err != nil {
		t.Fatalf("RecordStart: %v", err)
	}

	got, err := store.Get("exec-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Command != "echo hi" || got.ExitCode != nil {
		t.Fatalf("got = %+v", got)
	}

	exitCode := 0
	ended := start.StartedAt.Add(time.Second)
	if err := store.RecordEnd("exec-1", ended, &exitCode); err != nil {
		t.Fatalf("RecordEnd: %v", err)
	}

	got2, err := store.Get("exec-1")
	if err != nil {
		t.Fatalf("Get after end: %v", err)
	}
	if got2.ExitCode == nil || *got2.ExitCode != 0 {
		t.Fatalf("exit code = %v", got2.ExitCode)
	}
	if got2.EndedAt == nil {
		t.Fatal("EndedAt not set")
	}

	list, err := store.List(10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ExecutionID != "exec-1" {
		t.Fatalf("list = %+v", list)
	}
}

func TestHistoryStore_UnobservedExitCodeStaysNull(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "history.db")
	store, err := OpenHistoryStore(dbPath)
	if err != nil {
		t.Fatalf("OpenHistoryStore: %v", err)
	}
	defer store.Close()

	start := ExecutionStart{ExecutionID: "exec-2", DeviceID: "pine", Mode: "write", Command: "sleep 999", StartedAt: time.Now()}
	if err := store.RecordStart(start); err != nil {
		t.Fatalf("RecordStart: %v", err)
	}
	// Agent crashed before observing exit; base spec §12.4 allows exit_code
	// to remain NULL forever in this case, so we simply never call RecordEnd.
	got, err := store.Get("exec-2")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ExitCode != nil || got.EndedAt != nil {
		t.Fatalf("got = %+v", got)
	}
}

// TestHistoryStore_PurgeOlderThan covers base spec §23's history_retention
// setting: executions started before the cutoff are deleted, executions
// on or after it are kept.
func TestHistoryStore_PurgeOlderThan(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "history.db")
	store, err := OpenHistoryStore(dbPath)
	if err != nil {
		t.Fatalf("OpenHistoryStore: %v", err)
	}
	defer store.Close()

	now := time.Now()
	old := ExecutionStart{ExecutionID: "old", DeviceID: "pine", Mode: "write", Command: "true", StartedAt: now.Add(-48 * time.Hour)}
	recent := ExecutionStart{ExecutionID: "recent", DeviceID: "pine", Mode: "write", Command: "true", StartedAt: now}
	if err := store.RecordStart(old); err != nil {
		t.Fatalf("RecordStart(old): %v", err)
	}
	if err := store.RecordStart(recent); err != nil {
		t.Fatalf("RecordStart(recent): %v", err)
	}

	if err := store.PurgeOlderThan(now.Add(-24 * time.Hour)); err != nil {
		t.Fatalf("PurgeOlderThan: %v", err)
	}

	if _, err := store.Get("old"); err == nil {
		t.Fatal("old execution survived the purge")
	}
	if _, err := store.Get("recent"); err != nil {
		t.Fatalf("recent execution was purged: %v", err)
	}
}
