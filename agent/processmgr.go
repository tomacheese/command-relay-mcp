package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"command-relay-mcp/internal/backend"
)

var errProcessNotFound = errors.New("process_not_found")

// ProcessRecord is the Agent-side runtime object for one process,
// keyed externally by the opaque ID, never the OS PID.
type ProcessRecord struct {
	ID        string
	OSPID     int
	StartedAt time.Time
	Stdout    *RingBuffer
	Stderr    *RingBuffer

	handle             backend.ProcessHandle
	mu                 sync.Mutex
	exitCode           *int
	exitErr            error
	sandboxSetupFailed bool
	done               chan struct{}
	finished           time.Time // zero until the process has exited
}

// Wait blocks until the process exits or ctx is done, whichever comes
// first. A context timeout never kills the process.
func (r *ProcessRecord) Wait(ctx context.Context) (exitCode *int, timedOut bool) {
	select {
	case <-r.done:
		return r.ExitCode(), false
	case <-ctx.Done():
		return nil, true
	}
}

func (r *ProcessRecord) ExitCode() *int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.exitCode
}

// SandboxSetupFailed reports whether this process's backend (only
// SandboxedBackend ever sets this) detected that its own setup — not the
// command's execution — failed, via its out-of-band status pipe rather
// than a reserved exit code — a legitimate command exit code must never
// be misread as a protocol-level failure.
func (r *ProcessRecord) SandboxSetupFailed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sandboxSetupFailed
}

// Exited reports whether the process has actually exited, independent of
// whether its exit code was observable: "running" vs "exited" state
// must come from runtime inspection, not from ExitCode() being non-nil
// — an exit whose code the backend couldn't observe, e.g. killed by a
// signal, still leaves ExitCode() nil forever.
func (r *ProcessRecord) Exited() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

func (r *ProcessRecord) stdin() io.WriteCloser { return r.handle.Stdin() }

// Manager owns every ProcessRecord this Agent has started.
type Manager struct {
	backend     backend.ProcessBackend
	stdoutCap   int
	stderrCap   int
	finishedTTL time.Duration

	mu    sync.Mutex
	procs map[string]*ProcessRecord
}

func NewManager(b backend.ProcessBackend, stdoutCap, stderrCap int, finishedTTL time.Duration) *Manager {
	return &Manager{
		backend:     b,
		stdoutCap:   stdoutCap,
		stderrCap:   stderrCap,
		finishedTTL: finishedTTL,
		procs:       make(map[string]*ProcessRecord),
	}
}

func newProcessID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (m *Manager) Start(opts backend.StartOptions) (*ProcessRecord, error) {
	h, err := m.backend.Start(opts)
	if err != nil {
		return nil, err
	}
	rec := &ProcessRecord{
		ID:        newProcessID(),
		OSPID:     h.OSPID(),
		StartedAt: time.Now(),
		Stdout:    NewRingBuffer(m.stdoutCap),
		Stderr:    NewRingBuffer(m.stderrCap),
		handle:    h,
		done:      make(chan struct{}),
	}

	go func() {
		// os.ErrClosed is the expected race between this copy and the
		// exit-wait goroutine below: os/exec closes the pipe once Wait
		// observes the process has exited, which can happen while this
		// copy is still in flight. Anything else is a genuine failure.
		if _, err := io.Copy(rec.Stdout, h.Stdout()); err != nil && !errors.Is(err, os.ErrClosed) {
			log.Printf("agent: stdout copy for process %s (pid %d) failed: %v", rec.ID, rec.OSPID, err)
		}
	}()
	go func() {
		if _, err := io.Copy(rec.Stderr, h.Stderr()); err != nil && !errors.Is(err, os.ErrClosed) {
			log.Printf("agent: stderr copy for process %s (pid %d) failed: %v", rec.ID, rec.OSPID, err)
		}
	}()
	go func() {
		res := h.Wait()
		rec.mu.Lock()
		if res.Err == nil {
			ec := res.ExitCode
			rec.exitCode = &ec
		} else {
			rec.exitErr = res.Err
		}
		rec.sandboxSetupFailed = res.SandboxSetupFailed
		rec.finished = time.Now()
		rec.mu.Unlock()
		close(rec.done)
		log.Printf("agent: process %s (pid %d) exited: exitCode=%v err=%v", rec.ID, rec.OSPID, res.ExitCode, res.Err)
	}()

	m.mu.Lock()
	m.procs[rec.ID] = rec
	m.mu.Unlock()
	// The command line itself is not logged: a command can carry a
	// secret (e.g. in an arg), so logging it risks leaking one.
	log.Printf("agent: process %s (pid %d) started", rec.ID, rec.OSPID)
	return rec, nil
}

func (m *Manager) Get(id string) (*ProcessRecord, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.procs[id]
	return r, ok
}

func (m *Manager) List() []*ProcessRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*ProcessRecord, 0, len(m.procs))
	for _, r := range m.procs {
		out = append(out, r)
	}
	return out
}

func (m *Manager) Terminate(id string, graceMs int) error {
	m.mu.Lock()
	rec, ok := m.procs[id]
	m.mu.Unlock()
	if !ok {
		return errProcessNotFound
	}
	log.Printf("agent: terminating process %s (pid %d)", rec.ID, rec.OSPID)
	return rec.handle.Terminate(graceMs)
}

// TerminateAll terminates every still-running process tree this Agent
// started. Call it on Agent shutdown so the graceful-stop path enforces
// the kill boundary in-process; on Linux, systemd's default
// KillMode=control-group additionally enforces the same boundary at
// the cgroup level for a kill/crash the Agent can't catch. Processes
// are terminated concurrently, so total shutdown time is bounded by
// graceMs regardless of how many processes are running.
func (m *Manager) TerminateAll(graceMs int) {
	var wg sync.WaitGroup
	for _, rec := range m.List() {
		if rec.Exited() {
			continue
		}
		wg.Add(1)
		go func(rec *ProcessRecord) {
			defer wg.Done()
			if err := rec.handle.Terminate(graceMs); err != nil {
				log.Printf("agent: process-management failure: shutdown termination of process %s (pid %d) failed: %v", rec.ID, rec.OSPID, err)
			}
		}(rec)
	}
	wg.Wait()
}

// StartGC discards runtime state for processes that finished more than
// finishedTTL ago; execution history in SQLite is unaffected. Call
// once per Agent process.
func (m *Manager) StartGC(ctx context.Context, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				now := time.Now()
				m.mu.Lock()
				for id, rec := range m.procs {
					rec.mu.Lock()
					finished := !rec.finished.IsZero() && now.Sub(rec.finished) > m.finishedTTL
					rec.mu.Unlock()
					if finished {
						delete(m.procs, id)
					}
				}
				m.mu.Unlock()
			}
		}
	}()
}
