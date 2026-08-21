package agent

import (
	"context"
	"encoding/json"

	"command-relay-mcp/internal/proto"
)

// HandlerFunc implements one RPC method on the Agent side.
type HandlerFunc func(ctx context.Context, params json.RawMessage) (any, *proto.RPCError)

type Dispatcher struct {
	handlers map[string]HandlerFunc
}

func NewDispatcher() *Dispatcher {
	return &Dispatcher{handlers: make(map[string]HandlerFunc)}
}

func (d *Dispatcher) Handle(method string, h HandlerFunc) {
	d.handlers[method] = h
}

func (d *Dispatcher) Dispatch(ctx context.Context, req *proto.Request) *proto.Response {
	resp := &proto.Response{Type: proto.TypeResponse, RequestID: req.RequestID}
	h, ok := d.handlers[req.Method]
	if !ok {
		resp.Error = &proto.RPCError{Code: proto.ErrInvalidRequest, Message: "unknown method: " + req.Method}
		return resp
	}
	result, rpcErr := h(ctx, req.Params)
	if rpcErr != nil {
		resp.Error = rpcErr
		return resp
	}
	raw, err := json.Marshal(result)
	if err != nil {
		resp.Error = &proto.RPCError{Code: proto.ErrInternal, Message: err.Error()}
		return resp
	}
	resp.Result = raw
	return resp
}
