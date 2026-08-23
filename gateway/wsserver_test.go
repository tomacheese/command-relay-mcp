package gateway

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"command-relay-mcp/internal/proto"
	"github.com/coder/websocket"
)

func TestWSServer_AcceptsValidCredentialAndRegisters(t *testing.T) {
	reg := NewRegistry()
	verify := func(secret string) bool { return secret == "s3cr3t" }
	srv := httptest.NewServer(NewWSServer(reg, verify))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+srv.URL[len("http"):], nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()

	hello := proto.Hello{Type: proto.TypeHello, DeviceID: "pine", DeviceSecret: "s3cr3t", OS: "linux", Arch: "amd64"}
	data, _ := json.Marshal(hello)
	if err := ws.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write hello: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := reg.Get("pine"); ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("device never appeared in registry")
}

func TestWSServer_RejectsInvalidCredential(t *testing.T) {
	reg := NewRegistry()
	verify := func(secret string) bool { return false }
	srv := httptest.NewServer(NewWSServer(reg, verify))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+srv.URL[len("http"):], nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()

	hello := proto.Hello{Type: proto.TypeHello, DeviceID: "pine", DeviceSecret: "wrong"}
	data, _ := json.Marshal(hello)
	ws.Write(ctx, websocket.MessageText, data)

	// The server should close the connection rather than register the device.
	_, _, err = ws.Read(ctx)
	if err == nil {
		t.Fatal("expected connection to be closed after invalid credential")
	}
	if _, ok := reg.Get("pine"); ok {
		t.Fatal("device should not be registered with an invalid credential")
	}
}

func TestRegistry_CloseAllDropsRealConnectionsAndClearsTheRegistry(t *testing.T) {
	reg := NewRegistry()
	verify := func(secret string) bool { return secret == "s3cr3t" }
	srv := httptest.NewServer(NewWSServer(reg, verify))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+srv.URL[len("http"):], nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()

	hello := proto.Hello{Type: proto.TypeHello, DeviceID: "pine", DeviceSecret: "s3cr3t", OS: "linux", Arch: "amd64"}
	data, _ := json.Marshal(hello)
	if err := ws.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write hello: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := reg.Get("pine"); ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	reg.CloseAll()

	if _, ok := reg.Get("pine"); ok {
		t.Fatal("registry should be empty after CloseAll")
	}
	// The Agent's real connection must have been closed server-side too,
	// not just removed from the registry — otherwise it would hang
	// instead of erroring and reconnecting.
	readCtx, readCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readCancel()
	if _, _, err := ws.Read(readCtx); err == nil {
		t.Fatal("expected the underlying connection to be closed by CloseAll")
	}
}

// TestWSServer_AcceptsLargeMessageFromAgent verifies that a WebSocket
// message larger than coder/websocket's 32KiB default read limit sent
// by an already-authenticated Agent does not close the connection.
func TestWSServer_AcceptsLargeMessageFromAgent(t *testing.T) {
	const bigResultLen = 64 * 1024 // well above the 32,768-byte default
	reg := NewRegistry()
	verify := func(secret string) bool { return secret == "s3cr3t" }
	srv := httptest.NewServer(NewWSServer(reg, verify))
	defer srv.Close()

	ws := dialRegisteredDevice(t, srv, reg, "pine")
	defer ws.CloseNow()

	// readLoop is already running in the background as part of the HTTP
	// handler NewWSServer registered for this connection (see
	// NewWSServer's own conn.readLoop call) — starting a second one here
	// would race two concurrent readers against the same *websocket.Conn.
	conn, _ := reg.Get("pine")

	callDone := make(chan error, 1)
	go func() {
		_, err := conn.Call(context.Background(), "big", struct{}{})
		callDone <- err
	}()

	ctx := context.Background()
	_, reqData, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("read request: %v", err)
	}
	var req proto.Request
	if err := json.Unmarshal(reqData, &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}

	resp := proto.Response{
		Type: proto.TypeResponse, RequestID: req.RequestID,
		Result: json.RawMessage(`"` + strings.Repeat("x", bigResultLen) + `"`),
	}
	respData, _ := json.Marshal(resp)
	if err := ws.Write(ctx, websocket.MessageText, respData); err != nil {
		t.Fatalf("write large response: %v", err)
	}

	select {
	case err := <-callDone:
		if err != nil {
			t.Fatalf("Call returned an error — did the default 32KiB read limit close the connection? %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Call did not complete — did the default 32KiB read limit close the connection?")
	}
}

// TestWSServer_RejectsOversizedPreAuthMessage verifies that the raised
// SetReadLimit does not apply until after the device secret is
// verified, so an unauthenticated client cannot force a large
// allocation via an oversized hello message.
func TestWSServer_RejectsOversizedPreAuthMessage(t *testing.T) {
	const oversizedLen = 64 * 1024 // above the 32,768-byte default, below proto.MaxRPCMessageBytes
	reg := NewRegistry()
	verify := func(secret string) bool { return secret == "s3cr3t" }
	srv := httptest.NewServer(NewWSServer(reg, verify))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+srv.URL[len("http"):], nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.CloseNow()

	// An oversized hello can never unmarshal to a valid credential
	// anyway, but what this test asserts is that the connection is
	// closed for exceeding the pre-auth read limit rather than being
	// buffered in full and only then rejected as bad JSON.
	oversized := []byte(`{"type":"hello","device_secret":"` + strings.Repeat("x", oversizedLen) + `"}`)
	ws.Write(ctx, websocket.MessageText, oversized)

	if _, _, err := ws.Read(ctx); err == nil {
		t.Fatal("expected the connection to be closed for an oversized pre-auth message")
	}
	if _, ok := reg.Get("pine"); ok {
		t.Fatal("device should not be registered from an oversized, unverified hello")
	}
}
