package gateway

import "testing"

func TestRegistry_RegisterGetUnregister(t *testing.T) {
	reg := NewRegistry()
	conn := &AgentConn{deviceID: "pine"}

	reg.Register(conn)
	got, ok := reg.Get("pine")
	if !ok || got != conn {
		t.Fatalf("Get after Register: ok=%v got=%v", ok, got)
	}

	list := reg.List()
	if len(list) != 1 || list[0].DeviceID != "pine" {
		t.Fatalf("List = %+v", list)
	}

	reg.Unregister(conn)
	if _, ok := reg.Get("pine"); ok {
		t.Fatal("device still registered after Unregister")
	}
}

// TestRegistry_ReconnectRaceDoesNotEvictNewConnection covers base spec
// §5.2: "新しい接続を採用し、古い接続をclose" — the new connection must
// remain the one the registry serves, even once the old connection's
// readLoop returns (from Register's own old.Close(), in the real
// wsserver flow) and fires its own deferred Unregister(old). Registered
// directly rather than via Register(), which would call old.Close() and
// panic on these bare test AgentConns' nil ws field — irrelevant to the
// identity check this test targets.
func TestRegistry_ReconnectRaceDoesNotEvictNewConnection(t *testing.T) {
	reg := NewRegistry()
	oldConn := &AgentConn{deviceID: "pine"}
	newConn := &AgentConn{deviceID: "pine"}

	reg.conns[oldConn.deviceID] = oldConn
	reg.conns[newConn.deviceID] = newConn // simulates Register(newConn) replacing oldConn

	reg.Unregister(oldConn) // simulates oldConn's readLoop-triggered deferred Unregister

	got, ok := reg.Get("pine")
	if !ok {
		t.Fatal("device evicted entirely by the old connection's teardown")
	}
	if got != newConn {
		t.Fatalf("Get = %v, want the new connection to remain registered", got)
	}
}
