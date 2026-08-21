package agent

import (
	"context"
	"encoding/json"

	"command-relay-mcp/internal/proto"
)

// PingResult is the payload of device.ping.
type PingResult struct {
	Status string `json:"status"`
}

// Ping implements device.ping (base spec §19.1): a liveness probe with
// no side effects.
func Ping(ctx context.Context, raw json.RawMessage) (any, *proto.RPCError) {
	return PingResult{Status: "pong"}, nil
}
