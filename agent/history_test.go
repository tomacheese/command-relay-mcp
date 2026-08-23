package agent

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
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
	// Agent crashed before observing exit; exit_code is allowed to remain
	// NULL forever in this case, so we simply never call RecordEnd.
	got, err := store.Get("exec-2")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ExitCode != nil || got.EndedAt != nil {
		t.Fatalf("got = %+v", got)
	}
}

// TestHistoryStore_PurgeOlderThan covers that executions started before
// the cutoff are deleted, while executions on or after it are kept.
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

func TestHistoryStore_ListOrdersChronologicallyEvenWithZeroNanoseconds(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "history.db")
	store, err := OpenHistoryStore(dbPath)
	if err != nil {
		t.Fatalf("OpenHistoryStore: %v", err)
	}
	defer store.Close()

	// A zero-nanosecond timestamp formats with no fractional part under
	// time.RFC3339Nano, which would otherwise sort before a later
	// timestamp that does have one, under a plain string comparison.
	earlier := ExecutionStart{ExecutionID: "earlier", DeviceID: "pine", Mode: "write", Command: "true", StartedAt: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)}
	later := ExecutionStart{ExecutionID: "later", DeviceID: "pine", Mode: "write", Command: "true", StartedAt: time.Date(2024, 1, 1, 0, 0, 0, 500_000_000, time.UTC)}
	if err := store.RecordStart(earlier); err != nil {
		t.Fatalf("RecordStart(earlier): %v", err)
	}
	if err := store.RecordStart(later); err != nil {
		t.Fatalf("RecordStart(later): %v", err)
	}

	list, err := store.List(10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 || list[0].ExecutionID != "later" || list[1].ExecutionID != "earlier" {
		t.Fatalf("list = %+v, want [later, earlier] (DESC by started_at)", list)
	}
}

// TestOpenHistoryStore_UsesSingleConnection covers Issue #27: the
// underlying *sql.DB pool must be capped at one connection, so
// SQLite's per-connection busy_timeout applies uniformly instead of
// being silently absent on a pool-generated connection.
func TestOpenHistoryStore_UsesSingleConnection(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "history.db")
	store, err := OpenHistoryStore(dbPath)
	if err != nil {
		t.Fatalf("OpenHistoryStore: %v", err)
	}
	defer store.Close()

	stats := store.db.Stats()
	if stats.MaxOpenConnections != 1 {
		t.Fatalf("MaxOpenConnections = %d, want 1", stats.MaxOpenConnections)
	}
}

// TestOpenHistoryStore_BusyTimeoutIsSetOnTheConnection covers Issue
// #27: busy_timeout must be applied via the connection's own DSN, not
// a one-off db.Exec that only reaches whichever connection happened to
// run it first.
func TestOpenHistoryStore_BusyTimeoutIsSetOnTheConnection(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "history.db")
	store, err := OpenHistoryStore(dbPath)
	if err != nil {
		t.Fatalf("OpenHistoryStore: %v", err)
	}
	defer store.Close()

	var busyTimeoutMs int
	if err := store.db.QueryRow("PRAGMA busy_timeout;").Scan(&busyTimeoutMs); err != nil {
		t.Fatalf("query busy_timeout: %v", err)
	}
	if busyTimeoutMs != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000", busyTimeoutMs)
	}
}

// fakeCodedError simulates a driver error implementing the same
// interface modernc.org/sqlite's *sqlite.Error exposes (a Code()
// method), without depending on being able to force a real SQLITE_BUSY
// deterministically in a unit test.
type fakeCodedError struct{ code int }

func (e *fakeCodedError) Error() string { return fmt.Sprintf("fake sqlite error code %d", e.code) }
func (e *fakeCodedError) Code() int     { return e.code }

// TestIsSQLiteBusy_OnlyMatchesBusyCode covers Issue #27's retry-safety
// requirement: isSQLiteBusy must not treat a non-SQLITE_BUSY error
// (e.g. a constraint violation) as retryable.
func TestIsSQLiteBusy_OnlyMatchesBusyCode(t *testing.T) {
	const sqliteBusyCode = 5        // matches the "(5)" in the Issue #27 log line
	const sqliteConstraintCode = 19 // SQLITE_CONSTRAINT, picked as a representative non-busy code

	if !isSQLiteBusy(&fakeCodedError{code: sqliteBusyCode}) {
		t.Fatal("isSQLiteBusy(code=5) = false, want true")
	}
	if isSQLiteBusy(&fakeCodedError{code: sqliteConstraintCode}) {
		t.Fatal("isSQLiteBusy(code=19) = true, want false")
	}
	if isSQLiteBusy(errors.New("some unrelated error")) {
		t.Fatal("isSQLiteBusy(plain error) = true, want false")
	}
}

// TestRecordEnd_ConcurrentWritesDoNotLeaveExecutionsUnfinished covers
// Issue #27's core symptom: many goroutines calling RecordStart then
// RecordEnd concurrently against one HistoryStore must all complete
// without an unrecovered SQLITE_BUSY, leaving no execution with
// ended_at/exit_code still NULL.
func TestRecordEnd_ConcurrentWritesDoNotLeaveExecutionsUnfinished(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "history.db")
	store, err := OpenHistoryStore(dbPath)
	if err != nil {
		t.Fatalf("OpenHistoryStore: %v", err)
	}
	defer store.Close()

	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("exec-%d", i)
			start := ExecutionStart{ExecutionID: id, DeviceID: "pine", Mode: "write", Command: "echo hi", StartedAt: time.Now()}
			if err := store.RecordStart(start); err != nil {
				errs <- fmt.Errorf("RecordStart(%s): %w", id, err)
				return
			}
			exitCode := 0
			if err := store.RecordEnd(id, time.Now(), &exitCode); err != nil {
				errs <- fmt.Errorf("RecordEnd(%s): %w", id, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	list, err := store.List(n + 10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != n {
		t.Fatalf("len(list) = %d, want %d", len(list), n)
	}
	for _, e := range list {
		if e.EndedAt == nil || e.ExitCode == nil {
			t.Fatalf("execution %s left unfinished: ended_at=%v exit_code=%v", e.ExecutionID, e.EndedAt, e.ExitCode)
		}
	}
}
