package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"command-relay-mcp/internal/proto"
	"github.com/coder/websocket"
)

// TestMain shortens the keepalive interval/timeout once, on the main
// test goroutine, before any pingLoop goroutine exists. Every later read
// of these package vars from a spawned pingLoop is then safely ordered
// after this write, avoiding a data race against a per-test set/restore.
func TestMain(m *testing.M) {
	pingInterval = 20 * time.Millisecond
	pingTimeout = 20 * time.Millisecond
	os.Exit(m.Run())
}

// syncBuffer is a mutex-guarded bytes.Buffer. TestAgentConn_CallOnTransportLoss
// captures log output while a keepalive pingLoop goroutine may still be
// running in the background — TestMain's shortened pingInterval makes this
// window reachable within the test. A plain bytes.Buffer would race between
// that goroutine's log write and the test's own read of the buffer.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// dialRegisteredDevice dials, hellos, and waits for registration,
// returning the client-side connection so the test can control exactly
// when the transport dies mid-call.
func dialRegisteredDevice(t *testing.T, srv *httptest.Server, reg *Registry, deviceID string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+srv.URL[len("http"):], nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	hello := proto.Hello{Type: proto.TypeHello, DeviceID: deviceID, DeviceSecret: "s3cr3t", OS: "linux", Arch: "amd64"}
	data, _ := json.Marshal(hello)
	if err := ws.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write hello: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := reg.Get(deviceID); ok {
			return ws
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("device never appeared in registry")
	return nil
}

// TestAgentConn_CallOnCtxTimeoutReturnsTimeoutCode covers that a
// request/response round trip that doesn't complete before its context
// deadline is a "transport timeout", distinct from a command/process
// wait timeout — it must surface proto.ErrTimeout, not a bare context
// error the caller can't map to any of the required error codes.
func TestAgentConn_CallOnCtxTimeoutReturnsTimeoutCode(t *testing.T) {
	reg := NewRegistry()
	verify := func(secret string) bool { return secret == "s3cr3t" }
	srv := httptest.NewServer(NewWSServer(reg, verify))
	defer srv.Close()

	clientWS := dialRegisteredDevice(t, srv, reg, "device-timeout")
	defer clientWS.CloseNow()
	conn, _ := reg.Get("device-timeout")
	// Keep reading in the background so the Gateway's keepalive pings get
	// answered instead of tearing down the transport before the ctx
	// deadline fires (TestMain's shortened pingInterval applies here too).
	// CloseRead can't be used here since it closes the connection on any
	// data frame, and the Gateway's dispatched request is one.
	go func() {
		for {
			if _, _, err := clientWS.Read(context.Background()); err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	// The Agent never responds, so the deadline fires first.
	_, err := conn.Call(ctx, proto.MethodDevicePing, struct{}{})
	rpcErr, ok := err.(*proto.RPCError)
	if !ok || rpcErr.Code != proto.ErrTimeout {
		t.Fatalf("Call error = %v, want code %q", err, proto.ErrTimeout)
	}
}

// TestAgentConn_ConcurrentCallsDoNotCorruptTransport covers that
// multiple RPCs to the same device must be multiplexable on one
// connection, which requires every concurrent Call to serialize its
// own ws.Write — coder/websocket requires callers to serialize concurrent
// writers themselves (see agent/connection.go's symmetric writeMu on the
// Agent side of the same connection).
func TestAgentConn_ConcurrentCallsDoNotCorruptTransport(t *testing.T) {
	reg := NewRegistry()
	verify := func(secret string) bool { return secret == "s3cr3t" }
	srv := httptest.NewServer(NewWSServer(reg, verify))
	defer srv.Close()

	clientWS := dialRegisteredDevice(t, srv, reg, "device-concurrent")
	defer clientWS.CloseNow()
	conn, _ := reg.Get("device-concurrent")

	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx := context.Background()
		for {
			_, data, err := clientWS.Read(ctx)
			if err != nil {
				return
			}
			var req proto.Request
			if err := json.Unmarshal(data, &req); err != nil {
				continue
			}
			respData, _ := json.Marshal(proto.Response{Type: proto.TypeResponse, RequestID: req.RequestID, Result: json.RawMessage(`{}`)})
			if err := clientWS.Write(ctx, websocket.MessageText, respData); err != nil {
				return
			}
		}
	}()

	const n = 20
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			_, err := conn.Call(context.Background(), proto.MethodDevicePing, struct{}{})
			errCh <- err
		}()
	}
	for i := 0; i < n; i++ {
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("Call: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for concurrent calls")
		}
	}
	clientWS.CloseNow()
	<-done
}

