package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"

	"command-relay-mcp/internal/proto"
)

// ExecutionListParams is the payload of execution.list (base spec §19.5).
type ExecutionListParams struct {
	Limit int `json:"limit,omitempty"`
}

type ExecutionListResult struct {
	Executions []Execution `json:"executions"`
}

// ExecutionGetParams is the payload of execution.get (base spec §19.5).
type ExecutionGetParams struct {
	ExecutionID string `json:"execution_id"`
}

type ExecutionGetResult struct {
	Execution Execution `json:"execution"`
}

const defaultExecutionListLimit = 100

type ExecutionHandlers struct {
	hist *HistoryStore
}

func NewExecutionHandlers(hist *HistoryStore) *ExecutionHandlers {
	return &ExecutionHandlers{hist: hist}
}

// List implements execution.list: history only, never live process
// state (base spec §12.1).
func (h *ExecutionHandlers) List(ctx context.Context, raw json.RawMessage) (any, *proto.RPCError) {
	var p ExecutionListParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &proto.RPCError{Code: proto.ErrInvalidRequest, Message: err.Error()}
	}
	limit := p.Limit
	if limit <= 0 {
		limit = defaultExecutionListLimit
	}
	list, err := h.hist.List(limit)
	if err != nil {
		log.Printf("agent: history List failed: %v", err)
		return nil, &proto.RPCError{Code: proto.ErrInternal, Message: err.Error()}
	}
	return ExecutionListResult{Executions: list}, nil
}

// Get implements execution.get.
func (h *ExecutionHandlers) Get(ctx context.Context, raw json.RawMessage) (any, *proto.RPCError) {
	var p ExecutionGetParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &proto.RPCError{Code: proto.ErrInvalidRequest, Message: err.Error()}
	}
	exec, err := h.hist.Get(p.ExecutionID)
	if err == sql.ErrNoRows {
		return nil, &proto.RPCError{Code: proto.ErrInvalidRequest, Message: "execution_id not found: " + p.ExecutionID}
	}
	if err != nil {
		log.Printf("agent: history Get failed for execution %s: %v", p.ExecutionID, err)
		return nil, &proto.RPCError{Code: proto.ErrInternal, Message: err.Error()}
	}
	return ExecutionGetResult{Execution: *exec}, nil
}
