package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"

	"command-relay-mcp/internal/proto"
	"github.com/coder/websocket"
)

// AgentConn wraps one Agent's WebSocket connection and multiplexes
// Gateway-initiated RPCs on it, correlated by request_id (base spec
// §5.1, §16.5).
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
// cancellation, or connection loss (base spec §17: transport loss maps
// to execution_unknown, which the MCP layer surfaces as an error here).
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
		// base spec §18.3's "transport timeout" case — the request/response
		// round trip itself didn't complete in time, distinct from a
		// command/process wait timeout (which surfaces as timed_out:true in
		// the result, not a protocol error).
		return nil, errors.New(proto.ErrTimeout)
	case <-c.closed:
		log.Printf("gateway: transport failure: connection to device %q lost mid-call for %s", c.deviceID, method)
		if proto.IsMutatingMethod(method) {
			return nil, errors.New(proto.ErrExecutionUnknown)
		}
		return nil, errors.New(proto.ErrTransportLost)
	}
}

// readLoop dispatches incoming responses to their waiting Call; it owns
// the read side of the connection until it errors or ctx is done.
func (c *AgentConn) readLoop(ctx context.Context) error {
	defer close(c.closed)
	for {
		_, data, err := c.ws.Read(ctx)
		if err != nil {
			return err
		}
		var resp proto.Response
		if err := json.Unmarshal(data, &resp); err != nil {
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

func (c *AgentConn) Close() error {
	return c.ws.Close(websocket.StatusNormalClosure, "closing")
}
