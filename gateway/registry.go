package gateway

import (
	"sync"

	"command-relay-mcp/internal/proto"
)

type DeviceInfo struct {
	DeviceID     string
	OS           string
	Arch         string
	Capabilities proto.Capabilities
}

// Registry tracks online Agents. It is intentionally not persisted
// (base spec §4.1): a Gateway restart loses it, and Agents rebuild it
// by reconnecting.
type Registry struct {
	mu    sync.Mutex
	conns map[string]*AgentConn
}

func NewRegistry() *Registry {
	return &Registry{conns: make(map[string]*AgentConn)}
}

// Register replaces any existing connection for the same device_id,
// closing the old one (base spec §5.2: "新しい接続を採用し、古い接続をclose").
func (r *Registry) Register(c *AgentConn) {
	r.mu.Lock()
	old, existed := r.conns[c.deviceID]
	r.conns[c.deviceID] = c
	r.mu.Unlock()
	if existed {
		old.Close()
	}
}

// Unregister removes c from the registry, but only if c is still the
// connection currently registered for its device_id. This identity check
// matters on reconnect: Register's own old.Close() makes the OLD
// connection's readLoop return and its deferred Unregister(old) fire —
// without the check, that would unconditionally delete the device_id
// even though Register had already replaced it with the new connection,
// evicting a live connection (base spec §5.2: the new connection must be
// the one the registry keeps serving).
func (r *Registry) Unregister(c *AgentConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.conns[c.deviceID] == c {
		delete(r.conns, c.deviceID)
	}
}

func (r *Registry) Get(deviceID string) (*AgentConn, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.conns[deviceID]
	return c, ok
}

// CloseAll closes every registered Agent connection. Call it on Gateway
// shutdown: http.Server.Close/Shutdown does not know about hijacked
// WebSocket connections, so without this an Agent's read would simply
// hang instead of erroring and triggering its reconnect backoff
// (base spec §27 acceptance criterion #7).
func (r *Registry) CloseAll() {
	r.mu.Lock()
	conns := make([]*AgentConn, 0, len(r.conns))
	for _, c := range r.conns {
		conns = append(conns, c)
	}
	r.conns = make(map[string]*AgentConn)
	r.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

func (r *Registry) List() []DeviceInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]DeviceInfo, 0, len(r.conns))
	for _, c := range r.conns {
		out = append(out, DeviceInfo{DeviceID: c.deviceID, OS: c.os, Arch: c.arch, Capabilities: c.capabilities})
	}
	return out
}
