package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"command-relay-mcp/internal/backend"
	"command-relay-mcp/internal/proto"
)

func newTestCommandHandlers(t *testing.T) *CommandHandlers {
	mgr := NewManager(backend.NewLinuxBackend([]string{"/bin/bash", "-lc"}), 4<<20, 4<<20, time.Hour)
	hist, err := OpenHistoryStore(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("OpenHistoryStore: %v", err)
	}
	t.Cleanup(func() { hist.Close() })
	return NewCommandHandlers(mgr, nil, hist, "pine", 5000)
}

func TestCommandExec_CompletesWithinTimeout(t *testing.T) {
	h := newTestCommandHandlers(t)
	params, _ := json.Marshal(CommandExecParams{Command: "echo hi", TimeoutMs: 2000})

	result, rpcErr := h.Exec(context.Background(), params)
	if rpcErr != nil {
		t.Fatalf("rpcErr: %+v", rpcErr)
	}
	res := result.(CommandExecResult)
	if res.TimedOut || res.ExitCode == nil || *res.ExitCode != 0 || res.Stdout != "hi\n" {
		t.Fatalf("res = %+v", res)
	}
}

func TestCommandExec_TimeoutDoesNotKillProcess(t *testing.T) {
	h := newTestCommandHandlers(t)
	params, _ := json.Marshal(CommandExecParams{Command: "sleep 0.3; echo done", TimeoutMs: 50})

	result, rpcErr := h.Exec(context.Background(), params)
	if rpcErr != nil {
		t.Fatalf("rpcErr: %+v", rpcErr)
	}
	res := result.(CommandExecResult)
	if !res.TimedOut || res.ExitCode != nil {
		t.Fatalf("res = %+v", res)
	}

	// Base spec §8.5: the process keeps running; process.wait picks it up later.
	rec, ok := h.mgr.Get(res.ProcessID)
	if !ok {
		t.Fatal("process record vanished after exec timeout")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	exitCode, timedOut := rec.Wait(ctx)
	if timedOut || exitCode == nil || *exitCode != 0 {
		t.Fatalf("exitCode=%v timedOut=%v", exitCode, timedOut)
	}

	// The timed-out call returns before the process exits, but its
	// history row must still eventually get its real, fully-observed
	// outcome recorded — not sit at ended_at=NULL forever.
	deadline := time.Now().Add(2 * time.Second)
	for {
		list, err := h.hist.List(10)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(list) == 1 && list[0].EndedAt != nil {
			if list[0].ExitCode == nil || *list[0].ExitCode != 0 {
				t.Fatalf("recorded execution = %+v", list[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("execution history was never completed after the process actually exited: %+v", list)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCommandExec_RecordsClientContextAndSubject(t *testing.T) {
	h := newTestCommandHandlers(t)
	params, _ := json.Marshal(CommandExecParams{
		Command: "true", TimeoutMs: 2000,
		ClientContextID: "sess-abc", ClientSubject: "user-123",
	})

	result, rpcErr := h.Exec(context.Background(), params)
	if rpcErr != nil {
		t.Fatalf("rpcErr: %+v", rpcErr)
	}
	_ = result.(CommandExecResult) // Exec's result type; asserted only to catch a signature regression

	list, err := h.hist.List(10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("len(list) = %d, want 1", len(list))
	}
	if list[0].ClientContextID != "sess-abc" || list[0].ClientSubject != "user-123" {
		t.Fatalf("execution = %+v", list[0])
	}
}

func TestCommandRead_ReturnsUnsupported(t *testing.T) {
	h := newTestCommandHandlers(t)
	params, _ := json.Marshal(CommandExecParams{Command: "echo hi"})

	_, rpcErr := h.Read(context.Background(), params)
	if rpcErr == nil || rpcErr.Code != proto.ErrUnsupported {
		t.Fatalf("rpcErr = %+v", rpcErr)
	}
}

func buildAgentBinaryForSandboxTest(t *testing.T) string {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "agent")
	cmd := exec.Command("go", "build", "-o", binPath, "command-relay-mcp/agent/cmd")
	cmd.Dir = repoRootForTest(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build agent/cmd: %v\n%s", err, out)
	}
	return binPath
}

func repoRootForTest(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	return filepath.Dir(wd) // agent/ -> repo root
}

func TestCommandRead_DeniesWriteOutsideScratchAndRecordsHistory(t *testing.T) {
	agentBin := buildAgentBinaryForSandboxTest(t)
	sandboxBackend := backend.NewSandboxedBackend(agentBin, []string{"/bin/bash", "-lc"})
	if !ProbeSandbox(sandboxBackend) {
		t.Skip("Landlock not supported on this kernel")
	}
	sandboxMgr := NewManager(sandboxBackend, 4<<20, 4<<20, time.Hour)
	hist, err := OpenHistoryStore(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("OpenHistoryStore: %v", err)
	}
	defer hist.Close()
	h := NewCommandHandlers(NewManager(backend.NewLinuxBackend([]string{"/bin/bash", "-lc"}), 4<<20, 4<<20, time.Hour), sandboxMgr, hist, "pine", 5000)

	target := filepath.Join(t.TempDir(), "should-not-exist")
	params, _ := json.Marshal(CommandExecParams{Command: "echo x > " + target, TimeoutMs: 5000})
	result, rpcErr := h.Read(context.Background(), params)
	if rpcErr != nil {
		t.Fatalf("rpcErr: %+v", rpcErr)
	}
	res := result.(CommandExecResult)
	if res.ExitCode == nil || *res.ExitCode == 0 {
		t.Fatalf("res = %+v, expected a non-zero exit from the denied write", res)
	}
	// The sandbox denying a mutation is still an RPC success,
	// distinguished from an unrelated non-zero exit by this flag.
	if !res.SandboxViolation {
		t.Fatalf("res.SandboxViolation = false, want true for a denied write")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("target file exists despite the sandbox: err=%v", err)
	}

	list, err := hist.List(10)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Mode != "read" {
		t.Fatalf("history = %+v", list)
	}
}

func TestCommandRead_ReturnsUnsupportedWithoutSandboxManager(t *testing.T) {
	h := newTestCommandHandlers(t) // sandboxMgr is nil via the existing helper
	params, _ := json.Marshal(CommandExecParams{Command: "true"})
	_, rpcErr := h.Read(context.Background(), params)
	if rpcErr == nil || rpcErr.Code != proto.ErrUnsupported {
		t.Fatalf("rpcErr = %+v, want unsupported", rpcErr)
	}
}

// alwaysSandboxSetupFailedBackend simulates a Landlock/namespace setup
// failure deterministically, without depending on the host kernel
// actually lacking Landlock support (which it doesn't, in CI/dev).
type alwaysSandboxSetupFailedBackend struct{}

func (alwaysSandboxSetupFailedBackend) Start(backend.StartOptions) (backend.ProcessHandle, error) {
	return alwaysSandboxSetupFailedHandle{}, nil
}

type alwaysSandboxSetupFailedHandle struct{}

func (alwaysSandboxSetupFailedHandle) OSPID() int            { return 0 }
func (alwaysSandboxSetupFailedHandle) Stdout() io.Reader     { return strings.NewReader("") }
func (alwaysSandboxSetupFailedHandle) Stderr() io.Reader     { return strings.NewReader("") }
func (alwaysSandboxSetupFailedHandle) Stdin() io.WriteCloser { return nil }
func (alwaysSandboxSetupFailedHandle) Wait() backend.ExitResult {
	return backend.ExitResult{SandboxSetupFailed: true}
}
func (alwaysSandboxSetupFailedHandle) Terminate(graceMs int) error { return nil }
func (alwaysSandboxSetupFailedHandle) CloseIO()                    {}

// alwaysStartErrorBackend simulates the backend itself failing to spawn a
// process at all (distinct from a sandbox setup failure inside an
// otherwise-started process) — the Agent's "unexpected
// process-management failure" logging category.
type alwaysStartErrorBackend struct{}

func (alwaysStartErrorBackend) Start(backend.StartOptions) (backend.ProcessHandle, error) {
	return nil, errors.New("fork/exec failed")
}

// TestCommandExec_LogsProcessManagementFailure covers the Agent's
// "unexpected process-management failure" logging category for
// command.exec's backend-start failure path.
func TestCommandExec_LogsProcessManagementFailure(t *testing.T) {
	hist, err := OpenHistoryStore(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("OpenHistoryStore: %v", err)
	}
	defer hist.Close()
	h := NewCommandHandlers(NewManager(alwaysStartErrorBackend{}, 4<<20, 4<<20, time.Hour), nil, hist, "pine", 5000)

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	params, _ := json.Marshal(CommandExecParams{Command: "true", TimeoutMs: 5000})
	_, rpcErr := h.Exec(context.Background(), params)
	if rpcErr == nil || rpcErr.Code != proto.ErrInternal {
		t.Fatalf("rpcErr = %+v, want internal_error", rpcErr)
	}
	if !strings.Contains(logBuf.String(), "process-management failure") {
		t.Fatalf("log output = %q, want it to mention \"process-management failure\"", logBuf.String())
	}
}

// TestCommandRead_LogsSandboxFailure covers the Agent's "sandbox
// failure" logging — a per-call setup failure must be distinguishable
// in the log from a normal non-zero exit, not just folded into
// processmgr's generic "process exited" line.
func TestCommandRead_LogsSandboxFailure(t *testing.T) {
	sandboxMgr := NewManager(alwaysSandboxSetupFailedBackend{}, 4<<20, 4<<20, time.Hour)
	hist, err := OpenHistoryStore(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("OpenHistoryStore: %v", err)
	}
	defer hist.Close()
	h := NewCommandHandlers(NewManager(backend.NewLinuxBackend([]string{"/bin/bash", "-lc"}), 4<<20, 4<<20, time.Hour), sandboxMgr, hist, "pine", 5000)

	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	params, _ := json.Marshal(CommandExecParams{Command: "true", TimeoutMs: 5000})
	_, rpcErr := h.Read(context.Background(), params)
	if rpcErr == nil || rpcErr.Code != proto.ErrSandboxViolation {
		t.Fatalf("rpcErr = %+v, want sandbox_violation", rpcErr)
	}
	if !strings.Contains(logBuf.String(), "sandbox failure") {
		t.Fatalf("log output = %q, want it to mention \"sandbox failure\"", logBuf.String())
	}
}

// TestCommandExec_CapsStdoutAtMaxCommandOutputBytes verifies that
// command.exec does not return unbounded stdout — output beyond
// proto.MaxCommandOutputBytes is capped rather than returned in full.
func TestCommandExec_CapsStdoutAtMaxCommandOutputBytes(t *testing.T) {
	h := newTestCommandHandlers(t)
	overBy := 1024
	// /dev/zero piped through tr avoids relying on a specific shell
	// builtin's own output-size limits.
	cmd := "head -c " + strconv.Itoa(proto.MaxCommandOutputBytes+overBy) + " /dev/zero | tr '\\0' 'a'"
	params, _ := json.Marshal(CommandExecParams{Command: cmd, TimeoutMs: 5000})

	result, rpcErr := h.Exec(context.Background(), params)
	if rpcErr != nil {
		t.Fatalf("rpcErr: %+v", rpcErr)
	}
	res := result.(CommandExecResult)
	if len(res.Stdout) != proto.MaxCommandOutputBytes {
		t.Fatalf("len(Stdout) = %d, want %d", len(res.Stdout), proto.MaxCommandOutputBytes)
	}
	if !res.StdoutTruncated {
		t.Fatal("StdoutTruncated = false, want true since stdout exceeded the cap")
	}
	if res.StderrTruncated {
		t.Fatal("StderrTruncated = true, want false since stderr produced no output")
	}
}
