package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"command-relay-mcp/agent"
	"command-relay-mcp/internal/proto"
	"github.com/coder/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeAgentServer is a real WS server acting as the Agent side, so this
// test exercises the MCP<->Registry<->AgentConn wiring end to end.
func newFakeAgentServer(t *testing.T, reg *Registry) *httptest.Server {
	verify := func(secret string) bool { return true }
	return httptest.NewServer(NewWSServer(reg, verify))
}

func TestMCPServer_DevicesListAndPing(t *testing.T) {
	reg := NewRegistry()
	wsSrv := newFakeAgentServer(t, reg)
	defer wsSrv.Close()

	d := newTestDialedDevice(t, wsSrv.URL, "pine")
	defer d.Close()

	handler := NewMCPHTTPHandlerNoAuth(reg)
	mcpSrv := httptest.NewServer(handler)
	defer mcpSrv.Close()

	transport := &mcp.StreamableClientTransport{Endpoint: mcpSrv.URL}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0.0.0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "devices_list"})
	if err != nil {
		t.Fatalf("CallTool devices_list: %v", err)
	}
	if res.IsError {
		t.Fatalf("devices_list returned tool error: %+v", res.Content)
	}
	var listOut DevicesListResult
	if err := unmarshalStructured(res.StructuredContent, &listOut); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(listOut.Devices) != 1 || listOut.Devices[0].DeviceID != "pine" {
		t.Fatalf("devices = %+v", listOut.Devices)
	}

	res2, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "device_ping", Arguments: map[string]any{"device_id": "pine"}})
	if err != nil {
		t.Fatalf("CallTool device_ping: %v", err)
	}
	if res2.IsError {
		t.Fatalf("device_ping returned tool error: %+v", res2.Content)
	}
}

func TestMCPServer_CommandExecCarriesSessionAndSubject(t *testing.T) {
	reg := NewRegistry()
	wsSrv := newFakeAgentServer(t, reg)
	defer wsSrv.Close()

	d := newTestDialedDevice(t, wsSrv.URL, "pine")
	defer d.Close()

	handler := NewMCPHTTPHandlerNoAuth(reg)
	mcpSrv := httptest.NewServer(handler)
	defer mcpSrv.Close()

	transport := &mcp.StreamableClientTransport{Endpoint: mcpSrv.URL}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0.0.0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "command_exec",
		Arguments: map[string]any{"device_id": "pine", "command": "true"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("command_exec returned tool error: %+v", res.Content)
	}

	if d.lastClientContextID == "" {
		t.Fatal("Agent stub never received a non-empty client_context_id")
	}
}

func TestMCPServer_FileReadRoutesToDevice(t *testing.T) {
	reg := NewRegistry()
	wsSrv := newFakeAgentServer(t, reg)
	defer wsSrv.Close()

	d := newTestDialedDevice(t, wsSrv.URL, "pine")
	defer d.Close()

	handler := NewMCPHTTPHandlerNoAuth(reg)
	mcpSrv := httptest.NewServer(handler)
	defer mcpSrv.Close()

	transport := &mcp.StreamableClientTransport{Endpoint: mcpSrv.URL}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0.0.0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "file_read",
		Arguments: map[string]any{"device_id": "pine", "path": "/tmp/x"},
	})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("file_read returned tool error: %+v", res.Content)
	}
	var out agent.FileReadResult
	if err := unmarshalStructured(res.StructuredContent, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ContentBase64 != "aGVsbG8=" {
		t.Fatalf("ContentBase64 = %q", out.ContentBase64)
	}
}

func TestMCPServer_AcceptsRequestWithNoCredentials(t *testing.T) {
	reg := NewRegistry()
	handler := NewMCPHTTPHandlerNoAuth(reg)
	mcpSrv := httptest.NewServer(handler)
	defer mcpSrv.Close()

	resp, err := http.Post(mcpSrv.URL, "application/json", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		t.Fatalf("status = %d, want no auth-based rejection", resp.StatusCode)
	}
}

// TestMCPServer_InitializeRespondsWithJSONNotSSE verifies a single-shot
// request gets back application/json rather than text/event-stream (see
// NewMCPHTTPHandlerNoAuth for why).
func TestMCPServer_InitializeRespondsWithJSONNotSSE(t *testing.T) {
	reg := NewRegistry()
	handler := NewMCPHTTPHandlerNoAuth(reg)
	mcpSrv := httptest.NewServer(handler)
	defer mcpSrv.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	req, err := http.NewRequest(http.MethodPost, mcpSrv.URL, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
}

// TestNewMCPMux_UnrelatedPathGets404 verifies an unrelated well-known
// path (e.g. OAuth discovery) gets a plain 404, not the MCP handler's
// own 400.
func TestNewMCPMux_UnrelatedPathGets404(t *testing.T) {
	reg := NewRegistry()
	mcpSrv := httptest.NewServer(NewMCPMux(reg))
	defer mcpSrv.Close()

	resp, err := http.Get(mcpSrv.URL + "/.well-known/oauth-protected-resource/mcp")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

type testDialedDevice struct {
	ws                  *websocket.Conn
	lastClientContextID string
}

func newTestDialedDevice(t *testing.T, wsURL, deviceID string) *testDialedDevice {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+wsURL[len("http"):], nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	hello := proto.Hello{Type: proto.TypeHello, DeviceID: deviceID, OS: "linux", Arch: "amd64"}
	data, _ := json.Marshal(hello)
	if err := ws.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	d := &testDialedDevice{ws: ws}
	go d.serve()
	return d
}

func (d *testDialedDevice) serve() {
	ctx := context.Background()
	for {
		_, data, err := d.ws.Read(ctx)
		if err != nil {
			return
		}
		var req proto.Request
		json.Unmarshal(data, &req)
		resp := proto.Response{Type: proto.TypeResponse, RequestID: req.RequestID}
		switch req.Method {
		case proto.MethodDevicePing:
			resp.Result = json.RawMessage(`{"status":"pong"}`)
		case proto.MethodCommandExec:
			var p struct {
				ClientContextID string `json:"client_context_id"`
			}
			json.Unmarshal(req.Params, &p)
			d.lastClientContextID = p.ClientContextID
			resp.Result = json.RawMessage(`{"process_id":"p1","os_pid":1,"stdout":"","stderr":"","exit_code":0,"timed_out":false}`)
		case proto.MethodFileWrite:
			resp.Result = json.RawMessage(`{}`)
		case proto.MethodFileRead:
			resp.Result = json.RawMessage(`{"content_base64":"aGVsbG8=","size":5}`)
		default:
			resp.Error = &proto.RPCError{Code: proto.ErrUnsupported, Message: "test stub"}
		}
		respData, _ := json.Marshal(resp)
		d.ws.Write(ctx, websocket.MessageText, respData)
	}
}

func (d *testDialedDevice) Close() { d.ws.CloseNow() }

// unmarshalStructured re-marshals a CallToolResult's StructuredContent
// (typed as `any` by the SDK) into a concrete Go struct.
func unmarshalStructured(structured any, out any) error {
	raw, err := json.Marshal(structured)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}
