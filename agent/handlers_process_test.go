package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"command-relay-mcp/internal/backend"
	"command-relay-mcp/internal/proto"
)

func newTestProcessHandlers(t *testing.T) *ProcessHandlers {
	mgr := NewManager(backend.NewLinuxBackend([]string{"/bin/bash", "-lc"}), 4<<20, 4<<20, time.Hour)
	hist, err := OpenHistoryStore(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("OpenHistoryStore: %v", err)
	}
	t.Cleanup(func() { hist.Close() })
	return NewProcessHandlers(mgr, nil, hist, "pine")
}

func TestProcessLifecycle_StartReadWriteWaitTerminate(t *testing.T) {
	h := newTestProcessHandlers(t)

	startParams, _ := json.Marshal(ProcessStartParams{Command: "cat"})
	startResultAny, rpcErr := h.Start(context.Background(), startParams)
	if rpcErr != nil {
		t.Fatalf("Start: %+v", rpcErr)
	}
	startResult := startResultAny.(ProcessStartResult)

	writeParams, _ := json.Marshal(ProcessWriteParams{ProcessID: startResult.ProcessID, Data: "ping\n"})
	if _, rpcErr := h.Write(context.Background(), writeParams); rpcErr != nil {
		t.Fatalf("Write: %+v", rpcErr)
	}

	var readResult ProcessReadResult
	for i := 0; i < 20; i++ {
		readParams, _ := json.Marshal(ProcessReadParams{ProcessID: startResult.ProcessID, MaxBytes: 1024})
		readResultAny, rpcErr := h.Read(context.Background(), readParams)
		if rpcErr != nil {
			t.Fatalf("Read: %+v", rpcErr)
		}
		readResult = readResultAny.(ProcessReadResult)
		if readResult.Stdout == "ping\n" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if readResult.Stdout != "ping\n" {
		t.Fatalf("stdout = %q", readResult.Stdout)
	}

	termParams, _ := json.Marshal(ProcessTerminateParams{ProcessID: startResult.ProcessID, GraceMs: 500})
	if _, rpcErr := h.Terminate(context.Background(), termParams); rpcErr != nil {
		t.Fatalf("Terminate: %+v", rpcErr)
	}

	waitParams, _ := json.Marshal(ProcessWaitParams{ProcessID: startResult.ProcessID, TimeoutMs: 2000})
	waitResultAny, rpcErr := h.Wait(context.Background(), waitParams)
	if rpcErr != nil {
		t.Fatalf("Wait: %+v", rpcErr)
	}
	waitResult := waitResultAny.(ProcessWaitResult)
	if waitResult.TimedOut {
		t.Fatal("expected process to have exited after Terminate")
	}
}

// TestProcessStart_RecordsExecutionEndOnExit covers that a
// process.start execution's history row must get its ended_at/exit_code
// filled in once the process actually exits, not stay open forever.
func TestProcessStart_RecordsExecutionEndOnExit(t *testing.T) {
	mgr := NewManager(backend.NewLinuxBackend([]string{"/bin/bash", "-lc"}), 4<<20, 4<<20, time.Hour)
	hist, err := OpenHistoryStore(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("OpenHistoryStore: %v", err)
	}
	t.Cleanup(func() { hist.Close() })
	h := NewProcessHandlers(mgr, nil, hist, "pine")

	startParams, _ := json.Marshal(ProcessStartParams{Command: "true"})
	startResultAny, rpcErr := h.Start(context.Background(), startParams)
	if rpcErr != nil {
		t.Fatalf("Start: %+v", rpcErr)
	}
	startResult := startResultAny.(ProcessStartResult)

	deadline := time.Now().Add(2 * time.Second)
	for {
		list, err := hist.List(10)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		var found *Execution
		for i := range list {
			if list[i].ProcessID == startResult.ProcessID {
				found = &list[i]
				break
			}
		}
		if found != nil && found.EndedAt != nil {
			if found.ExitCode == nil || *found.ExitCode != 0 {
				t.Fatalf("execution = %+v", found)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("execution end was never recorded: %+v", found)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestProcessStart_RecordsClientContextAndSubject(t *testing.T) {
	mgr := NewManager(backend.NewLinuxBackend([]string{"/bin/bash", "-lc"}), 4<<20, 4<<20, time.Hour)
	hist, err := OpenHistoryStore(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("OpenHistoryStore: %v", err)
	}
	defer hist.Close()
	h := NewProcessHandlers(mgr, nil, hist, "pine")

	params, _ := json.Marshal(ProcessStartParams{
		Command: "true", ClientContextID: "sess-xyz", ClientSubject: "user-456",
	})
	_, rpcErr := h.Start(context.Background(), params)
	if rpcErr != nil {
		t.Fatalf("rpcErr: %+v", rpcErr)
	}

	list, err := hist.List(10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].ClientContextID != "sess-xyz" || list[0].ClientSubject != "user-456" {
		t.Fatalf("execution = %+v", list)
	}
}

func TestProcessRead_UnknownProcessIsNotFound(t *testing.T) {
	h := newTestProcessHandlers(t)
	readParams, _ := json.Marshal(ProcessReadParams{ProcessID: "does-not-exist"})
	_, rpcErr := h.Read(context.Background(), readParams)
	if rpcErr == nil {
		t.Fatal("expected process_not_found error")
	}
}

// erroringStdinHandle wraps a real backend.ProcessHandle but fails every
// Stdin() write, simulating a broken pipe without depending on OS timing.
type erroringStdinHandle struct {
	backend.ProcessHandle
}

func (erroringStdinHandle) Stdin() io.WriteCloser { return failingWriteCloser{} }

type failingWriteCloser struct{}

func (failingWriteCloser) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }
func (failingWriteCloser) Close() error              { return nil }

type erroringStdinBackend struct{ inner backend.ProcessBackend }

func (b erroringStdinBackend) Start(opts backend.StartOptions) (backend.ProcessHandle, error) {
	h, err := b.inner.Start(opts)
	if err != nil {
		return nil, err
	}
	return erroringStdinHandle{h}, nil
}

// TestProcessWrite_LogsProcessManagementFailure covers the Agent's
// "unexpected process-management failure" logging category for
// process.write's stdin-write failure path.
func TestProcessWrite_LogsProcessManagementFailure(t *testing.T) {
	mgr := NewManager(erroringStdinBackend{backend.NewLinuxBackend([]string{"/bin/bash", "-lc"})}, 4<<20, 4<<20, time.Hour)
	hist, err := OpenHistoryStore(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("OpenHistoryStore: %v", err)
	}
	defer hist.Close()
	h := NewProcessHandlers(mgr, nil, hist, "pine")

	startResultAny, rpcErr := h.Start(context.Background(), mustMarshal(t, ProcessStartParams{Command: "cat"}))
	if rpcErr != nil {
		t.Fatalf("Start: %+v", rpcErr)
	}
	startResult := startResultAny.(ProcessStartResult)

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	_, rpcErr = h.Write(context.Background(), mustMarshal(t, ProcessWriteParams{ProcessID: startResult.ProcessID, Data: "x"}))
	if rpcErr == nil || rpcErr.Code != proto.ErrInternal {
		t.Fatalf("rpcErr = %+v, want internal_error", rpcErr)
	}
	if !strings.Contains(logBuf.String(), "process-management failure") {
		t.Fatalf("log output = %q, want it to mention \"process-management failure\"", logBuf.String())
	}
}

// erroringTerminateHandle wraps a real backend.ProcessHandle but fails
// Terminate, simulating a real OS-level termination failure distinct
// from process_not_found.
type erroringTerminateHandle struct {
	backend.ProcessHandle
}

func (erroringTerminateHandle) Terminate(graceMs int) error { return errors.New("kill failed") }

type erroringTerminateBackend struct{ inner backend.ProcessBackend }

func (b erroringTerminateBackend) Start(opts backend.StartOptions) (backend.ProcessHandle, error) {
	h, err := b.inner.Start(opts)
	if err != nil {
		return nil, err
	}
	return erroringTerminateHandle{h}, nil
}

// TestProcessTerminate_RealFailureLogsAndReturnsInternal covers that a
// genuine termination failure must not be misclassified as
// process_not_found, and must be logged under the "unexpected
// process-management failure" category.
func TestProcessTerminate_RealFailureLogsAndReturnsInternal(t *testing.T) {
	mgr := NewManager(erroringTerminateBackend{backend.NewLinuxBackend([]string{"/bin/bash", "-lc"})}, 4<<20, 4<<20, time.Hour)
	hist, err := OpenHistoryStore(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("OpenHistoryStore: %v", err)
	}
	defer hist.Close()
	h := NewProcessHandlers(mgr, nil, hist, "pine")

	startResultAny, rpcErr := h.Start(context.Background(), mustMarshal(t, ProcessStartParams{Command: "cat"}))
	if rpcErr != nil {
		t.Fatalf("Start: %+v", rpcErr)
	}
	startResult := startResultAny.(ProcessStartResult)

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	_, rpcErr = h.Terminate(context.Background(), mustMarshal(t, ProcessTerminateParams{ProcessID: startResult.ProcessID}))
	if rpcErr == nil || rpcErr.Code != proto.ErrInternal {
		t.Fatalf("rpcErr = %+v, want internal_error (not process_not_found)", rpcErr)
	}
	if !strings.Contains(logBuf.String(), "process-management failure") {
		t.Fatalf("log output = %q, want it to mention \"process-management failure\"", logBuf.String())
	}
}

// unobservableExitHandle wraps a real backend.ProcessHandle but reports
// its Wait() as failed (exit code unobservable — e.g. the OS signal that
// killed it couldn't be translated to a code), while the process itself
// has genuinely exited.
type unobservableExitHandle struct {
	backend.ProcessHandle
}

func (unobservableExitHandle) Wait() backend.ExitResult {
	return backend.ExitResult{Err: errors.New("wait: signal: killed")}
}

type unobservableExitBackend struct{ inner backend.ProcessBackend }

func (b unobservableExitBackend) Start(opts backend.StartOptions) (backend.ProcessHandle, error) {
	h, err := b.inner.Start(opts)
	if err != nil {
		return nil, err
	}
	return unobservableExitHandle{h}, nil
}

// TestProcessList_ReportsExitedEvenWithUnobservableExitCode covers base
// spec §8.2: state must come from runtime inspection (the process really
// did exit), not from ExitCode() being non-nil — which stays nil forever
// whenever the backend couldn't translate the exit to a code.
func TestProcessList_ReportsExitedEvenWithUnobservableExitCode(t *testing.T) {
	mgr := NewManager(unobservableExitBackend{backend.NewLinuxBackend([]string{"/bin/bash", "-lc"})}, 4<<20, 4<<20, time.Hour)
	hist, err := OpenHistoryStore(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("OpenHistoryStore: %v", err)
	}
	defer hist.Close()
	h := NewProcessHandlers(mgr, nil, hist, "pine")

	startResultAny, rpcErr := h.Start(context.Background(), mustMarshal(t, ProcessStartParams{Command: "true"}))
	if rpcErr != nil {
		t.Fatalf("Start: %+v", rpcErr)
	}
	startResult := startResultAny.(ProcessStartResult)

	rec, ok := mgr.Get(startResult.ProcessID)
	if !ok {
		t.Fatal("process record not found")
	}
	deadline := time.Now().Add(2 * time.Second)
	for !rec.Exited() {
		if time.Now().After(deadline) {
			t.Fatal("process never exited")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rec.ExitCode() != nil {
		t.Fatalf("ExitCode() = %v, want nil (unobservable)", rec.ExitCode())
	}

	listResultAny, rpcErr := h.List(context.Background(), mustMarshal(t, struct{}{}))
	if rpcErr != nil {
		t.Fatalf("List: %+v", rpcErr)
	}
	list := listResultAny.(ProcessListResult)
	var found *ProcessSummary
	for i := range list.Processes {
		if list.Processes[i].ProcessID == startResult.ProcessID {
			found = &list.Processes[i]
		}
	}
	if found == nil {
		t.Fatalf("process %s not in list", startResult.ProcessID)
	}
	if found.State != "exited" {
		t.Fatalf("State = %q, want \"exited\"", found.State)
	}
}

// TestProcessHandlers_TracksProcessStartedUnderSandboxManager covers base
// spec §8.5: a command_read call that times out leaves its process
// running under the sandbox Manager, not the plain one — process_read/
// write/wait/terminate/list must still find it there.
func TestProcessHandlers_TracksProcessStartedUnderSandboxManager(t *testing.T) {
	mgr := NewManager(backend.NewLinuxBackend([]string{"/bin/bash", "-lc"}), 4<<20, 4<<20, time.Hour)
	sandboxMgr := NewManager(backend.NewLinuxBackend([]string{"/bin/bash", "-lc"}), 4<<20, 4<<20, time.Hour)
	hist, err := OpenHistoryStore(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("OpenHistoryStore: %v", err)
	}
	defer hist.Close()
	h := NewProcessHandlers(mgr, sandboxMgr, hist, "pine")

	rec, err := sandboxMgr.Start(backend.StartOptions{Command: "cat"})
	if err != nil {
		t.Fatalf("sandboxMgr.Start: %v", err)
	}

	writeParams := mustMarshal(t, ProcessWriteParams{ProcessID: rec.ID, Data: "ping\n"})
	if _, rpcErr := h.Write(context.Background(), writeParams); rpcErr != nil {
		t.Fatalf("Write: %+v", rpcErr)
	}

	var readResult ProcessReadResult
	for i := 0; i < 20; i++ {
		readResultAny, rpcErr := h.Read(context.Background(), mustMarshal(t, ProcessReadParams{ProcessID: rec.ID, MaxBytes: 1024}))
		if rpcErr != nil {
			t.Fatalf("Read: %+v", rpcErr)
		}
		readResult = readResultAny.(ProcessReadResult)
		if readResult.Stdout == "ping\n" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if readResult.Stdout != "ping\n" {
		t.Fatalf("stdout = %q", readResult.Stdout)
	}

	listResultAny, rpcErr := h.List(context.Background(), mustMarshal(t, struct{}{}))
	if rpcErr != nil {
		t.Fatalf("List: %+v", rpcErr)
	}
	found := false
	for _, p := range listResultAny.(ProcessListResult).Processes {
		if p.ProcessID == rec.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("process %s not in list", rec.ID)
	}

	if _, rpcErr := h.Terminate(context.Background(), mustMarshal(t, ProcessTerminateParams{ProcessID: rec.ID, GraceMs: 500})); rpcErr != nil {
		t.Fatalf("Terminate: %+v", rpcErr)
	}

	waitResultAny, rpcErr := h.Wait(context.Background(), mustMarshal(t, ProcessWaitParams{ProcessID: rec.ID, TimeoutMs: 2000}))
	if rpcErr != nil {
		t.Fatalf("Wait: %+v", rpcErr)
	}
	if waitResultAny.(ProcessWaitResult).TimedOut {
		t.Fatal("expected process to have exited after Terminate")
	}
}

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return data
}

// TestProcessRead_ClampsMaxBytesToServerCap verifies that process.read's
// caller-supplied max_bytes never exceeds proto.MaxCommandOutputBytes,
// even when the caller asks for more (or asks for "unlimited" via 0).
func TestProcessRead_ClampsMaxBytesToServerCap(t *testing.T) {
	h := newTestProcessHandlers(t)
	overBy := 1024
	cmd := "head -c " + strconv.Itoa(proto.MaxCommandOutputBytes+overBy) + " /dev/zero | tr '\\0' 'a'"
	startParams, _ := json.Marshal(ProcessStartParams{Command: cmd})
	startResultAny, rpcErr := h.Start(context.Background(), startParams)
	if rpcErr != nil {
		t.Fatalf("Start: %+v", rpcErr)
	}
	startResult := startResultAny.(ProcessStartResult)

	waitParams, _ := json.Marshal(ProcessWaitParams{ProcessID: startResult.ProcessID, TimeoutMs: 5000})
	if _, rpcErr := h.Wait(context.Background(), waitParams); rpcErr != nil {
		t.Fatalf("Wait: %+v", rpcErr)
	}

	readParams, _ := json.Marshal(ProcessReadParams{ProcessID: startResult.ProcessID, MaxBytes: proto.MaxCommandOutputBytes + overBy})
	result, rpcErr := h.Read(context.Background(), readParams)
	if rpcErr != nil {
		t.Fatalf("Read: %+v", rpcErr)
	}
	res := result.(ProcessReadResult)
	if len(res.Stdout) != proto.MaxCommandOutputBytes {
		t.Fatalf("len(Stdout) = %d, want %d (server cap should win over the caller's larger max_bytes)", len(res.Stdout), proto.MaxCommandOutputBytes)
	}
}
