package gateway

import (
	"context"
	"encoding/json"
	"net/http/httptest"
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
