package agent

import (
	"os"
	"reflect"
	"testing"
	"time"
)

// TestLoadConfig_AppliesSpecDefaults covers the documented defaults
// (finished_process_ttl=1h, stdout/stderr_buffer_bytes=4MiB,
// history_retention=30d) when the corresponding env vars are unset.
func TestLoadConfig_AppliesSpecDefaults(t *testing.T) {
	for _, key := range []string{"DEFAULT_SHELL", "FINISHED_PROCESS_TTL", "HISTORY_RETENTION", "STDOUT_BUFFER_BYTES", "STDERR_BUFFER_BYTES"} {
		os.Unsetenv(key)
	}
	cfg := LoadConfig()
	if !reflect.DeepEqual(cfg.DefaultShell, []string{"/bin/bash", "-lc"}) {
		t.Fatalf("DefaultShell = %v", cfg.DefaultShell)
	}
	if cfg.FinishedProcessTTL != time.Hour {
		t.Fatalf("FinishedProcessTTL = %v, want 1h", cfg.FinishedProcessTTL)
	}
	if cfg.HistoryRetention != 30*24*time.Hour {
		t.Fatalf("HistoryRetention = %v, want 30d", cfg.HistoryRetention)
	}
	if cfg.StdoutBufferBytes != 4<<20 || cfg.StderrBufferBytes != 4<<20 {
		t.Fatalf("buffer sizes = %d/%d, want 4MiB each", cfg.StdoutBufferBytes, cfg.StderrBufferBytes)
	}
}

// TestLoadConfig_ReadsOverridesFromEnv covers that default_shell,
// finished_process_ttl, and history_retention are all configurable via
// environment variables.
func TestLoadConfig_ReadsOverridesFromEnv(t *testing.T) {
	t.Setenv("DEFAULT_SHELL", "/bin/sh -c")
	t.Setenv("FINISHED_PROCESS_TTL", "2h")
	t.Setenv("HISTORY_RETENTION", "168h")

	cfg := LoadConfig()
	if !reflect.DeepEqual(cfg.DefaultShell, []string{"/bin/sh", "-c"}) {
		t.Fatalf("DefaultShell = %v", cfg.DefaultShell)
	}
	if cfg.FinishedProcessTTL != 2*time.Hour {
		t.Fatalf("FinishedProcessTTL = %v, want 2h", cfg.FinishedProcessTTL)
	}
	if cfg.HistoryRetention != 168*time.Hour {
		t.Fatalf("HistoryRetention = %v, want 168h", cfg.HistoryRetention)
	}
}
