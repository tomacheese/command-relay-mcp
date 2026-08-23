package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	"command-relay-mcp/internal/backend"
	"command-relay-mcp/internal/proto"
)

type ProcessStartParams struct {
	Command         string            `json:"command"`
	Cwd             string            `json:"cwd,omitempty"`
	Env             map[string]string `json:"env,omitempty"`
	ClientContextID string            `json:"client_context_id,omitempty"`
	ClientSubject   string            `json:"client_subject,omitempty"`
}

type ProcessStartResult struct {
	ProcessID string `json:"process_id"`
	OSPID     int    `json:"os_pid"`
}

type ProcessReadParams struct {
	ProcessID    string `json:"process_id"`
	StdoutOffset int64  `json:"stdout_offset"`
	StderrOffset int64  `json:"stderr_offset"`
	MaxBytes     int    `json:"max_bytes,omitempty"`
}

type ProcessReadResult struct {
	Stdout                string `json:"stdout"`
	Stderr                string `json:"stderr"`
	NextStdoutOffset      int64  `json:"next_stdout_offset"`
	NextStderrOffset      int64  `json:"next_stderr_offset"`
	StdoutTruncatedBefore bool   `json:"stdout_truncated_before"`
	StderrTruncatedBefore bool   `json:"stderr_truncated_before"`
	ExitCode              *int   `json:"exit_code"`
}

// ProcessWriteParams carries UTF-8 text stdin only for V1.
//
// ponytail: binary stdin (base64) is deferred until a real caller needs
// it — every V1 use case (git, build tools, REPLs) speaks text stdin.
type ProcessWriteParams struct {
	ProcessID string `json:"process_id"`
	Data      string `json:"data"`
}

type ProcessWaitParams struct {
	ProcessID string `json:"process_id"`
	TimeoutMs int    `json:"timeout_ms,omitempty"`
}

type ProcessWaitResult struct {
	ExitCode *int `json:"exit_code"`
	TimedOut bool `json:"timed_out"`
}

type ProcessTerminateParams struct {
	ProcessID string `json:"process_id"`
	GraceMs   int    `json:"grace_ms,omitempty"`
}

type ProcessTerminateResult struct {
	Terminated bool `json:"terminated"`
}

type ProcessSummary struct {
	ProcessID string `json:"process_id"`
	OSPID     int    `json:"os_pid"`
	State     string `json:"state"` // "running" | "exited"
	ExitCode  *int   `json:"exit_code,omitempty"`
	StartedAt string `json:"started_at"`
}

type ProcessListResult struct {
	Processes []ProcessSummary `json:"processes"`
}

const defaultWaitTimeoutMs = 30_000

type ProcessHandlers struct {
	mgr        *Manager
	sandboxMgr *Manager // nil if the sandbox is unavailable on this Agent
	hist       *HistoryStore
	deviceID   string
}

func NewProcessHandlers(mgr, sandboxMgr *Manager, hist *HistoryStore, deviceID string) *ProcessHandlers {
	return &ProcessHandlers{mgr: mgr, sandboxMgr: sandboxMgr, hist: hist, deviceID: deviceID}
}

func notFound(msg string) *proto.RPCError {
	return &proto.RPCError{Code: proto.ErrProcessNotFound, Message: msg}
}

// find looks up id across every Manager this Agent runs processes under.
// A command_read call that times out leaves its process running under
// sandboxMgr, not mgr, but it must still stay trackable via
// process_read/write/wait/terminate/list afterward.
func (h *ProcessHandlers) find(id string) (rec *ProcessRecord, mgr *Manager, ok bool) {
	if rec, ok := h.mgr.Get(id); ok {
		return rec, h.mgr, true
	}
	if h.sandboxMgr != nil {
		if rec, ok := h.sandboxMgr.Get(id); ok {
			return rec, h.sandboxMgr, true
		}
	}
	return nil, nil, false
}

// Start implements process.start.
func (h *ProcessHandlers) Start(ctx context.Context, raw json.RawMessage) (any, *proto.RPCError) {
	var p ProcessStartParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &proto.RPCError{Code: proto.ErrInvalidRequest, Message: err.Error()}
	}
	rec, err := h.mgr.Start(backend.StartOptions{Command: p.Command, Cwd: p.Cwd, Env: p.Env})
	if err != nil {
		return nil, &proto.RPCError{Code: proto.ErrInternal, Message: err.Error()}
	}
	execID := newExecutionID()
	if err := h.hist.RecordStart(ExecutionStart{
		ExecutionID: execID, ProcessID: rec.ID, DeviceID: h.deviceID,
		Mode: "process", Command: p.Command, Cwd: p.Cwd, StartedAt: rec.StartedAt,
		ClientContextID: p.ClientContextID, ClientSubject: p.ClientSubject,
	}); err != nil {
		log.Printf("agent: history RecordStart failed for execution %s: %v", execID, err)
	}
	// Unlike command.exec, process.start returns before the process
	// exits, so nothing else observes its end — record it here whenever
	// that eventually happens. exit_code stays NULL only if the Agent
	// itself goes away first (e.g. a crash).
	go func() {
		exitCode, _ := rec.Wait(context.Background())
		if err := h.hist.RecordEnd(execID, time.Now(), exitCode); err != nil {
			log.Printf("agent: history RecordEnd failed for execution %s: %v", execID, err)
		}
	}()
	return ProcessStartResult{ProcessID: rec.ID, OSPID: rec.OSPID}, nil
}

