package gateway

import (
	"os"
)

// GatewayConfig holds every Gateway setting, sourced from environment
// variables. MCPListenAddress and AgentListenAddress are separate ports so
// an operator can expose /mcp (no authentication) through a public
// tunnel while keeping /agent/ws on a port scoped to Agents only.
type GatewayConfig struct {
	MCPListenAddress   string
	AgentListenAddress string
	AgentSecret        string // shared secret every Agent must present
}

func LoadGatewayConfig() GatewayConfig {
	return GatewayConfig{
		MCPListenAddress:   envOr("MCP_LISTEN_ADDRESS", ":8080"),
		AgentListenAddress: envOr("AGENT_LISTEN_ADDRESS", ":8081"),
		AgentSecret:        os.Getenv("AGENT_SHARED_SECRET"),
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
