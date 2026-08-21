package gateway

import (
	"os"
	"strings"
)

// GatewayConfig holds every Gateway setting from base spec §23.
type GatewayConfig struct {
	ListenAddress string
	// PublicMCPURL is the externally reachable URL of the /mcp endpoint
	// (base spec §23's public_mcp_url) — distinct from ListenAddress,
	// which is only a local bind address (e.g. ":8080") and is not a
	// valid resource identifier on its own. Required when OAuth is
	// enabled, to advertise the protected-resource metadata's
	// "resource" field (RFC 9728 §2) correctly.
	PublicMCPURL string
	BearerToken  string
	AgentSecrets map[string]string // device_id -> device_secret (base spec §1: multiple devices)

	// OAuthTenantID / OAuthAudience configure Azure AD verification
	// (addendum §2). Both must be set to enable it; otherwise the
	// Gateway falls back to BearerToken.
	OAuthTenantID string
	OAuthAudience string
}

func LoadGatewayConfig() GatewayConfig {
	return GatewayConfig{
		ListenAddress: envOr("LISTEN_ADDRESS", ":8080"),
		PublicMCPURL:  os.Getenv("PUBLIC_MCP_URL"),
		BearerToken:   os.Getenv("MCP_BEARER_TOKEN"),
		AgentSecrets:  parseAgentSecrets(os.Getenv("AGENT_DEVICE_SECRETS")),
		OAuthTenantID: os.Getenv("AZURE_TENANT_ID"),
		OAuthAudience: os.Getenv("AZURE_AUDIENCE"),
	}
}

// parseAgentSecrets parses the AGENT_DEVICE_SECRETS env var, formatted as
// comma-separated "device_id:device_secret" pairs (base spec §1's
// multi-device management goal — a single fixed pair cannot register more
// than one Agent). Entries without a ":" are skipped.
func parseAgentSecrets(env string) map[string]string {
	out := make(map[string]string)
	for _, entry := range strings.Split(env, ",") {
		id, secret, ok := strings.Cut(entry, ":")
		if !ok || id == "" {
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