func clampMaxBytes(requested int) int {
	if requested <= 0 || requested > proto.MaxCommandOutputBytes {
		return proto.MaxCommandOutputBytes
	}
	return requested
}

// Read implements process.read: pull-only, offset-based.
func (h *ProcessHandlers) Read(ctx context.Context, raw json.RawMessage) (any, *proto.RPCError) {
	var p ProcessReadParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &proto.RPCError{Code: proto.ErrInvalidRequest, Message: err.Error()}
	}
	rec, _, ok := h.find(p.ProcessID)
	if !ok {
		return nil, notFound("process_id not found: " + p.ProcessID)
	}
	maxBytes := clampMaxBytes(p.MaxBytes)
	stdout, nextOut, truncOut := rec.Stdout.ReadFrom(p.StdoutOffset, maxBytes)
	stderr, nextErr, truncErr := rec.Stderr.ReadFrom(p.StderrOffset, maxBytes)
	return ProcessReadResult{
		Stdout: string(stdout), Stderr: string(stderr),
		NextStdoutOffset: nextOut, NextStderrOffset: nextErr,
		StdoutTruncatedBefore: truncOut, StderrTruncatedBefore: truncErr,
		ExitCode: rec.ExitCode(),
	}, nil
}

// Write implements process.write.
func (h *ProcessHandlers) Write(ctx context.Context, raw json.RawMessage) (any, *proto.RPCError) {
	var p ProcessWriteParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &proto.RPCError{Code: proto.ErrInvalidRequest, Message: err.Error()}
	}
	rec, _, ok := h.find(p.ProcessID)
	if !ok {
		return nil, notFound("process_id not found: " + p.ProcessID)
	}
	if _, err := rec.stdin().Write([]byte(p.Data)); err != nil {
		log.Printf("agent: process-management failure: stdin write to process %s failed: %v", p.ProcessID, err)
		return nil, &proto.RPCError{Code: proto.ErrInternal, Message: err.Error()}
	}
	return struct{}{}, nil
}

// Wait implements process.wait; a timeout never kills the process.
func (h *ProcessHandlers) Wait(ctx context.Context, raw json.RawMessage) (any, *proto.RPCError) {
	var p ProcessWaitParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &proto.RPCError{Code: proto.ErrInvalidRequest, Message: err.Error()}
	}
	rec, _, ok := h.find(p.ProcessID)
	if !ok {
		return nil, notFound("process_id not found: " + p.ProcessID)
	}
	timeoutMs := clampTimeoutMs(p.TimeoutMs, defaultWaitTimeoutMs)
	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()
	exitCode, timedOut := rec.Wait(waitCtx)
	return ProcessWaitResult{ExitCode: exitCode, TimedOut: timedOut}, nil
}

// Terminate implements process.terminate: whole process tree, graceful
// then forced.
func (h *ProcessHandlers) Terminate(ctx context.Context, raw json.RawMessage) (any, *proto.RPCError) {
	var p ProcessTerminateParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &proto.RPCError{Code: proto.ErrInvalidRequest, Message: err.Error()}
	}
	graceMs := p.GraceMs
	if graceMs <= 0 {
		graceMs = 5000 // default grace period before SIGKILL
	}
	_, mgr, ok := h.find(p.ProcessID)
	if !ok {
		return nil, notFound("process_id not found: " + p.ProcessID)
	}
	if err := mgr.Terminate(p.ProcessID, graceMs); err != nil {
		if errors.Is(err, errProcessNotFound) {
			return nil, notFound(err.Error())
		}
		log.Printf("agent: process-management failure: terminating process %s failed: %v", p.ProcessID, err)
		return nil, &proto.RPCError{Code: proto.ErrInternal, Message: err.Error()}
	}
	return ProcessTerminateResult{Terminated: true}, nil
}

// List implements process.list, across every Manager this Agent runs
// processes under (see find).
func (h *ProcessHandlers) List(ctx context.Context, raw json.RawMessage) (any, *proto.RPCError) {
	recs := h.mgr.List()
	if h.sandboxMgr != nil {
		recs = append(recs, h.sandboxMgr.List()...)
	}
	var out []ProcessSummary
	for _, rec := range recs {
		state := "running"
		exitCode := rec.ExitCode()
		if rec.Exited() {
			state = "exited"
		}
		out = append(out, ProcessSummary{
			ProcessID: rec.ID, OSPID: rec.OSPID, State: state,
			ExitCode: exitCode, StartedAt: rec.StartedAt.UTC().Format(time.RFC3339),
		})
	}
	return ProcessListResult{Processes: out}, nil
}
