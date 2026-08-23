package backend

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// SandboxSetupFailedExitCode is the hidden "--landlock-exec" subcommand's
// exit code on a setup failure, kept only as a debugging aid (visible in
// e.g. `ps`/core dumps). It is NOT how the parent detects setup failure:
// syscall.Exec on success fully replaces the child's process image with
// the sandboxed command's, so the same exit code could just as well be
// that command's own legitimate exit status (a command's own
// non-zero exit is never a protocol error). Detection
// instead uses an out-of-band status pipe — see statusPipeFD and
// ExitResult.SandboxSetupFailed.
const SandboxSetupFailedExitCode = 111

// statusPipeFD is the file descriptor the hidden "--landlock-exec"
// child finds its status pipe's write end on: exec.Cmd assigns
// ExtraFiles to consecutive descriptors starting at 3 (stdin/stdout/
// stderr already occupy 0-2), and SandboxedBackend.Start only ever sets
// one ExtraFiles entry.
const statusPipeFD = 3

// SandboxedBackend starts each command inside a Landlock-restricted,
// network-namespaced child by re-execing agentBinary with a hidden
// "--landlock-exec" subcommand instead of execing the shell directly.
//
// Landlock restrictions are permanent for whichever process applies
// them (this is documented, not a bug in go-landlock), so only that
// short-lived re-exec target may ever call the Landlock API — never
// this long-running backend's own process.
//
// The child's own user, network, PID, and mount namespaces are created
// here, at process-creation time, via SysProcAttr.Cloneflags — not by
// the child calling unshare(2) on itself after it starts. Doing it this
// way avoids the well-known hazards of calling unshare(CLONE_NEWUSER)
// from inside an already-running, multi-threaded process (which the Go
// runtime always is). The namespaces exist before the child's own
// main() ever runs, and survive its later syscall.Exec by kernel design.
type SandboxedBackend struct {
	agentBinary string
	shell       []string
}

// NewSandboxedBackend returns a ProcessBackend for command.read.
// agentBinary must be a binary that recognizes "--landlock-exec --
// <argv...>" and applies Landlock + syscall.Exec — this is
// agent/cmd/main.go's landlockExecMain, already implemented and wired
// in there as that binary's hidden subcommand.
func NewSandboxedBackend(agentBinary string, shell []string) ProcessBackend {
	return &SandboxedBackend{agentBinary: agentBinary, shell: shell}
}

// scratchDirEnvVar carries the per-invocation scratch directory from this
// parent to the hidden "--landlock-exec" child: the parent creates it (so
// it can remove it again once the command exits — the child's own
// process image is gone by then, replaced by syscall.Exec) and the child
// only ever restricts Landlock to it, never creates or deletes it.
const scratchDirEnvVar = "RC_SANDBOX_SCRATCH_DIR"

func (b *SandboxedBackend) Start(opts StartOptions) (ProcessHandle, error) {
	scratchDir, err := os.MkdirTemp("", "rc-sandbox-")
	if err != nil {
		return nil, err
	}

	args := append([]string{"--landlock-exec", "--"}, b.shell...)
	args = append(args, opts.Command)
	cmd := exec.Command(b.agentBinary, args...)
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}
	env := append([]string{}, cmd.Environ()...)
	for k, v := range opts.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = append(env, scratchDirEnvVar+"="+scratchDir)
	// New process group (Terminate signals the whole tree) plus new
	// user+network+PID+mount namespaces ("host process mutation denied"):
	// the uid/gid mappings make the child see itself as the same user it
	// already is, so this works without root. Landlock alone does not
	// restrict signal delivery, and same-UID kill(2) is otherwise
	// unconditionally allowed by the kernel — CLONE_NEWPID makes every
	// host process invisible to (and therefore unaddressable by) the
	// sandboxed command, regardless of UID. CLONE_NEWNS is required for
	// landlockExecMain to remount /proc against this new PID namespace —
	// without it, procps-family tools (ps, etc.) still see the host's
	// /proc and fail to resolve their own PID once they're not PID 1 of
	// the namespace anymore (e.g. any command past the first in a
	// pipeline).
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:    true,
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET | syscall.CLONE_NEWPID | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getuid(), Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getgid(), Size: 1},
		},
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		os.RemoveAll(scratchDir)
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		os.RemoveAll(scratchDir)
		return nil, err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		os.RemoveAll(scratchDir)
		return nil, err
	}

	// Status pipe (self-pipe pattern): the write end passed via
	// ExtraFiles lands on statusPipeFD in the child. landlockExecMain
	// marks it close-on-exec, so a successful syscall.Exec closes it
	// automatically — the parent then reads a clean EOF with no data.
	// On a setup failure, landlockExecMain writes a byte to it before
	// exiting, still without ever having exec'd.
	statusR, statusW, err := os.Pipe()
	if err != nil {
		os.RemoveAll(scratchDir)
		return nil, err
	}
	cmd.ExtraFiles = []*os.File{statusW}

	if err := cmd.Start(); err != nil {
		statusR.Close()
		statusW.Close()
		os.RemoveAll(scratchDir)
		return nil, err
	}
	// The parent's own copy of the write end must close too, or its read
	// side would never see EOF even after every child-side copy closes.
	statusW.Close()

	return &sandboxedHandle{cmd: cmd, stdout: stdout, stderr: stderr, stdin: stdin, statusR: statusR, scratchDir: scratchDir}, nil
}

type sandboxedHandle struct {
	cmd        *exec.Cmd
	stdout     io.ReadCloser
	stderr     io.ReadCloser
	stdin      io.WriteCloser
	statusR    *os.File
	scratchDir string
}

func (h *sandboxedHandle) OSPID() int            { return h.cmd.Process.Pid }
func (h *sandboxedHandle) Stdout() io.Reader     { return h.stdout }
func (h *sandboxedHandle) Stderr() io.Reader     { return h.stderr }
func (h *sandboxedHandle) Stdin() io.WriteCloser { return h.stdin }

func (h *sandboxedHandle) Wait() ExitResult {
	// Unlike cmd.Wait, Process.Wait never touches the pipes; CloseIO
	// closes them.
	state, err := h.cmd.Process.Wait()
	// The write end is long closed by now — either by landlockExecMain
	// exiting (failure path) or by the exec-time close-on-exec (success
	// path) — so this never blocks beyond Process.Wait() itself.
	statusData, _ := io.ReadAll(h.statusR)
	h.statusR.Close()
	setupFailed := len(statusData) > 0
	// The scratch area is "writable, ephemeral" — the child process
	// image is gone by the time the command has exited (syscall.Exec
	// replaced it), so only the parent can remove it.
	if err := os.RemoveAll(h.scratchDir); err != nil {
		log.Printf("backend: failed to remove sandbox scratch dir %s: %v", h.scratchDir, err)
	}

	if err != nil {
		return ExitResult{Err: fmt.Errorf("wait: %w", err), SandboxSetupFailed: setupFailed}
	}
	return ExitResult{ExitCode: state.ExitCode(), SandboxSetupFailed: setupFailed}
}

func (h *sandboxedHandle) CloseIO() {
	// cmd.Wait() used to close every pipe cmd.*Pipe() handed out,
	// including the stdin write end; Process.Wait() closes none of them,
	// so this must close all three or stdin leaks its fd until GC. Errors
	// are ignored: a pipe already closing on process exit is expected,
	// and there is no recovery action for a close failure either way.
	_ = h.stdout.Close()
	_ = h.stderr.Close()
	_ = h.stdin.Close()
}

func (h *sandboxedHandle) Terminate(graceMs int) error {
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
				return nil
			}
		}
	}
}
