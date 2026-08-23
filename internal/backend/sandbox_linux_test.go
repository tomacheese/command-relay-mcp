package backend

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// buildTestSandboxHelper compiles a throwaway Go program implementing
// just the "--landlock-exec -- <argv...>" contract SandboxedBackend
// relies on, so this test can prove the backend/kernel mechanism without
// depending on the real Agent binary (that wiring is Task 8).
func buildTestSandboxHelper(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	const program = `package main

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/landlock-lsm/go-landlock/landlock"
)

func main() {
	statusFD := os.NewFile(3, "sandbox-status")
	syscall.CloseOnExec(3)
	fail := func() {
		statusFD.Write([]byte{1})
		os.Exit(111)
	}
	if len(os.Args) < 4 || os.Args[1] != "--landlock-exec" || os.Args[2] != "--" {
		fail()
	}
	execArgv := os.Args[3:]
	scratchDir := os.Getenv("RC_SANDBOX_SCRATCH_DIR")
	if scratchDir == "" {
		fail()
	}
	os.Setenv("TMPDIR", scratchDir)
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		fail()
	}
	if err := syscall.Mount("proc", "/proc", "proc", 0, ""); err != nil {
		fail()
	}
	if err := landlock.V4.RestrictPaths(landlock.RODirs("/"), landlock.RWDirs(scratchDir), landlock.RWFiles("/dev/null")); err != nil {
		fail()
	}
	path, err := exec.LookPath(execArgv[0])
	if err != nil {
		fail()
	}
	syscall.Exec(path, execArgv, os.Environ())
	fail() // only reached if Exec itself failed
}
`
	if err := os.WriteFile(src, []byte(program), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	modSrc := "module sandboxtesthelper\n\ngo 1.26\n\nrequire github.com/landlock-lsm/go-landlock v0.9.0\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(modSrc), 0o644); err != nil {
		t.Fatalf("WriteFile go.mod: %v", err)
	}
	binPath := filepath.Join(dir, "helper")
	cmd := exec.Command("go", "build", "-o", binPath, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=-mod=mod")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build helper: %v\n%s", err, out)
	}
	return binPath
}

func TestSandboxedBackend_DeniesWriteOutsideScratchDir(t *testing.T) {
	helper := buildTestSandboxHelper(t)
	b := NewSandboxedBackend(helper, []string{"/bin/bash", "-lc"})
	target := filepath.Join(t.TempDir(), "should-not-be-written")

	h, err := b.Start(StartOptions{Command: "echo x > " + target})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	res := h.Wait()
	if res.Err != nil {
		t.Fatalf("Wait: %v", res.Err)
	}
	if res.SandboxSetupFailed {
		t.Fatal("sandbox setup itself failed — Landlock unsupported on this kernel?")
	}
	if res.ExitCode == 0 {
		t.Fatal("command unexpectedly succeeded writing outside the scratch dir")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("target file was created despite the sandbox: err=%v", err)
	}
}

func TestSandboxedBackend_AllowsWriteInsideOwnScratchDir(t *testing.T) {
	helper := buildTestSandboxHelper(t)
	b := NewSandboxedBackend(helper, []string{"/bin/bash", "-lc"})

	h, err := b.Start(StartOptions{Command: `echo -n hello > "$TMPDIR/x" && cat "$TMPDIR/x"`})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	stdout, _ := io.ReadAll(h.Stdout())
	res := h.Wait()
	if res.Err != nil {
		t.Fatalf("Wait: %v", res.Err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stdout=%q)", res.ExitCode, stdout)
	}
	if string(stdout) != "hello" {
		t.Fatalf("stdout = %q, want hello", stdout)
	}
}

func TestSandboxedBackend_DeniesNetworkAccess(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	helper := buildTestSandboxHelper(t)
	b := NewSandboxedBackend(helper, []string{"/bin/bash", "-lc"})
	h, err := b.Start(StartOptions{Command: "echo connect-attempt > /dev/tcp/" + ln.Addr().String()})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	res := h.Wait()
	if res.Err != nil {
		t.Fatalf("Wait: %v", res.Err)
	}
	if res.ExitCode == 0 {
		t.Fatal("network connect unexpectedly succeeded from inside the sandbox's network namespace")
	}
}

