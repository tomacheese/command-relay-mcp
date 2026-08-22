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
	"os/signal"
	"syscall"

	"command-relay-mcp/gateway"
)

func main() {
	cfg := gateway.LoadGatewayConfig()
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	reg := gateway.NewRegistry()

	verify := func(secret string) bool {
		return cfg.AgentSecret != "" && subtle.ConstantTimeCompare([]byte(cfg.AgentSecret), []byte(secret)) == 1
	}

	log.Print("gateway: MCP endpoint has NO AUTHENTICATION — do not expose it to an untrusted network")

	agentSrv := &http.Server{Addr: cfg.AgentListenAddress, Handler: gateway.NewWSServer(reg, verify)}
	mcpSrv := &http.Server{Addr: cfg.MCPListenAddress, Handler: gateway.NewMCPHTTPHandlerNoAuth(reg)}

	go func() {
		<-ctx.Done()
		// http.Server.Shutdown does not know about hijacked WebSocket
		// connections, so tell every Agent to close explicitly: without
		// this, an Agent's read would hang instead of erroring and
		// reconnecting once this Gateway comes back.
		log.Print("gateway: shutting down, disconnecting agents")
		reg.CloseAll()
		if err := agentSrv.Shutdown(context.Background()); err != nil {
			log.Printf("gateway: agent server shutdown error: %v", err)
		}
		if err := mcpSrv.Shutdown(context.Background()); err != nil {
			log.Printf("gateway: mcp server shutdown error: %v", err)
		}
	}()

	go func() {
		log.Printf("gateway: agent endpoint listening on %s", cfg.AgentListenAddress)
		if err := agentSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	log.Printf("gateway: mcp endpoint listening on %s", cfg.MCPListenAddress)
	if err := mcpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
