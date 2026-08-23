package agent

import (
	"context"
	"encoding/json"
	"log"
	"math/rand"
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	"command-relay-mcp/internal/proto"
	"github.com/coder/websocket"
)

// pingInterval is how often runOnce pings the Gateway while otherwise
// idle. Infra between Agent and Gateway (reverse proxies, Cloudflare,
// etc.) can silently close an idle connection without this.
// Overridden by tests to a much shorter interval.
var pingInterval = 30 * time.Second

// pingTimeout bounds how long a single keepalive ping waits for its pong
// before being treated as a failed connection. Overridden by tests.
var pingTimeout = 10 * time.Second

type Connection struct {
	cfg          Config
	dispatcher   *Dispatcher
	capabilities proto.Capabilities
	// wg tracks every in-flight per-request handler goroutine across
	// reconnects. It is owned by Connection, not scoped to a single
	// runOnce call, so a slow handler left over from a dead connection
	// can never block the reconnect loop from dialing a new one — only
	// Run()'s own shutdown path waits for it.
	wg sync.WaitGroup
}

func NewConnection(cfg Config, d *Dispatcher, caps proto.Capabilities) *Connection {
	return &Connection{cfg: cfg, dispatcher: d, capabilities: caps}
}

// Run connects, handshakes, serves requests until the connection drops,
// then reconnects with exponential backoff and jitter until ctx is
// cancelled. It waits for every in-flight per-request handler goroutine
// to finish before returning, so the caller can safely terminate
// managed processes only once handling has fully drained.
func (c *Connection) Run(ctx context.Context) error {
	defer c.wg.Wait()
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

	pingCtx, cancelPing := context.WithCancel(ctx)
	defer cancelPing()
	go c.pingLoop(pingCtx, ws)

	// Multiple RPCs are multiplexed on this one connection, so a slow
	// handler (e.g. command.exec/command.read/process.wait blocking for
	// up to their own timeout) must never stall dispatch of other
	// in-flight requests — each request runs in its own goroutine,
	// tracked by c.wg rather than a WaitGroup scoped to this call (see
	// the Connection.wg field doc). writeMu only serializes the write
	// side; coder/websocket's Conn.Read/Write are each independently
	// safe for one concurrent caller, but concurrent writers must
	// serialize themselves.
	var writeMu sync.Mutex
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return err
		}
		var req proto.Request
		if err := json.Unmarshal(data, &req); err != nil {
			continue // malformed frame; no wire-level nack defined, just skip
		}
		c.wg.Add(1)
		go func(req proto.Request) {
			defer c.wg.Done()
			defer func() {
				if r := recover(); r != nil {
					log.Printf("agent: recovered panic handling request %s (%s): %v\n%s", req.RequestID, req.Method, r, debug.Stack())
				}
			}()
			resp := c.dispatcher.Dispatch(ctx, &req)
			respData, err := json.Marshal(resp)
			if err != nil {
				log.Printf("agent: failed to marshal response for request %s: %v", req.RequestID, err)
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

// pingLoop periodically pings the Gateway so idle infra between Agent
// and Gateway doesn't silently close the connection (see runOnce). A
// failed ping closes the connection immediately. This makes runOnce's
// blocked ws.Read return and Run reconnect, instead of waiting for the
// read to hang indefinitely on a transport that's already dead.
func (c *Connection) pingLoop(ctx context.Context, ws *websocket.Conn) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
			err := ws.Ping(pingCtx)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return // runOnce is already shutting down for an unrelated reason
				}
				log.Printf("agent: keepalive ping failed: %v", err)
				ws.CloseNow()
				return
			}
		}
	}
}
