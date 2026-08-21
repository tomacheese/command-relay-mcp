// Command gateway is the Gateway entry point. It lives in its own
// subdirectory so package main can coexist with package gateway's own
// files (config.go, registry.go, ...) in the gateway/ directory.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os/signal"
	"syscall"

	"command-relay-mcp/gateway"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

func main() {
	cfg := gateway.LoadGatewayConfig()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	reg := gateway.NewRegistry()

	verify := func(deviceID, secret string) bool {
		return cfg.AgentSecrets[deviceID] != "" && cfg.AgentSecrets[deviceID] == secret
	}

	mux := http.NewServeMux()
	mux.Handle("/agent/ws", gateway.NewWSServer(reg, verify))

	if cfg.OAuthTenantID != "" && cfg.OAuthAudience != "" {
		if cfg.PublicMCPURL == "" {
			log.Fatal("PUBLIC_MCP_URL must be set when AZURE_TENANT_ID + AZURE_AUDIENCE are set: the protected-resource metadata's resource field (RFC 9728 §2) needs the externally reachable /mcp URL, which ListenAddress (a local bind address) cannot provide")
		}
		verifier, err := gateway.NewAzureADVerifier(ctx, cfg.OAuthTenantID, cfg.OAuthAudience)
		if err != nil {
			log.Fatalf("gateway: azure ad discovery: %v", err)
		}
		mux.Handle("/mcp", gateway.NewMCPHTTPHandlerWithVerifier(reg, verifier))
		mux.Handle("/.well-known/oauth-protected-resource", auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
			Resource:             cfg.PublicMCPURL,
			AuthorizationServers: []string{"https://login.microsoftonline.com/" + cfg.OAuthTenantID + "/v2.0"},
			ScopesSupported:      []string{cfg.OAuthAudience},
		}))
		log.Print("gateway: MCP endpoint protected by Azure AD OAuth")
	} else {
		if cfg.BearerToken == "" {
			log.Fatal("MCP_BEARER_TOKEN must be set (or AZURE_TENANT_ID + AZURE_AUDIENCE for OAuth)")
		}
		mux.Handle("/mcp", gateway.NewMCPHTTPHandler(reg, cfg.BearerToken))
	}

	srv := &http.Server{Addr: cfg.ListenAddress, Handler: mux}

	go func() {
		<-ctx.Done()
		// http.Server.Shutdown does not know about hijacked WebSocket
		// connections, so tell every Agent to close explicitly: without
		// this, an Agent's read would hang instead of erroring and
		// reconnecting once this Gateway comes back (base spec §27
		// acceptance criterion #7).
		log.Print("gateway: shutting down, disconnecting agents")
		reg.CloseAll()
		_ = srv.Shutdown(context.Background())
	}()

	log.Printf("gateway: listening on %s", cfg.ListenAddress)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
