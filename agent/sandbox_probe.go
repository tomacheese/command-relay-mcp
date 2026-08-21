package agent

import "command-relay-mcp/internal/backend"

// ProbeSandbox runs a trivial command through b (expected to be a
// SandboxedBackend) once and reports whether sandbox setup itself
// succeeded. Call this once at Agent startup and cache the result —
// command.read must not re-probe per call (addendum §4's
// capability-wiring note).
func ProbeSandbox(b backend.ProcessBackend) bool {
	h, err := b.Start(backend.StartOptions{Command: "true"})
	if err != nil {
		return false
	}
	res := h.Wait()
	return res.Err == nil && !res.SandboxSetupFailed
}
