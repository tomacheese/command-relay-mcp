package backend

import (
	"fmt"
	"io"
	"os/exec"
	"syscall"
	"time"
)

type linuxBackend struct {
	shell []string // e.g. []string{"/bin/bash", "-lc"}
}

// NewLinuxBackend returns a ProcessBackend that wraps every command with
// the given shell invocation.
func NewLinuxBackend(shell []string) ProcessBackend {
	return &linuxBackend{shell: shell}
}

type linuxHandle struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr io.ReadCloser
	stdin  io.WriteCloser
}

func (l *linuxBackend) Start(opts StartOptions) (ProcessHandle, error) {
	args := append(append([]string{}, l.shell[1:]...), opts.Command)
	cmd := exec.Command(l.shell[0], args...)
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}
	if len(opts.Env) > 0 {
		env := append([]string{}, cmd.Environ()...)
		for k, v := range opts.Env {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}
	// New process group so Terminate can signal the whole tree, not just
	// the shell PID.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &linuxHandle{cmd: cmd, stdout: stdout, stderr: stderr, stdin: stdin}, nil
}

func (h *linuxHandle) OSPID() int            { return h.cmd.Process.Pid }
func (h *linuxHandle) Stdout() io.Reader     { return h.stdout }
func (h *linuxHandle) Stderr() io.Reader     { return h.stderr }
func (h *linuxHandle) Stdin() io.WriteCloser { return h.stdin }

func (h *linuxHandle) Wait() ExitResult {
	// Unlike cmd.Wait, Process.Wait never touches the pipes; CloseIO
	// closes them.
	state, err := h.cmd.Process.Wait()
	if err != nil {
		// Exit code unobservable (e.g. Agent killed, signal, etc.) — the
		// caller must allow exit_code to stay unknown in that case.
		return ExitResult{Err: fmt.Errorf("wait: %w", err)}
	}
	return ExitResult{ExitCode: state.ExitCode()}
}

func (h *linuxHandle) CloseIO() {
	// cmd.Wait() used to close every pipe cmd.*Pipe() handed out,
	// including the stdin write end; Process.Wait() closes none of them,
	// so this must close all three or stdin leaks its fd until GC. Errors
	// are ignored: a pipe already closing on process exit is expected,
	// and there is no recovery action for a close failure either way.
	_ = h.stdout.Close()
	_ = h.stderr.Close()
	_ = h.stdin.Close()
}

func (h *linuxHandle) Terminate(graceMs int) error {
	pgid := h.cmd.Process.Pid
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		return err
	}
	deadline := time.After(time.Duration(graceMs) * time.Millisecond)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
				return err
			}
			return nil
		case <-tick.C:
			if err := syscall.Kill(-pgid, syscall.Signal(0)); err == syscall.ESRCH {
				return nil // whole group already gone
			}
		}
	}
}
