package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"time"

	"command-relay-mcp/internal/backend"
	"command-relay-mcp/internal/proto"
)

// CommandExecParams is the payload of command.exec / command.read.
type CommandExecParams struct {
	Command         string            `json:"command"`
	Cwd             string            `json:"cwd,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	TimeoutMs       int               `json:"timeout_ms,omitempty"`
	ClientContextID string            `json:"client_context_id,omitempty"`
	ClientSubject   string            `json:"client_subject,omitempty"`
}

// CommandExecResult covers both response shapes: timed_out=false and
// timed_out=true use the same struct.
type CommandExecResult struct {
	ProcessID string `json:"process_id"`
	OSPID     int    `json:"os_pid"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	ExitCode  *int   `json:"exit_code"`
	TimedOut  bool   `json:"timed_out"`
	// SandboxViolation is set only by command.read: a non-zero exit from
	// a command that ran inside the read-only sandbox means the sandbox
	// denied a mutation attempt exactly as intended, which is still an
	// RPC success. This flag is how the caller tells that apart from
	// any other non-zero exit. Always false for command.exec, which
	// never runs sandboxed.
	SandboxViolation bool `json:"sandbox_violation,omitempty"`
	// StdoutTruncated/StderrTruncated are set when the corresponding
	// stream exceeded proto.MaxCommandOutputBytes and was cut off in
	// this response — command.exec/command.read are one-shot with no
	// paging, so this is the caller's only signal that output beyond
	// the cap was discarded.
	StdoutTruncated bool `json:"stdout_truncated,omitempty"`
	StderrTruncated bool `json:"stderr_truncated,omitempty"`
}

const (
	defaultExecTimeoutMs = 30_000
	// maxExecTimeoutMs caps caller-supplied timeout_ms so a single slow
	// command.exec/command.read call can't hold its per-request handler
	// goroutine open indefinitely and delay Connection's reconnect.
	maxExecTimeoutMs = 300_000
)

type CommandHandlers struct {
	mgr              *Manager
	sandboxMgr       *Manager // nil if the sandbox is unavailable on this Agent
	hist             *HistoryStore
	deviceID         string
	defaultTimeoutMs int
}

func NewCommandHandlers(mgr, sandboxMgr *Manager, hist *HistoryStore, deviceID string, defaultTimeoutMs int) *CommandHandlers {
	if defaultTimeoutMs <= 0 {
		defaultTimeoutMs = defaultExecTimeoutMs
	}
	return &CommandHandlers{mgr: mgr, sandboxMgr: sandboxMgr, hist: hist, deviceID: deviceID, defaultTimeoutMs: defaultTimeoutMs}
}

// clampTimeoutMs resolves a caller-supplied timeout_ms (0 meaning "use
// the default") to an effective value no larger than maxExecTimeoutMs,
// so an excessive caller-supplied timeout can't be honored unbounded.
func clampTimeoutMs(requested, fallback int) int {
	timeoutMs := requested
	if timeoutMs <= 0 {
		timeoutMs = fallback
	}
	if timeoutMs > maxExecTimeoutMs {
		timeoutMs = maxExecTimeoutMs
	}
	return timeoutMs
}

func newExecutionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Exec implements command.exec: normal, state-changing execution.
func (h *CommandHandlers) Exec(ctx context.Context, raw json.RawMessage) (any, *proto.RPCError) {
	var p CommandExecParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &proto.RPCError{Code: proto.ErrInvalidRequest, Message: err.Error()}
	}
	timeoutMs := clampTimeoutMs(p.TimeoutMs, h.defaultTimeoutMs)

	rec, err := h.mgr.Start(backend.StartOptions{Command: p.Command, Cwd: p.Cwd, Env: p.Env})
	if err != nil {
		log.Printf("agent: process-management failure: command_exec start failed: %v", err)
		return nil, &proto.RPCError{Code: proto.ErrInternal, Message: err.Error()}
	}

	execID := newExecutionID()
	if err := h.hist.RecordStart(ExecutionStart{
		ExecutionID: execID, ProcessID: rec.ID, DeviceID: h.deviceID,
		Mode: "write", Command: p.Command, Cwd: p.Cwd, StartedAt: rec.StartedAt,
		ClientContextID: p.ClientContextID, ClientSubject: p.ClientSubject,
	}); err != nil {
		log.Printf("agent: history RecordStart failed for execution %s: %v", execID, err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()
	exitCode, timedOut := rec.Wait(waitCtx)
	if !timedOut {
		if err := h.hist.RecordEnd(execID, time.Now(), exitCode); err != nil {
			log.Printf("agent: history RecordEnd failed for execution %s: %v", execID, err)
		}
	} else {
		h.recordEndEventually(execID, rec)
	}

	stdout, stdoutNext, _ := rec.Stdout.ReadFrom(0, proto.MaxCommandOutputBytes)
	stderr, stderrNext, _ := rec.Stderr.ReadFrom(0, proto.MaxCommandOutputBytes)
	return CommandExecResult{
		ProcessID: rec.ID, OSPID: rec.OSPID,
		Stdout: string(stdout), Stderr: string(stderr),
		ExitCode: exitCode, TimedOut: timedOut,
		StdoutTruncated: stdoutNext < rec.Stdout.Len(),
		StderrTruncated: stderrNext < rec.Stderr.Len(),
	}, nil
}

// recordEndEventually completes execID's history row once its process
// actually finishes, for the timeout case where Exec/Read return to the
// caller before that happens. A timeout never kills the process, so
// its real, fully-observable outcome would otherwise sit in
// execution_get/execution_list as ended_at=NULL forever. That's unlike
// exit_code=NULL, which is reserved for a genuinely unobservable
// outcome (e.g. Agent crash) — this exit *was* observed, just not by
// this call.
func (h *CommandHandlers) recordEndEventually(execID string, rec *ProcessRecord) {
	go func() {
		exitCode, _ := rec.Wait(context.Background())
		if err := h.hist.RecordEnd(execID, time.Now(), exitCode); err != nil {
			log.Printf("agent: history RecordEnd failed for execution %s: %v", execID, err)
		}
	}()
}

// Read implements command.read. Returns unsupported rather than
// silently running normally when the sandbox backend is unavailable —
// never falls back to a plain command.exec.
func (h *CommandHandlers) Read(ctx context.Context, raw json.RawMessage) (any, *proto.RPCError) {
	if h.sandboxMgr == nil {
		return nil, &proto.RPCError{Code: proto.ErrUnsupported, Message: "command_read sandbox not available on this Agent"}
	}
	var p CommandExecParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &proto.RPCError{Code: proto.ErrInvalidRequest, Message: err.Error()}
	}
	timeoutMs := clampTimeoutMs(p.TimeoutMs, h.defaultTimeoutMs)

	rec, err := h.sandboxMgr.Start(backend.StartOptions{Command: p.Command, Cwd: p.Cwd, Env: p.Env})
	if err != nil {
		log.Printf("agent: process-management failure: command_read start failed: %v", err)
		return nil, &proto.RPCError{Code: proto.ErrInternal, Message: err.Error()}
	}

	execID := newExecutionID()
	if err := h.hist.RecordStart(ExecutionStart{
		ExecutionID: execID, ProcessID: rec.ID, DeviceID: h.deviceID,
		Mode: "read", Command: p.Command, Cwd: p.Cwd, StartedAt: rec.StartedAt,
		ClientContextID: p.ClientContextID, ClientSubject: p.ClientSubject,
	}); err != nil {
		log.Printf("agent: history RecordStart failed for execution %s: %v", execID, err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()
	exitCode, timedOut := rec.Wait(waitCtx)
	if !timedOut {
		if err := h.hist.RecordEnd(execID, time.Now(), exitCode); err != nil {
			log.Printf("agent: history RecordEnd failed for execution %s: %v", execID, err)
		}
	} else {
		h.recordEndEventually(execID, rec)
	}

	// The sandbox setup itself failing is a distinct condition from the
	// sandboxed command's own non-zero exit (which is a normal RPC
	// success — the sandbox worked as intended): always sandbox_violation,
	// never a plain exit code (addendum §4 step 3). Detected via the
	// backend's out-of-band status pipe (ProcessRecord.SandboxSetupFailed),
	// not the exit code itself — that reserved code is also an ordinary
	// exit code the sandboxed command could legitimately return on its
	// own.
	if !timedOut && rec.SandboxSetupFailed() {
		log.Printf("agent: sandbox failure for execution %s: command_read setup failed", execID)
		return nil, &proto.RPCError{Code: proto.ErrSandboxViolation, Message: "sandbox setup failed for this call"}
	}

	stdout, stdoutNext, _ := rec.Stdout.ReadFrom(0, proto.MaxCommandOutputBytes)
	stderr, stderrNext, _ := rec.Stderr.ReadFrom(0, proto.MaxCommandOutputBytes)
	return CommandExecResult{
		ProcessID: rec.ID, OSPID: rec.OSPID,
		Stdout: string(stdout), Stderr: string(stderr),
		ExitCode: exitCode, TimedOut: timedOut,
		// A non-zero exit under this sandboxed run means the sandbox
		// denied a mutation attempt exactly as intended — never true on
		// timeout, since there is no exit code yet.
		SandboxViolation: !timedOut && exitCode != nil && *exitCode != 0,
		StdoutTruncated:  stdoutNext < rec.Stdout.Len(),
		StderrTruncated:  stderrNext < rec.Stderr.Len(),
	}, nil
}
