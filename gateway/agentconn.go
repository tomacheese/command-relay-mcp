package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"command-relay-mcp/internal/proto"
	"github.com/coder/websocket"
)

// pingInterval is how often readLoop pings the Agent while otherwise
// idle, so infra between Agent and Gateway (reverse proxies, Cloudflare,
// etc.) doesn't silently close the connection on its own idle timeout.
// Overridden by tests to a much shorter interval.
var pingInterval = 30 * time.Second

// pingTimeout bounds how long a single keepalive ping waits for its pong
// before being treated as a failed connection. Overridden by tests.
var pingTimeout = 10 * time.Second

// AgentConn wraps one Agent's WebSocket connection and multiplexes
// Gateway-initiated RPCs on it, correlated by request_id.
type AgentConn struct {
	deviceID     string
	os, arch     string
	capabilities proto.Capabilities

	ws      *websocket.Conn
	writeMu sync.Mutex // coder/websocket requires concurrent writers to serialize themselves
	nextID  atomic.Uint64
	mu      sync.Mutex
	pending map[string]chan *proto.Response
	closed  chan struct{}
}

func newAgentConn(ws *websocket.Conn, hello proto.Hello) *AgentConn {
	return &AgentConn{
		deviceID: hello.DeviceID, os: hello.OS, arch: hello.Arch, capabilities: hello.Capabilities,
		ws: ws, pending: make(map[string]chan *proto.Response), closed: make(chan struct{}),
	}
}

func (c *AgentConn) DeviceID() string { return c.deviceID }

// Call sends a request and blocks for the matching response, ctx
// cancellation, or connection loss: transport loss maps to
// execution_unknown, which the MCP layer surfaces as an error here.
func (c *AgentConn) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	req, err := proto.NewRequest(method, params)
	if err != nil {
		return nil, err
	}
	req.RequestID = fmt.Sprintf("%d", c.nextID.Add(1))

	ch := make(chan *proto.Response, 1)
	c.mu.Lock()
	c.pending[req.RequestID] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, req.RequestID)
		c.mu.Unlock()
	}()

	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	c.writeMu.Lock()
	writeErr := c.ws.Write(ctx, websocket.MessageText, data)
	c.writeMu.Unlock()
	if writeErr != nil {
		return nil, writeErr
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-ctx.Done():
		// This is the "transport timeout" case — the request/response round
		// trip itself didn't complete in time, distinct from a
		// command/process wait timeout (which surfaces as timed_out:true in
		// the result, not a protocol error).
		return nil, &proto.RPCError{Code: proto.ErrTimeout, Message: "RPC timed out waiting for a response"}
	case <-c.closed:
		log.Printf("gateway: transport failure: connection to device %q lost mid-call for %s", c.deviceID, method)
		if proto.IsMutatingMethod(method) {
			return nil, &proto.RPCError{Code: proto.ErrExecutionUnknown, Message: "connection lost while an execution was in flight; its outcome is unknown"}
		}
		return nil, &proto.RPCError{Code: proto.ErrTransportLost, Message: "the underlying transport was lost"}
	}
}

// readLoop dispatches incoming responses to their waiting Call; it owns
// the read side of the connection until it errors or ctx is done.
func (c *AgentConn) readLoop(ctx context.Context) error {
	defer close(c.closed)
	pingCtx, cancelPing := context.WithCancel(ctx)
	defer cancelPing()
	go c.pingLoop(pingCtx)
	for {
		_, data, err := c.ws.Read(ctx)
		if err != nil {
			return err
		}
		var resp proto.Response
		if err := json.Unmarshal(data, &resp); err != nil {
			log.Printf("gateway: malformed RPC response from device %q: %v", c.deviceID, err)
			continue
		}
		c.mu.Lock()
		ch, ok := c.pending[resp.RequestID]
		c.mu.Unlock()
		if ok {
			ch <- &resp
		}
	}
}

// pingLoop periodically pings the Agent so idle infra between Agent and
// Gateway doesn't silently close the connection (see readLoop). A failed
// ping closes the connection immediately so readLoop's blocked ws.Read
// returns instead of waiting for the read to hang indefinitely on a
// transport that's already dead.
func (c *AgentConn) pingLoop(ctx context.Context) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
			err := c.ws.Ping(pingCtx)
			cancel()
			if err != nil {
				log.Printf("gateway: keepalive ping to device %q failed: %v", c.deviceID, err)
				c.ws.CloseNow()
				return
			}
		}
	}
}

func (c *AgentConn) Close() error {
	return c.ws.Close(websocket.StatusNormalClosure, "closing")
}
