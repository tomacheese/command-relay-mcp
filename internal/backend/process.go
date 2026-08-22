package backend

import "io"

// StartOptions describes a process to launch.
type StartOptions struct {
	// Command is a shell command string; the backend wraps it with the
	// configured default shell.
	Command string
	// Cwd overrides the shell's own `cd`, when non-empty.
	Cwd string
	// Env overrides/adds only these keys on top of the Agent's own
	// environment.
	Env map[string]string
}

// ExitResult is the outcome of a finished process. Err is set only when
// the exit code could not be observed.
type ExitResult struct {
	ExitCode int
	Err      error
	// SandboxSetupFailed is set only by SandboxedBackend, via an
	// out-of-band status pipe rather than ExitCode: the self-re-exec
	// child's own process image is fully replaced by the sandboxed
	// command's on success (syscall.Exec), so a reserved exit code alone
	// cannot be distinguished from the sandboxed command legitimately
	// exiting with that same code (a command's own non-zero exit is
	// never a protocol error).
	SandboxSetupFailed bool
}

// ProcessHandle represents one running (or exited) OS process tree,
// identified externally only via the Agent-issued opaque process_id;
// this handle is the OS-level counterpart.
type ProcessHandle interface {
	OSPID() int
	Stdout() io.Reader
	Stderr() io.Reader
	Stdin() io.WriteCloser
	// Wait blocks until the process exits and returns its result. Safe
	// to call from exactly one goroutine. It does not touch the
	// Stdout()/Stderr() pipes, so a concurrent reader is never force-
	// closed mid-read; call CloseIO once reads are drained, or as a
	// backstop if a descendant still holds a pipe open.
	Wait() ExitResult
	// CloseIO closes the Stdout()/Stderr() pipes. A caller reading them
	// must treat os.ErrClosed from a still-open read as an expected side
	// effect of this call, not an error.
	CloseIO()
	// Terminate ends the whole process group: SIGTERM, wait up to
	// graceMs, then SIGKILL if still alive.
	Terminate(graceMs int) error
}

// ProcessBackend starts OS processes. Linux and Windows implementations
// share this interface; OS differences stay inside the backend.
type ProcessBackend interface {
	Start(opts StartOptions) (ProcessHandle, error)
}