// TestSandboxedBackend_RemovesScratchDirAfterExit covers that the
// "writable, ephemeral" scratch area must not accumulate indefinitely
// under the host's temp directory across command_read invocations.
func TestSandboxedBackend_RemovesScratchDirAfterExit(t *testing.T) {
	helper := buildTestSandboxHelper(t)
	b := NewSandboxedBackend(helper, []string{"/bin/bash", "-lc"})
	h, err := b.Start(StartOptions{Command: "true"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	scratchDir := h.(*sandboxedHandle).scratchDir
	if _, err := os.Stat(scratchDir); err != nil {
		t.Fatalf("scratch dir missing before exit: %v", err)
	}
	if res := h.Wait(); res.Err != nil {
		t.Fatalf("Wait: %v", res.Err)
	}
	if _, err := os.Stat(scratchDir); !os.IsNotExist(err) {
		t.Fatalf("scratch dir still exists after exit: err=%v", err)
	}
}

// TestSandboxedBackend_DeniesSignalingHostProcesses covers "host
// process mutation denied": Landlock alone does not restrict signal
// delivery, and the sandboxed child otherwise runs as the same host
// UID as everything else the Agent started, so without a
// PID namespace it could kill(2) any of them.
func TestSandboxedBackend_DeniesSignalingHostProcesses(t *testing.T) {
	victim := exec.Command("sleep", "5")
	if err := victim.Start(); err != nil {
		t.Fatalf("start victim: %v", err)
	}
	defer victim.Process.Kill()

	helper := buildTestSandboxHelper(t)
	b := NewSandboxedBackend(helper, []string{"/bin/bash", "-lc"})
	h, err := b.Start(StartOptions{Command: fmt.Sprintf("kill -0 %d", victim.Process.Pid)})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	res := h.Wait()
	if res.Err != nil {
		t.Fatalf("Wait: %v", res.Err)
	}
	if res.SandboxSetupFailed {
		t.Fatal("sandbox setup itself failed")
	}
	if res.ExitCode == 0 {
		t.Fatal("sandboxed command could signal a host process — PID namespace isolation not enforced")
	}
}

// TestSandboxedBackend_AllowsWriteToDevNull covers that the RODirs("/")
// default doesn't block the common `>/dev/null` redirect: /dev/null is a
// no-op sink that changes no persistent state, so denying writes to it
// has no security benefit.
func TestSandboxedBackend_AllowsWriteToDevNull(t *testing.T) {
	helper := buildTestSandboxHelper(t)
	b := NewSandboxedBackend(helper, []string{"/bin/bash", "-lc"})

	h, err := b.Start(StartOptions{Command: "echo x > /dev/null"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	stderr, _ := io.ReadAll(h.Stderr())
	res := h.Wait()
	if res.Err != nil {
		t.Fatalf("Wait: %v", res.Err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", res.ExitCode, stderr)
	}
}

// TestSandboxedBackend_PipelineProcessCanReadOwnProcSelf covers that a
// non-PID-1 process inside the sandbox's new PID namespace still sees a
// /proc matching that namespace. Without remounting /proc, the inherited
// host /proc leaves procps-family tools unable to resolve their own PID
// once they aren't PID 1 of the namespace (i.e. any command past the
// first stage of a pipeline).
func TestSandboxedBackend_PipelineProcessCanReadOwnProcSelf(t *testing.T) {
	helper := buildTestSandboxHelper(t)
	b := NewSandboxedBackend(helper, []string{"/bin/bash", "-lc"})

	h, err := b.Start(StartOptions{Command: "ps -eo pid,comm | cat"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	stdout, _ := io.ReadAll(h.Stdout())
	stderr, _ := io.ReadAll(h.Stderr())
	res := h.Wait()
	if res.Err != nil {
		t.Fatalf("Wait: %v", res.Err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", res.ExitCode, stderr)
	}
	if len(stdout) == 0 {
		t.Fatal("ps produced no output")
	}
}

// TestSandboxedBackend_CloseIOClosesStdin covers CloseIO for the
// sandboxed backend: unlike the plain Linux backend, a backgrounded
// descendant can't outlive the immediate child here (it dies with the
// whole PID namespace when the child, its PID 1, exits), so the
// regression this method exists for is stdin's fd otherwise never
// closing once Wait no longer force-closes it via cmd.Wait.
func TestSandboxedBackend_CloseIOClosesStdin(t *testing.T) {
	helper := buildTestSandboxHelper(t)
	b := NewSandboxedBackend(helper, []string{"/bin/bash", "-lc"})

	h, err := b.Start(StartOptions{Command: "true"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res := h.Wait(); res.Err != nil {
		t.Fatalf("Wait: %v", res.Err)
	}

	h.CloseIO()

	if _, err := h.Stdin().Write([]byte("x")); err == nil {
		t.Fatal("Stdin write succeeded after CloseIO, want the pipe closed")
	}
}

// TestSandboxedBackend_UserCommandExit111IsNotMisreportedAsSetupFailure
// covers that SandboxSetupFailedExitCode (111) is an ordinary,
// unreserved exit code many programs can legitimately return.
// Because syscall.Exec fully replaces the wrapper's process image on
// success, the exit code alone can't distinguish "wrapper failed setup"
// from "the sandboxed command itself exited 111" — this must be resolved
// via the status pipe, not misreported as a sandbox setup failure.
func TestSandboxedBackend_UserCommandExit111IsNotMisreportedAsSetupFailure(t *testing.T) {
	helper := buildTestSandboxHelper(t)
	b := NewSandboxedBackend(helper, []string{"/bin/bash", "-lc"})

	h, err := b.Start(StartOptions{Command: "exit 111"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	res := h.Wait()
	if res.Err != nil {
		t.Fatalf("Wait: %v", res.Err)
	}
	if res.SandboxSetupFailed {
		t.Fatal("a command legitimately exiting 111 was misreported as a sandbox setup failure")
	}
	if res.ExitCode != 111 {
		t.Fatalf("ExitCode = %d, want 111 (the command's own exit code, passed through)", res.ExitCode)
	}
}
