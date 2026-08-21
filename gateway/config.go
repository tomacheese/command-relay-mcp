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
	// PublicMCPURL is the externally reachable URL of the /mcp endpoint —
	// distinct from ListenAddress, which is only a local bind address
	// (e.g. ":8080") and is not a valid resource identifier on its own.
	// Required when OAuth is enabled, to advertise the
	// protected-resource metadata's "resource" field (RFC 9728 §2)
	// correctly.
	PublicMCPURL string
	BearerToken  string
	AgentSecrets map[string]string // device_id -> device_secret, supporting multiple devices

	// OAuthTenantID / OAuthAudience configure Azure AD verification
	// (addendum §2). Both must be set to enable it; otherwise the
	// Gateway falls back to BearerToken.
	OAuthTenantID string
	OAuthAudience string
	// OAuthRequiredScopes, when non-empty, are enforced against the
	// Azure AD token's own "scp" claim by NewMCPHTTPHandlerWithVerifier
	// — every listed scope must be present, or the request is rejected
	// before it reaches any tool. Empty (the default) means any
	// audience-valid token is trusted for every tool, matching this
	// project's intentional V1 design of audience-only auth with no
	// per-tool RBAC.
	OAuthRequiredScopes []string
}

func LoadGatewayConfig() GatewayConfig {
	return GatewayConfig{
		ListenAddress:       envOr("LISTEN_ADDRESS", ":8080"),
		PublicMCPURL:        os.Getenv("PUBLIC_MCP_URL"),
		BearerToken:         os.Getenv("MCP_BEARER_TOKEN"),
		AgentSecrets:        parseAgentSecrets(os.Getenv("AGENT_DEVICE_SECRETS")),
		OAuthTenantID:       os.Getenv("AZURE_TENANT_ID"),
		OAuthAudience:       os.Getenv("AZURE_AUDIENCE"),
		OAuthRequiredScopes: strings.Fields(os.Getenv("AZURE_REQUIRED_SCOPES")),
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
