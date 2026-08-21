package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"command-relay-mcp/internal/proto"
	"github.com/coder/websocket"
)

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
	verify := func(deviceID, secret string) bool { return secret == "s3cr3t" }
	srv := httptest.NewServer(NewWSServer(reg, verify))
	defer srv.Close()

	clientWS := dialRegisteredDevice(t, srv, reg, "device-timeout")
	defer clientWS.CloseNow()
	conn, _ := reg.Get("device-timeout")

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
	verify := func(deviceID, secret string) bool { return secret == "s3cr3t" }
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
			verify := func(deviceID, secret string) bool { return secret == "s3cr3t" }
			srv := httptest.NewServer(NewWSServer(reg, verify))
			defer srv.Close()

			clientWS := dialRegisteredDevice(t, srv, reg, "device-"+tc.method)
			defer clientWS.CloseNow()
			conn, _ := reg.Get("device-" + tc.method)

			var logBuf bytes.Buffer
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
