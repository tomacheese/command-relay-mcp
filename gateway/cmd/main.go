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

	verify := func(deviceID, secret string) bool {
		expected := cfg.AgentSecrets[deviceID]
		return expected != "" && subtle.ConstantTimeCompare([]byte(expected), []byte(secret)) == 1
	}

	mux := http.NewServeMux()
	mux.Handle("/agent/ws", gateway.NewWSServer(reg, verify))
	mux.Handle("/mcp", gateway.NewMCPHTTPHandlerNoAuth(reg))
	log.Print("gateway: MCP endpoint has NO AUTHENTICATION (temporary, see project history)")

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
