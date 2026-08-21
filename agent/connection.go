package agent

import (
	"context"
	"encoding/json"
	"log"
	"math/rand"
	"runtime"
	"sync"
	"time"

	"command-relay-mcp/internal/proto"
	"github.com/coder/websocket"
)

type Connection struct {
	cfg          Config
	dispatcher   *Dispatcher
	capabilities proto.Capabilities
}

func NewConnection(cfg Config, d *Dispatcher, caps proto.Capabilities) *Connection {
	return &Connection{cfg: cfg, dispatcher: d, capabilities: caps}
}

// Run connects, handshakes, serves requests until the connection drops,
// then reconnects with exponential backoff and jitter (base spec §5.1)
// until ctx is cancelled.
func (c *Connection) Run(ctx context.Context) error {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := c.runOnce(ctx); err != nil {
			log.Printf("agent: connection lost: %v (retrying in %s)", err, backoff)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff + time.Duration(rand.Int63n(int64(backoff)/2+1))):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

func (c *Connection) runOnce(ctx context.Context) error {
	ws, _, err := websocket.Dial(ctx, c.cfg.GatewayURL, nil)
	if err != nil {
		return err
	}
	defer ws.CloseNow()

	hello := proto.Hello{
		Type: proto.TypeHello, DeviceID: c.cfg.DeviceID, DeviceSecret: c.cfg.DeviceSecret,
		AgentVersion: c.cfg.AgentVersion, OS: runtime.GOOS, Arch: runtime.GOARCH,
		Capabilities: c.capabilities,
	}
	helloData, err := json.Marshal(hello)
	if err != nil {
		return err
	}
	if err := ws.Write(ctx, websocket.MessageText, helloData); err != nil {
		return err
	}
	log.Printf("agent: connected to gateway %s", c.cfg.GatewayURL)

	// base spec §5.1/§16.1: multiple RPCs are multiplexed on this one
	// connection, so a slow handler (e.g. command.exec/command.read/
	// process.wait blocking for up to their own timeout) must never stall
	// dispatch of other in-flight requests — each request runs in its own
	// goroutine. writeMu only serializes the write side; coder/websocket's
	// Conn.Read/Write are each independently safe for one concurrent
	// caller, but concurrent writers must serialize themselves.
	var writeMu sync.Mutex
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return err
		}
		var req proto.Request
		if err := json.Unmarshal(data, &req); err != nil {
			continue // malformed frame; base spec has no wire-level nack, just skip
		}
		wg.Add(1)
		go func(req proto.Request) {
			defer wg.Done()
			resp := c.dispatcher.Dispatch(ctx, &req)
			respData, err := json.Marshal(resp)
			if err != nil {
				return
			}
			writeMu.Lock()
			writeErr := ws.Write(ctx, websocket.MessageText, respData)
			writeMu.Unlock()
			if writeErr != nil {
				// The connection is dead; force the blocked ws.Read above
				// to return promptly so Run() reconnects instead of
				// leaving it hanging on a transport that already failed.
				ws.CloseNow()
			}
		}(req)
	}
}
