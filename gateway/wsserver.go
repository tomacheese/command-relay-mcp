package gateway

import (
	"context"
	"encoding/json"
	"log"
	"net/http"

	"command-relay-mcp/internal/proto"
	"github.com/coder/websocket"
)

// DeviceVerifier checks an Agent's device credential (base spec §6.2).
type DeviceVerifier func(deviceID, secret string) bool

// NewWSServer returns the Agent-facing WebSocket endpoint: accepts the
// connection, reads the hello handshake, verifies the device
// credential, and — on success — registers the AgentConn and serves it
// until it disconnects.
func NewWSServer(reg *Registry, verify DeviceVerifier) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := r.Context()

		_, data, err := ws.Read(ctx)
		if err != nil {
			ws.Close(websocket.StatusProtocolError, "expected hello")
			return
		}
		var hello proto.Hello
		if err := json.Unmarshal(data, &hello); err != nil || hello.Type != proto.TypeHello {
			ws.Close(websocket.StatusProtocolError, "invalid hello")
			return
		}
		if !verify(hello.DeviceID, hello.DeviceSecret) {
			log.Printf("gateway: authentication failure for device %q", hello.DeviceID)
			ws.Close(websocket.StatusPolicyViolation, "invalid device credential")
			return
		}

		conn := newAgentConn(ws, hello)
		reg.Register(conn)
		defer reg.Unregister(conn)
		log.Printf("gateway: agent %s connected (os=%s arch=%s)", hello.DeviceID, hello.OS, hello.Arch)

		if err := conn.readLoop(context.Background()); err != nil {
			log.Printf("gateway: agent %s disconnected: %v", hello.DeviceID, err)
		}
	})
}
