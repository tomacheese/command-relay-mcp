package agent

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"command-relay-mcp/internal/backend"
)

func TestManager_StartTrackAndWait(t *testing.T) {
	b := backend.NewLinuxBackend([]string{"/bin/bash", "-lc"})
	m := NewManager(b, 4<<20, 4<<20, time.Hour)

	rec, err := m.Start(backend.StartOptions{Command: "echo hi; sleep 0.1"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if rec.ID == "" || rec.OSPID == 0 {
		t.Fatalf("record not populated: %+v", rec)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	exitCode, timedOut := rec.Wait(ctx)
	if timedOut {
		t.Fatal("unexpected timeout")
	}
	if exitCode == nil || *exitCode != 0 {
		t.Fatalf("exitCode = %v", exitCode)
	}

	data, _, _ := rec.Stdout.ReadFrom(0, 1024)
	if string(data) != "hi\n" {
		t.Fatalf("stdout = %q", data)
	}

	got, ok := m.Get(rec.ID)
	if !ok || got != rec {
		t.Fatalf("Get did not return the same record")
	}
}

// TestManager_ExitedDoesNotHangOnDescendantHoldingPipesOpen guards
// against a deadlock regression: if a command backgrounds a descendant
// that inherits stdout/stderr and outlives the immediate child, the
// exit-wait goroutine must still reach rec.done via Wait force-closing
// the pipes, rather than blocking forever waiting for the descendant to
// close them on its own.
func TestManager_ExitedDoesNotHangOnDescendantHoldingPipesOpen(t *testing.T) {
	b := backend.NewLinuxBackend([]string{"/bin/bash", "-lc"})
	m := NewManager(b, 4<<20, 4<<20, time.Hour)

	rec, err := m.Start(backend.StartOptions{Command: "echo hi; sleep 3 & exit 0"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	exitCode, timedOut := rec.Wait(ctx)
	if timedOut {
		t.Fatal("Wait timed out: process never reached exited state, likely blocked draining a descendant-held pipe")
	}
	if exitCode == nil || *exitCode != 0 {
		t.Fatalf("exitCode = %v", exitCode)
	}

	data, _, _ := rec.Stdout.ReadFrom(0, 1024)
	if string(data) != "hi\n" {
		t.Fatalf("stdout = %q", data)
	}
}

func TestManager_WaitTimeoutDoesNotKillProcess(t *testing.T) {
	b := backend.NewLinuxBackend([]string{"/bin/bash", "-lc"})
	m := NewManager(b, 4<<20, 4<<20, time.Hour)

	rec, err := m.Start(backend.StartOptions{Command: "sleep 1; echo done"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, timedOut := rec.Wait(ctx)
	if !timedOut {
		t.Fatal("expected timeout")
	}
	if rec.ExitCode() != nil {
		t.Fatal("process should still be running after a wait timeout")
	}

	// It really is still running: waiting again, longer, observes the exit.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	exitCode, timedOut2 := rec.Wait(ctx2)
	if timedOut2 || exitCode == nil || *exitCode != 0 {
		t.Fatalf("exitCode=%v timedOut=%v", exitCode, timedOut2)
	}
}

func TestManager_TerminateEndsProcessGroup(t *testing.T) {
	b := backend.NewLinuxBackend([]string{"/bin/bash", "-lc"})
	m := NewManager(b, 4<<20, 4<<20, time.Hour)

	rec, err := m.Start(backend.StartOptions{Command: "sleep 30"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.Terminate(rec.ID, 500); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, timedOut := rec.Wait(ctx); timedOut {
		t.Fatal("process should have exited after Terminate")
	}
}

func TestManager_TerminateAllEndsEveryRunningProcessTree(t *testing.T) {
	b := backend.NewLinuxBackend([]string{"/bin/bash", "-lc"})
	m := NewManager(b, 4<<20, 4<<20, time.Hour)

	running1, err := m.Start(backend.StartOptions{Command: "sleep 30"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	running2, err := m.Start(backend.StartOptions{Command: "sleep 30"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	alreadyExited, err := m.Start(backend.StartOptions{Command: "true"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, timedOut := alreadyExited.Wait(ctx); timedOut {
		t.Fatal("alreadyExited should have exited on its own")
	}

	m.TerminateAll(500)

	for _, rec := range []*ProcessRecord{running1, running2} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, timedOut := rec.Wait(ctx); timedOut {
			t.Fatalf("process %s should have been terminated by TerminateAll", rec.ID)
		}
	}
}

// TestManager_TerminateAllLogsFailure covers that TerminateAll logs
// its shutdown-time termination errors under the Agent's
// "process-management failure" logging category.
func TestManager_TerminateAllLogsFailure(t *testing.T) {
	m := NewManager(erroringTerminateBackend{backend.NewLinuxBackend([]string{"/bin/bash", "-lc"})}, 4<<20, 4<<20, time.Hour)
	if _, err := m.Start(backend.StartOptions{Command: "sleep 1"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	m.TerminateAll(100)

	if !strings.Contains(logBuf.String(), "process-management failure") {
		t.Fatalf("log output = %q, want it to mention \"process-management failure\"", logBuf.String())
	}
}

// TestManager_StartGCDiscardsOnlyAfterTheTTLElapses covers that a
// finished process must stay reachable via Get (so
// process_read/process_wait keep working) until finishedTTL has
// elapsed, and only then be discarded by GC.
func TestManager_StartGCDiscardsOnlyAfterTheTTLElapses(t *testing.T) {
	const ttl = 100 * time.Millisecond
	b := backend.NewLinuxBackend([]string{"/bin/bash", "-lc"})
	m := NewManager(b, 4<<20, 4<<20, ttl)

	rec, err := m.Start(backend.StartOptions{Command: "true"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, timedOut := rec.Wait(waitCtx); timedOut {
		t.Fatal("process should have exited on its own")
	}

	gcCtx, gcCancel := context.WithCancel(context.Background())
	defer gcCancel()
	m.StartGC(gcCtx, 20*time.Millisecond)

	// Still within the TTL: process_read/process_wait must keep working.
	time.Sleep(ttl / 2)
	if _, ok := m.Get(rec.ID); !ok {
		t.Fatal("finished process was discarded before its TTL elapsed")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := m.Get(rec.ID); !ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("finished process was never discarded after its TTL elapsed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
