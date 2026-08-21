package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"command-relay-mcp/internal/proto"
	"github.com/coder/websocket"
)

// TestConnection_MultiplexesConcurrentRequests covers that multiple
// RPCs must be multiplexed on one connection, so a slow handler in
// flight must not block a fast request sent right after it from being
// answered.
func TestConnection_MultiplexesConcurrentRequests(t *testing.T) {
	fastRespondedFirst := make(chan bool, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer c.CloseNow()
		ctx := context.Background()

		if _, _, err := c.Read(ctx); err != nil { // hello
			t.Errorf("read hello: %v", err)
			return
		}

		send := func(id, method string) {
			data, _ := json.Marshal(proto.Request{Type: proto.TypeRequest, RequestID: id, Method: method})
			if err := c.Write(ctx, websocket.MessageText, data); err != nil {
				t.Errorf("write %s: %v", id, err)
			}
		}
		// Dispatch the slow request first, then the fast one right after,
		// without waiting for the slow one's response.
		send("slow-1", "slow")
		send("fast-1", "device.ping")

		var order []string
		for i := 0; i < 2; i++ {
			_, data, err := c.Read(ctx)
			if err != nil {
				t.Errorf("read response %d: %v", i, err)
				return
			}
			var resp proto.Response
			if err := json.Unmarshal(data, &resp); err != nil {
				t.Errorf("unmarshal response: %v", err)
				return
			}
			order = append(order, resp.RequestID)
		}
		fastRespondedFirst <- len(order) == 2 && order[0] == "fast-1"
	}))
	defer srv.Close()

	d := NewDispatcher()
	d.Handle("device.ping", Ping)
	d.Handle("slow", func(ctx context.Context, params json.RawMessage) (any, *proto.RPCError) {
		time.Sleep(300 * time.Millisecond)
		return struct{}{}, nil
	})

	cfg := Config{DeviceID: "pine", DeviceSecret: "s3cr3t", GatewayURL: "ws" + srv.URL[len("http"):]}
	conn := NewConnection(cfg, d, proto.Capabilities{CommandExec: true})

	ctx, cancel := context.WithCancel(context.Background())
	go conn.Run(ctx)
	defer cancel()

	select {
	case fastFirst := <-fastRespondedFirst:
		if !fastFirst {
			t.Fatal("the fast request's response arrived after the slow one — requests were serialized, not multiplexed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("did not observe both responses in time")
	}
}

// TestConnection_HandshakeAndRoundTrip spins up a minimal fake Gateway
// WS endpoint, then verifies the real agent Connection sends a valid
// hello and answers a request/response round trip.
func TestConnection_HandshakeAndRoundTrip(t *testing.T) {
	roundTripDone := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accept: %v", err)
			return
		}
		defer c.CloseNow()
		ctx := context.Background()

		_, data, err := c.Read(ctx)
		if err != nil {
			t.Errorf("read hello: %v", err)
			return
		}
		var hello proto.Hello
		if err := json.Unmarshal(data, &hello); err != nil || hello.Type != proto.TypeHello || hello.DeviceID != "pine" {
			t.Errorf("bad hello: %v %+v", err, hello)
			return
		}

		req := proto.Request{Type: proto.TypeRequest, RequestID: "gw-1", Method: "device.ping"}
		reqData, _ := json.Marshal(req)
		if err := c.Write(ctx, websocket.MessageText, reqData); err != nil {
			t.Errorf("write request: %v", err)
			return
		}

		_, respData, err := c.Read(ctx)
		if err != nil {
			t.Errorf("read response: %v", err)
			return
		}
		var resp proto.Response
		if err := json.Unmarshal(respData, &resp); err != nil || resp.RequestID != "gw-1" {
			t.Errorf("bad response: %v %+v", err, resp)
			return
		}
		close(roundTripDone)
	}))
	defer srv.Close()

	d := NewDispatcher()
	d.Handle("device.ping", Ping)

	cfg := Config{DeviceID: "pine", DeviceSecret: "s3cr3t", GatewayURL: "ws" + srv.URL[len("http"):]}
	conn := NewConnection(cfg, d, proto.Capabilities{CommandExec: true})

	ctx, cancel := context.WithCancel(context.Background())
	go conn.Run(ctx)
	defer cancel()

	select {
	case <-roundTripDone:
	case <-time.After(3 * time.Second):
		t.Fatal("handshake/round trip did not complete")
	}
}
