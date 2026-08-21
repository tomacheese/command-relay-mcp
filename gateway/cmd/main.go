// Command gateway is the Gateway entry point. It lives in its own
// subdirectory so package main can coexist with package gateway's own
// files (config.go, registry.go, ...) in the gateway/ directory.
package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os/signal"
	"syscall"

	"command-relay-mcp/gateway"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// protectedResourceMetadataPath is shared between the mux route and the
// WWW-Authenticate resource_metadata URL below so the two can never drift
// apart.
const protectedResourceMetadataPath = "/.well-known/oauth-protected-resource"

func main() {
	cfg := gateway.LoadGatewayConfig()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	reg := gateway.NewRegistry()

	verify := func(deviceID, secret string) bool {
		expected := cfg.AgentSecrets[deviceID]
		return expected != "" && subtle.ConstantTimeCompare([]byte(expected), []byte(secret)) == 1
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
		publicMCPURL, err := url.Parse(cfg.PublicMCPURL)
		if err != nil {
			log.Fatalf("gateway: invalid PUBLIC_MCP_URL: %v", err)
		}
		// authServerIssuer is this Gateway's own origin, not Azure's: the
		// /.well-known/oauth-authorization-server handler below proxies
		// Azure's real metadata from here (see NewAzureADAuthServerMetadataHandler).
		authServerIssuer := publicMCPURL.Scheme + "://" + publicMCPURL.Host
		metadataHandler, err := gateway.NewAzureADAuthServerMetadataHandler(ctx, cfg.OAuthTenantID, authServerIssuer)
		if err != nil {
			log.Fatalf("gateway: azure ad metadata proxy: %v", err)
		}
		resourceMetadataURL := authServerIssuer + protectedResourceMetadataPath
		mux.Handle("/mcp", gateway.NewMCPHTTPHandlerWithVerifier(reg, verifier, cfg.OAuthRequiredScopes, resourceMetadataURL))
		mux.Handle("/.well-known/oauth-authorization-server", metadataHandler)
		mux.Handle(protectedResourceMetadataPath, auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
			Resource:             cfg.PublicMCPURL,
			AuthorizationServers: []string{authServerIssuer},
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
		// reconnecting once this Gateway comes back.
		log.Print("gateway: shutting down, disconnecting agents")
		reg.CloseAll()
		if err := srv.Shutdown(context.Background()); err != nil {
			log.Printf("gateway: server shutdown error: %v", err)
		}
	}()

	log.Printf("gateway: listening on %s", cfg.ListenAddress)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