// TestAgentConn_CallOnTransportLoss covers that a transport loss while
// a mutating request is in flight must surface as execution_unknown
// (the Gateway cannot tell whether the Agent applied it), while a
// non-mutating request surfaces the plain transport_lost.
func TestAgentConn_CallOnTransportLoss(t *testing.T) {
	cases := []struct {
		method   string
		wantCode string
	}{
		{proto.MethodProcessStart, proto.ErrExecutionUnknown},
		{proto.MethodDevicePing, proto.ErrTransportLost},
	}

	for _, tc := range cases {
		t.Run(tc.method, func(t *testing.T) {
			reg := NewRegistry()
			verify := func(secret string) bool { return secret == "s3cr3t" }
			srv := httptest.NewServer(NewWSServer(reg, verify))
			defer srv.Close()

			clientWS := dialRegisteredDevice(t, srv, reg, "device-"+tc.method)
			defer clientWS.CloseNow()
			conn, _ := reg.Get("device-" + tc.method)

			var logBuf syncBuffer
			log.SetOutput(&logBuf)
			defer log.SetOutput(os.Stderr)

			errCh := make(chan error, 1)
			go func() {
				_, err := conn.Call(context.Background(), tc.method, struct{}{})
				errCh <- err
			}()

			// Let the Gateway actually send the request, then sever the
			// transport without ever responding.
			readCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if _, _, err := clientWS.Read(readCtx); err != nil {
				t.Fatalf("expected to receive the dispatched request: %v", err)
			}
			clientWS.CloseNow()

			select {
			case err := <-errCh:
				rpcErr, ok := err.(*proto.RPCError)
				if !ok || rpcErr.Code != tc.wantCode {
					t.Fatalf("Call error = %v, want code %q", err, tc.wantCode)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("Call did not return after transport loss")
			}

			// This must be logged as a distinct "transport failure" category,
			// not just silently mapped to an error code.
			if !strings.Contains(logBuf.String(), "transport failure") {
				t.Fatalf("log output = %q, want it to mention \"transport failure\"", logBuf.String())
			}
		})
	}
}

// TestAgentConn_SendsKeepalivePingsWhileIdle covers that readLoop sends
// periodic WebSocket pings even when no RPC traffic is flowing. Idle
// infra between Agent and Gateway can otherwise silently close the
// connection.
func TestAgentConn_SendsKeepalivePingsWhileIdle(t *testing.T) {
	reg := NewRegistry()
	verify := func(secret string) bool { return secret == "s3cr3t" }
	srv := httptest.NewServer(NewWSServer(reg, verify))
	defer srv.Close()

	pings := make(chan struct{}, 8)
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer dialCancel()
	clientWS, _, err := websocket.Dial(dialCtx, "ws"+srv.URL[len("http"):], &websocket.DialOptions{
		OnPingReceived: func(ctx context.Context, payload []byte) bool {
			select {
			case pings <- struct{}{}:
			default:
			}
			return true
		},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer clientWS.CloseNow()

	hello := proto.Hello{Type: proto.TypeHello, DeviceID: "device-idle-ping", DeviceSecret: "s3cr3t", OS: "linux", Arch: "amd64"}
	data, _ := json.Marshal(hello)
	if err := clientWS.Write(dialCtx, websocket.MessageText, data); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	// No further RPC traffic — CloseRead pumps frames in the background
	// so incoming pings are still processed while we otherwise stay idle.
	clientWS.CloseRead(context.Background())

	received := 0
	timeout := time.After(2 * time.Second)
	for received < 3 {
		select {
		case <-pings:
			received++
		case <-timeout:
			t.Fatalf("received only %d keepalive pings in time, want at least 3", received)
		}
	}
}

// TestAgentConn_KeepaliveFailureDisconnectsDevice covers a keepalive
// ping that never gets a pong: a "blackholed" connection, where ws.Read
// alone would hang forever. This must force readLoop to return via its
// own ping timeout, so the device is unregistered instead of lingering
// forever as falsely "connected".
func TestAgentConn_KeepaliveFailureDisconnectsDevice(t *testing.T) {
	reg := NewRegistry()
	verify := func(secret string) bool { return secret == "s3cr3t" }
	srv := httptest.NewServer(NewWSServer(reg, verify))
	defer srv.Close()

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer dialCancel()
	clientWS, _, err := websocket.Dial(dialCtx, "ws"+srv.URL[len("http"):], &websocket.DialOptions{
		OnPingReceived: func(ctx context.Context, payload []byte) bool {
			return false // never send a pong — simulate a blackholed connection
		},
	})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer clientWS.CloseNow()

	hello := proto.Hello{Type: proto.TypeHello, DeviceID: "device-keepalive-fail", DeviceSecret: "s3cr3t", OS: "linux", Arch: "amd64"}
	data, _ := json.Marshal(hello)
	if err := clientWS.Write(dialCtx, websocket.MessageText, data); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	clientWS.CloseRead(context.Background())

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := reg.Get("device-keepalive-fail"); !ok {
			return // unregistered — readLoop returned after the keepalive timeout
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("device is still registered after 2s; a keepalive timeout should have disconnected it")
}

// TestAgentConn_CallLogsWriteFailureMidCall covers the ws.Write failure
// path, distinct from TestAgentConn_CallOnTransportLoss's c.closed path.
//
// No readLoop runs for this AgentConn, so closing ws beforehand makes
// Write fail deterministically instead of racing readLoop's own close.
func TestAgentConn_CallLogsWriteFailureMidCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		<-r.Context().Done()
		ws.CloseNow()
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+srv.URL[len("http"):], nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()
	ws.CloseNow()

	conn := &AgentConn{deviceID: "device-write-fail", ws: ws, pending: make(map[string]chan *proto.Response), closed: make(chan struct{})}

	var logBuf syncBuffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	if _, err := conn.Call(context.Background(), proto.MethodDevicePing, struct{}{}); err == nil {
		t.Fatal("expected Call to fail after the transport was closed")
	}
	if !strings.Contains(logBuf.String(), "transport failure") {
		t.Fatalf("log output = %q, want it to mention \"transport failure\"", logBuf.String())
	}
}

// TestAgentConn_CallDoesNotLogTransportFailureOnCtxCancel covers that a
// Write failing because the caller's own ctx was canceled — an expected
// outcome, not a lost transport — must not be misreported as a
// "transport failure".
func TestAgentConn_CallDoesNotLogTransportFailureOnCtxCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		<-r.Context().Done()
		ws.CloseNow()
	}))
	defer srv.Close()

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer dialCancel()
	ws, _, err := websocket.Dial(dialCtx, "ws"+srv.URL[len("http"):], nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()

	conn := &AgentConn{deviceID: "device-ctx-cancel", ws: ws, pending: make(map[string]chan *proto.Response), closed: make(chan struct{})}

	var logBuf syncBuffer
	log.SetOutput(&logBuf)
	defer log.SetOutput(os.Stderr)

	callCtx, callCancel := context.WithCancel(context.Background())
	callCancel() // already canceled, so the Write itself must fail with it
	if _, err := conn.Call(callCtx, proto.MethodDevicePing, struct{}{}); err == nil {
		t.Fatal("expected Call to fail with an already-canceled ctx")
	}
	if strings.Contains(logBuf.String(), "transport failure") {
		t.Fatalf("log output = %q, want no \"transport failure\" log for a caller-canceled ctx", logBuf.String())
	}
}
