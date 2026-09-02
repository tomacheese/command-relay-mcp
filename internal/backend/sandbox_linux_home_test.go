package backend

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

var (
	realAgentBinaryOnce sync.Once
	realAgentBinaryPath string
	realAgentBinaryErr  error
)

// realAgentBinary builds the actual agent/cmd binary (the one that
// implements landlockExecMain, unlike buildTestSandboxHelper's inline
// reimplementation) once per test run. Building it from inside the repo
// module — rather than in a throwaway module under a tmp dir — avoids the
// unrelated "go build: error obtaining VCS status" failure that
// buildTestSandboxHelper hits when run outside the repo.
func realAgentBinary(t *testing.T) string {
	t.Helper()
	realAgentBinaryOnce.Do(func() {
		dir, err := os.MkdirTemp("", "sandbox-home-test-agent-")
		if err != nil {
			realAgentBinaryErr = err
			return
		}
		binPath := filepath.Join(dir, "command-relay-agent")
		build := exec.Command("go", "build", "-o", binPath, "command-relay-mcp/agent/cmd")
		if out, err := build.CombinedOutput(); err != nil {
			realAgentBinaryErr = err
			t.Logf("go build agent/cmd output:\n%s", out)
			return
		}
		realAgentBinaryPath = binPath
	})
	if realAgentBinaryErr != nil {
		t.Fatalf("go build agent/cmd: %v", realAgentBinaryErr)
	}
	return realAgentBinaryPath
}

// writeFile writes content to path, creating parent directories as needed.
func writeFile(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

// runSandboxed runs command inside a real SandboxedBackend (built from
// the real agent/cmd binary) with the given HOME, and returns stdout and
// the exit code.
func runSandboxed(t *testing.T, home, command string) (stdout string, exitCode int) {
	t.Helper()
	// realAgentBinary's "go build" must run under this test process's
	// real HOME (GOCACHE/GOPATH derive from it), so it is built before
	// HOME is overridden to the fake one below.
	agentBinary := realAgentBinary(t)
	t.Setenv("HOME", home)

	b := NewSandboxedBackend(agentBinary, []string{"/bin/bash", "-lc"})
	h, err := b.Start(StartOptions{Command: command})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	out, _ := io.ReadAll(h.Stdout())
	res := h.Wait()
	if res.Err != nil {
		t.Fatalf("Wait: %v", res.Err)
	}
	if res.SandboxSetupFailed {
		t.Fatal("sandbox setup itself failed — Landlock unsupported on this kernel?")
	}
	return string(out), res.ExitCode
}

// TestSandboxedBackend_CommandReadResolvesUserManagedPathAndConfig covers
// Issue #31: command_read replaces HOME with an empty scratch directory,
// so a login shell's ~/.profile — and therefore any PATH entries or XDG
// config a user-level tool manager (mise, asdf, ...) sets up there — is
// never sourced, and user-installed CLIs become unresolvable even though
// they work fine from command_exec.
//
// "mytool" here stands in for any such tool manager: nothing in this test
// or its fix is specific to mise.
func TestSandboxedBackend_CommandReadResolvesUserManagedPathAndConfig(t *testing.T) {
	realHome := t.TempDir()
	writeFile(t, filepath.Join(realHome, ".profile"), `export PATH="$HOME/.local/bin:$PATH"`+"\n", 0o644)
	writeFile(t, filepath.Join(realHome, ".local/bin/mytool"), "#!/bin/sh\necho mytool-ran\n", 0o755)
	writeFile(t, filepath.Join(realHome, ".config/mytool/marker"), "trusted-config\n", 0o644)

	stdout, exitCode := runSandboxed(t, realHome, `mytool && cat "$HOME/.config/mytool/marker"`)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stdout=%q) — user-managed CLI/config not resolvable from command_read", exitCode, stdout)
	}
	if stdout != "mytool-ran\ntrusted-config\n" {
		t.Fatalf("stdout = %q, want %q", stdout, "mytool-ran\ntrusted-config\n")
	}
}

// TestSandboxedBackend_CommandReadCannotWriteHostToolManagerConfig covers
// the write side of the same fix: making a user-level tool manager's
// existing config/data readable from command_read must not also make it
// writable — the sandbox must still be unable to mutate host state.
func TestSandboxedBackend_CommandReadCannotWriteHostToolManagerConfig(t *testing.T) {
	realHome := t.TempDir()
	markerPath := filepath.Join(realHome, ".config/mytool/marker")
	writeFile(t, markerPath, "trusted-config\n", 0o644)

	_, exitCode := runSandboxed(t, realHome, `echo hacked > "$HOME/.config/mytool/marker"`)
	if exitCode == 0 {
		t.Fatal("write to host tool-manager config unexpectedly succeeded from inside command_read")
	}
	got, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "trusted-config\n" {
		t.Fatalf("host config was modified: got %q, want unchanged %q", got, "trusted-config\n")
	}
}

// TestSandboxedBackend_CommandReadStillHasWritableScratchHome covers that
// exposing the host's user-managed tool directories doesn't regress the
// pre-existing writable, ephemeral HOME behavior for anything else: a
// brand new file created directly under HOME must still land in the
// isolated scratch area, not be rejected.
func TestSandboxedBackend_CommandReadStillHasWritableScratchHome(t *testing.T) {
	realHome := t.TempDir()

	stdout, exitCode := runSandboxed(t, realHome, `echo -n scratch-data > "$HOME/newfile" && cat "$HOME/newfile"`)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stdout=%q)", exitCode, stdout)
	}
	if stdout != "scratch-data" {
		t.Fatalf("stdout = %q, want %q", stdout, "scratch-data")
	}
	if _, err := os.Stat(filepath.Join(realHome, "newfile")); !os.IsNotExist(err) {
		t.Fatalf("newfile leaked into the real HOME: err=%v", err)
	}
}
