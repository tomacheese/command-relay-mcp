package agent

import (
	"context"
	"encoding/json"
	"testing"

	"command-relay-mcp/internal/proto"
)

func TestDispatcher_RoutesToRegisteredHandler(t *testing.T) {
	d := NewDispatcher()
	d.Handle("device.ping", func(ctx context.Context, params json.RawMessage) (any, *proto.RPCError) {
		return map[string]string{"status": "pong"}, nil
	})

	req := &proto.Request{Type: proto.TypeRequest, RequestID: "1", Method: "device.ping"}
	resp := d.Dispatch(context.Background(), req)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %+v", resp.Error)
	}
	var out map[string]string
	if err := json.Unmarshal(resp.Result, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out["status"] != "pong" {
		t.Fatalf("got %+v", out)
	}
}

func TestDispatcher_UnknownMethodIsInvalidRequest(t *testing.T) {
	d := NewDispatcher()
	req := &proto.Request{Type: proto.TypeRequest, RequestID: "1", Method: "no.such.method"}
	resp := d.Dispatch(context.Background(), req)
	if resp.Error == nil || resp.Error.Code != proto.ErrInvalidRequest {
		t.Fatalf("resp = %+v", resp)
	}
}
