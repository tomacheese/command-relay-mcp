package backend

import (
	"io"
	"testing"
	"time"
)

func TestLinuxBackend_RunsAndCapturesOutput(t *testing.T) {
	b := NewLinuxBackend([]string{"/bin/bash", "-lc"})
	h, err := b.Start(StartOptions{Command: "echo hello; echo err-line 1>&2"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	stdout, _ := io.ReadAll(h.Stdout())
	stderr, _ := io.ReadAll(h.Stderr())
	res := h.Wait()
	if res.Err != nil {
		t.Fatalf("Wait err: %v", res.Err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d", res.ExitCode)
	}
	if string(stdout) != "hello\n" {
		t.Fatalf("stdout = %q", stdout)
	}
	if string(stderr) != "err-line\n" {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestLinuxBackend_TerminateKillsProcessGroup(t *testing.T) {
	b := NewLinuxBackend([]string{"/bin/bash", "-lc"})
	// Spawns a child that would outlive a naive single-PID kill.
	h, err := b.Start(StartOptions{Command: "sleep 30 & wait"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	done := make(chan ExitResult, 1)
	go func() { done <- h.Wait() }()

	time.Sleep(200 * time.Millisecond)
	if err := h.Terminate(500); err != nil {
		t.Fatalf("Terminate: %v", err)
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("process did not exit after Terminate")
	}
}

// TestLinuxBackend_EnvOverridesOnlySpecifiedKeys covers that
// StartOptions.Env must override/add only the given keys, not replace
// the Agent service's whole environment.
func TestLinuxBackend_EnvOverridesOnlySpecifiedKeys(t *testing.T) {
	t.Setenv("RC_TEST_INHERITED", "inherited-value")
	b := NewLinuxBackend([]string{"/bin/bash", "-lc"})
	h, err := b.Start(StartOptions{
		Command: `echo "inherited=$RC_TEST_INHERITED overridden=$RC_TEST_OVERRIDE"`,
		Env:     map[string]string{"RC_TEST_OVERRIDE": "override-value"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	stdout, _ := io.ReadAll(h.Stdout())
	if res := h.Wait(); res.Err != nil || res.ExitCode != 0 {
		t.Fatalf("Wait: exitCode=%d err=%v", res.ExitCode, res.Err)
	}
	if want := "inherited=inherited-value overridden=override-value\n"; string(stdout) != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func TestLinuxBackend_NoEnvOptionInheritsAgentEnvironment(t *testing.T) {
	t.Setenv("RC_TEST_INHERITED", "still-here")
	b := NewLinuxBackend([]string{"/bin/bash", "-lc"})
	h, err := b.Start(StartOptions{Command: "echo $RC_TEST_INHERITED"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	stdout, _ := io.ReadAll(h.Stdout())
	if res := h.Wait(); res.Err != nil || res.ExitCode != 0 {
		t.Fatalf("Wait: exitCode=%d err=%v", res.ExitCode, res.Err)
	}
	if string(stdout) != "still-here\n" {
		t.Fatalf("stdout = %q", stdout)
	}
}
