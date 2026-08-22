package gateway

import (
	"log"
	"os"
	"strings"
)

// GatewayConfig holds every Gateway setting, sourced from environment
// variables.
type GatewayConfig struct {
	ListenAddress string
	AgentSecrets  map[string]string // device_id -> device_secret, supporting multiple devices
}

func LoadGatewayConfig() GatewayConfig {
	return GatewayConfig{
		ListenAddress: envOr("LISTEN_ADDRESS", ":8080"),
		AgentSecrets:  parseAgentSecrets(os.Getenv("AGENT_DEVICE_SECRETS")),
	}
}

// parseAgentSecrets parses the AGENT_DEVICE_SECRETS env var, formatted as
// comma-separated "device_id:device_secret" pairs — a single fixed pair
// cannot register more than one Agent. Entries without a ":" or with an
// empty device id are skipped, with a warning logged (identifying only
// the entry's position, never its value, so a malformed secret is
// never itself logged).
func parseAgentSecrets(env string) map[string]string {
	out := make(map[string]string)
	if env == "" {
		return out
	}
	for i, entry := range strings.Split(env, ",") {
		id, secret, ok := strings.Cut(entry, ":")
		if !ok || id == "" {
			log.Printf("gateway: skipping malformed AGENT_DEVICE_SECRETS entry #%d (want \"device_id:device_secret\")", i)
			continue
		}
		out[id] = secret
	}
	return out
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
