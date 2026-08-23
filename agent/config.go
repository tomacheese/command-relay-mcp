package agent

import (
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"command-relay-mcp/internal/version"
)

// Config holds every Agent setting, sourced from environment variables.
type Config struct {
	DeviceID           string
	DeviceSecret       string
	GatewayURL         string
	DefaultShell       []string // e.g. []string{"/bin/bash", "-lc"}
	StdoutBufferBytes  int
	StderrBufferBytes  int
	FinishedProcessTTL time.Duration
	HistoryRetention   time.Duration
	AgentVersion       string
	AutoUpdateEnabled  bool
	AutoUpdateInterval time.Duration
}

// LoadConfig reads every Agent setting from the environment, applying
// documented defaults. DeviceID, DeviceSecret, and GatewayURL have no
// default — the caller must validate they were provided (mustEnv in
// agent/cmd/main.go).
func LoadConfig() Config {
	return Config{
		DeviceID:           os.Getenv("DEVICE_ID"),
		DeviceSecret:       os.Getenv("DEVICE_SECRET"),
		GatewayURL:         os.Getenv("GATEWAY_URL"),
		DefaultShell:       parseShellCommand(envOr("DEFAULT_SHELL", "/bin/bash -lc")),
		StdoutBufferBytes:  envIntOr("STDOUT_BUFFER_BYTES", 4<<20),
		StderrBufferBytes:  envIntOr("STDERR_BUFFER_BYTES", 4<<20),
		FinishedProcessTTL: envDurationOr("FINISHED_PROCESS_TTL", time.Hour),
		HistoryRetention:   envDurationOr("HISTORY_RETENTION", 30*24*time.Hour),
		AgentVersion:       version.Version,
		AutoUpdateEnabled:  envBoolOr("AUTO_UPDATE_ENABLED", true),
		AutoUpdateInterval: envDurationOr("AUTO_UPDATE_INTERVAL", 6*time.Hour),
	}
}

// parseShellCommand splits a whitespace-separated shell invocation, e.g.
// "/bin/bash -lc" -> []string{"/bin/bash", "-lc"}. Shell paths containing
// spaces are not supported — no V1 deployment needs one.
func parseShellCommand(s string) []string {
	return strings.Fields(s)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIntOr(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		} else {
			log.Printf("agent: invalid value for %s=%q, using default %d: %v", key, v, fallback, err)
		}
	}
	return fallback
}

func envBoolOr(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		} else {
			log.Printf("agent: invalid value for %s=%q, using default %t: %v", key, v, fallback, err)
		}
	}
	return fallback
}

func envDurationOr(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		} else {
			log.Printf("agent: invalid value for %s=%q, using default %s: %v", key, v, fallback, err)
		}
	}
	return fallback
}
