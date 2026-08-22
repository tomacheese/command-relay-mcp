package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"command-relay-mcp/agent"
	"command-relay-mcp/gateway"
	"command-relay-mcp/internal/backend"
	"command-relay-mcp/internal/proto"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type harness struct {
	mcpClient *mcp.ClientSession
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	const deviceID = "e2e-device"
	const deviceSecret = "e2e-secret"

	reg := gateway.NewRegistry()
	verify := func(id, secret string) bool { return id == deviceID && secret == deviceSecret }
	wsSrv := httptest.NewServer(gateway.NewWSServer(reg, verify))
	t.Cleanup(wsSrv.Close)

	mcpSrv := httptest.NewServer(gateway.NewMCPHTTPHandlerNoAuth(reg))
	t.Cleanup(mcpSrv.Close)

	startTestAgent(t, deviceID, deviceSecret, "ws"+wsSrv.URL[len("http"):])
	waitForRegistration(t, reg, deviceID)

	return &harness{mcpClient: mcpSession(t, mcpSrv.URL)}
}

var (
	builtAgentBinaryOnce sync.Once
	builtAgentBinaryPath string
	builtAgentBinaryErr  error
)

// realAgentBinary builds the real agent/cmd binary once per test run and
// caches its path, so every e2e test that needs a self-re-exec sandbox
// target (or the compiled binary itself) shares one build.
func realAgentBinary(t *testing.T) string {
	t.Helper()
	builtAgentBinaryOnce.Do(func() {
		dir, err := os.MkdirTemp("", "e2e-agent-bin-")
		if err != nil {
			builtAgentBinaryErr = err
			return
		}
		binPath := filepath.Join(dir, "command-relay-agent")
		build := exec.Command("go", "build", "-o", binPath, "command-relay-mcp/agent/cmd")
		if out, err := build.CombinedOutput(); err != nil {
			builtAgentBinaryErr = err
			t.Logf("go build agent/cmd output:\n%s", out)
			return
		}
		builtAgentBinaryPath = binPath
	})
	if builtAgentBinaryErr != nil {
		t.Fatalf("go build agent/cmd: %v", builtAgentBinaryErr)
	}
	return builtAgentBinaryPath
}

// startTestAgent boots a real Agent (real Linux process backend, real
// SQLite history store, real dispatcher/handlers) connecting to
// gatewayURL, and keeps it running until the test ends. The sandboxed
// backend behind command.read re-execs the real, separately built
// agent/cmd binary (realAgentBinary), the same way the production Agent
// re-execs itself (addendum §4) — this in-process harness cannot use its
// own test binary as the re-exec target, since that binary doesn't
// implement the "--landlock-exec" subcommand.
func startTestAgent(t *testing.T, deviceID, deviceSecret, gatewayURL string) {
	t.Helper()
	hist, err := agent.OpenHistoryStore(filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatalf("OpenHistoryStore: %v", err)
	}
	t.Cleanup(func() { hist.Close() })

	mgr := agent.NewManager(backend.NewLinuxBackend([]string{"/bin/bash", "-lc"}), 4<<20, 4<<20, time.Hour)

	sandboxBackend := backend.NewSandboxedBackend(realAgentBinary(t), []string{"/bin/bash", "-lc"})
	sandboxSupported := agent.ProbeSandbox(sandboxBackend)
	var sandboxMgr *agent.Manager
	if sandboxSupported {
		sandboxMgr = agent.NewManager(sandboxBackend, 4<<20, 4<<20, time.Hour)
	}

	cmdHandlers := agent.NewCommandHandlers(mgr, sandboxMgr, hist, deviceID, 30_000)
	procHandlers := agent.NewProcessHandlers(mgr, sandboxMgr, hist, deviceID)
	execHandlers := agent.NewExecutionHandlers(hist)
	fileHandlers := agent.NewFileHandlers()

	d := agent.NewDispatcher()
	d.Handle(proto.MethodExecutionList, execHandlers.List)
	d.Handle(proto.MethodExecutionGet, execHandlers.Get)
	d.Handle(proto.MethodCommandExec, cmdHandlers.Exec)
	d.Handle(proto.MethodCommandRead, cmdHandlers.Read)
	d.Handle(proto.MethodProcessStart, procHandlers.Start)
	d.Handle(proto.MethodProcessRead, procHandlers.Read)
	d.Handle(proto.MethodProcessWrite, procHandlers.Write)
	d.Handle(proto.MethodProcessWait, procHandlers.Wait)
	d.Handle(proto.MethodProcessTerminate, procHandlers.Terminate)
	d.Handle(proto.MethodProcessList, procHandlers.List)
	d.Handle(proto.MethodDevicePing, agent.Ping)
	d.Handle(proto.MethodFileRead, fileHandlers.Read)
	d.Handle(proto.MethodFileStat, fileHandlers.Stat)
	d.Handle(proto.MethodFileList, fileHandlers.List)
	d.Handle(proto.MethodFileWrite, fileHandlers.Write)
	d.Handle(proto.MethodFileMove, fileHandlers.Move)
	d.Handle(proto.MethodFileDelete, fileHandlers.Delete)
	d.Handle(proto.MethodDirectoryCreate, fileHandlers.CreateDirectory)

	agentCfg := agent.Config{DeviceID: deviceID, DeviceSecret: deviceSecret, GatewayURL: gatewayURL}
	caps := proto.Capabilities{CommandExec: true, CommandRead: sandboxSupported, Process: true, Filesystem: true}
	conn := agent.NewConnection(agentCfg, d, caps)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go conn.Run(ctx)
}

func waitForRegistration(t *testing.T, reg *gateway.Registry, deviceID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := reg.Get(deviceID); ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("device %q never registered", deviceID)
}

// mcpSession opens a real MCP client session against a Gateway MCP
// endpoint.
func mcpSession(t *testing.T, endpoint string) *mcp.ClientSession {
	t.Helper()
	transport := &mcp.StreamableClientTransport{Endpoint: endpoint}
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e-test-client", Version: "v0.0.0"}, nil)
	session, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("mcp Connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func (h *harness) callTool(t *testing.T, name string, args map[string]any, out any) {
	t.Helper()
	callToolOn(t, h.mcpClient, name, args, out)
}

func callToolOn(t *testing.T, session *mcp.ClientSession, name string, args map[string]any, out any) {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	if res.IsError {
		var msg string
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				msg += tc.Text
			}
		}
		t.Fatalf("CallTool %s returned tool error: %s", name, msg)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("CallTool %s: marshal structured content: %v", name, err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("CallTool %s: unmarshal: %v", name, err)
	}
}

func TestE2E_DevicesListAndPing(t *testing.T) {
	h := newHarness(t)

	var list gateway.DevicesListResult
	h.callTool(t, "devices_list", nil, &list)
	if len(list.Devices) != 1 || list.Devices[0].DeviceID != "e2e-device" {
		t.Fatalf("devices = %+v", list.Devices)
	}

	var ping agent.PingResult
	h.callTool(t, "device_ping", map[string]any{"device_id": "e2e-device"}, &ping)
	if ping.Status != "pong" {
		t.Fatalf("ping = %+v", ping)
	}
}

func TestE2E_CommandExecRunsRealShellCommand(t *testing.T) {
	h := newHarness(t)

	var res agent.CommandExecResult
	h.callTool(t, "command_exec", map[string]any{"device_id": "e2e-device", "command": "echo mcp-e2e-ok"}, &res)
	if res.TimedOut || res.ExitCode == nil || *res.ExitCode != 0 {
		t.Fatalf("res = %+v", res)
	}
	if res.Stdout != "mcp-e2e-ok\n" {
		t.Fatalf("stdout = %q", res.Stdout)
	}
}

// TestE2E_ExecutionHistoryPersistsAndIsQueryableOverMCP covers base
// spec §27 acceptance criteria #9-10 and addendum §7 scenario 5: after
// a real command_exec, the execution is retrievable via execution_list
// / execution_get, and the persisted row never contains stdout/stderr
// — those fields simply don't exist on agent.Execution.
func TestE2E_ExecutionHistoryPersistsAndIsQueryableOverMCP(t *testing.T) {
	h := newHarness(t)

	var execRes agent.CommandExecResult
	h.callTool(t, "command_exec", map[string]any{"device_id": "e2e-device", "command": "echo history-check"}, &execRes)
	if execRes.ExitCode == nil || *execRes.ExitCode != 0 {
		t.Fatalf("execRes = %+v", execRes)
	}

	var list agent.ExecutionListResult
	h.callTool(t, "execution_list", map[string]any{"device_id": "e2e-device", "limit": 10}, &list)
	if len(list.Executions) != 1 {
		t.Fatalf("executions = %+v", list.Executions)
	}
	got := list.Executions[0]
	if got.Command != "echo history-check" || got.ProcessID != execRes.ProcessID {
		t.Fatalf("got = %+v", got)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("exit code = %v", got.ExitCode)
	}

	var get agent.ExecutionGetResult
	h.callTool(t, "execution_get", map[string]any{"device_id": "e2e-device", "execution_id": got.ExecutionID}, &get)
	if get.Execution.ExecutionID != got.ExecutionID {
		t.Fatalf("get = %+v", get.Execution)
	}
}

// TestE2E_CommandExecTimeoutContinuesAndIsTrackable covers that a
// command_exec timeout must not kill the underlying process, and the
// process must remain trackable by process_id afterwards.
func TestE2E_CommandExecTimeoutContinuesAndIsTrackable(t *testing.T) {
	h := newHarness(t)

	var res agent.CommandExecResult
	h.callTool(t, "command_exec", map[string]any{
		"device_id": "e2e-device", "command": "sleep 0.3; echo done-after-timeout",
		"timeout_ms": 50,
	}, &res)
	if !res.TimedOut || res.ExitCode != nil {
		t.Fatalf("expected a timeout with no exit code yet, got res = %+v", res)
	}
	if res.ProcessID == "" || res.OSPID == 0 {
		t.Fatalf("res = %+v", res)
	}

	// The real OS process is still alive past the command_exec timeout.
	if _, err := os.Stat("/proc/" + strconv.Itoa(res.OSPID)); err != nil {
		t.Fatalf("expected pid %d to still be running after the command_exec timeout: %v", res.OSPID, err)
	}

	var waitRes agent.ProcessWaitResult
	h.callTool(t, "process_wait", map[string]any{"device_id": "e2e-device", "process_id": res.ProcessID, "timeout_ms": 3000}, &waitRes)
	if waitRes.TimedOut || waitRes.ExitCode == nil || *waitRes.ExitCode != 0 {
		t.Fatalf("waitRes = %+v", waitRes)
	}

	var readRes agent.ProcessReadResult
	h.callTool(t, "process_read", map[string]any{
		"device_id": "e2e-device", "process_id": res.ProcessID,
		"stdout_offset": int64(0), "stderr_offset": int64(0), "max_bytes": 4096,
	}, &readRes)
	if readRes.Stdout != "done-after-timeout\n" {
		t.Fatalf("stdout = %q", readRes.Stdout)
	}
}

func TestE2E_ProcessLifecycleOverRealOSProcess(t *testing.T) {
	h := newHarness(t)

	var started agent.ProcessStartResult
	h.callTool(t, "process_start", map[string]any{"device_id": "e2e-device", "command": "for i in 1 2 3; do echo line-$i; sleep 0.05; done"}, &started)
	if started.ProcessID == "" || started.OSPID == 0 {
		t.Fatalf("started = %+v", started)
	}

	// Confirm the OS process genuinely exists right now (real process,
	// not a mock): /proc/<pid> must be present on this Linux host.
	if _, err := os.Stat("/proc/" + strconv.Itoa(started.OSPID)); err != nil {
		t.Fatalf("expected a real OS process at pid %d: %v", started.OSPID, err)
	}

	var stdout string
	deadline := time.Now().Add(3 * time.Second)
	var offset int64
	for time.Now().Before(deadline) {
		var read agent.ProcessReadResult
		h.callTool(t, "process_read", map[string]any{
			"device_id": "e2e-device", "process_id": started.ProcessID,
			"stdout_offset": offset, "stderr_offset": int64(0), "max_bytes": 4096,
		}, &read)
		stdout += read.Stdout
		offset = read.NextStdoutOffset
		if read.ExitCode != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if stdout != "line-1\nline-2\nline-3\n" {
		t.Fatalf("accumulated stdout = %q", stdout)
	}

	var waitRes agent.ProcessWaitResult
	h.callTool(t, "process_wait", map[string]any{"device_id": "e2e-device", "process_id": started.ProcessID, "timeout_ms": 2000}, &waitRes)
	if waitRes.TimedOut || waitRes.ExitCode == nil || *waitRes.ExitCode != 0 {
		t.Fatalf("waitRes = %+v", waitRes)
	}

	// Real process really is gone now.
	if _, err := os.Stat("/proc/" + strconv.Itoa(started.OSPID)); err == nil {
		t.Fatalf("expected pid %d to be gone after exit", started.OSPID)
	}
}

func TestE2E_ProcessTerminateKillsRealProcessTree(t *testing.T) {
	h := newHarness(t)

	var started agent.ProcessStartResult
	h.callTool(t, "process_start", map[string]any{"device_id": "e2e-device", "command": "sleep 30"}, &started)

	var termRes agent.ProcessTerminateResult
	h.callTool(t, "process_terminate", map[string]any{"device_id": "e2e-device", "process_id": started.ProcessID, "grace_ms": 500}, &termRes)
	if !termRes.Terminated {
		t.Fatalf("termRes = %+v", termRes)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat("/proc/" + strconv.Itoa(started.OSPID)); err != nil {
			return // gone, as expected
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("pid %d still alive after process_terminate", started.OSPID)
}

// restartableGateway runs the Gateway's real HTTP handlers (/agent/ws,
// /mcp — the same mux gateway/cmd/main.go builds) on a fixed address so
// it can be torn down and rebuilt on the same port, simulating a
// Gateway process restart.
type restartableGateway struct {
	addr string
	srv  *http.Server
	reg  *gateway.Registry
}

func startRestartableGateway(t *testing.T, addr, deviceID, deviceSecret string) *restartableGateway {
	t.Helper()
	g := &restartableGateway{addr: addr}
	g.listenAndServe(t, deviceID, deviceSecret)
	t.Cleanup(func() { g.srv.Close() })
	return g
}

func (g *restartableGateway) listenAndServe(t *testing.T, deviceID, deviceSecret string) {
	t.Helper()
	ln, err := net.Listen("tcp", g.addr)
	if err != nil {
		t.Fatalf("listen on %s: %v", g.addr, err)
	}
	g.addr = ln.Addr().String()

	reg := gateway.NewRegistry()
	verify := func(id, secret string) bool { return id == deviceID && secret == deviceSecret }
	mux := http.NewServeMux()
	mux.Handle("/agent/ws", gateway.NewWSServer(reg, verify))
	mux.Handle("/mcp", gateway.NewMCPHTTPHandlerNoAuth(reg))

	g.reg = reg
	g.srv = &http.Server{Handler: mux}
	go func() {
		if err := g.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			t.Logf("gateway Serve: %v", err)
		}
	}()
}

// restart abruptly closes every connection the current Gateway process
// holds (including the Agent's live WebSocket) and rebuilds the HTTP
// handlers with a brand-new in-memory Registry on the same address,
// simulating a Gateway process restart that wipes its in-memory state.
func (g *restartableGateway) restart(t *testing.T, deviceID, deviceSecret string) {
	t.Helper()
	// http.Server.Close does not know about hijacked WebSocket
	// connections (see net/http docs), so CloseAll drops the Agent's
	// live connection explicitly — the same call gateway/cmd/main.go
	// makes on a graceful shutdown.
	g.reg.CloseAll()
	_ = g.srv.Close()
	g.listenAndServe(t, deviceID, deviceSecret)
}

// TestE2E_ProcessSurvivesGatewayRestart covers that a process the
// Agent started keeps running through a Gateway restart, and is
// trackable again over MCP once the Agent has reconnected to the
// restarted Gateway.
func TestE2E_ProcessSurvivesGatewayRestart(t *testing.T) {
	const deviceID = "restart-device"
	const deviceSecret = "restart-secret"

	gw := startRestartableGateway(t, "127.0.0.1:0", deviceID, deviceSecret)
	startTestAgent(t, deviceID, deviceSecret, "ws://"+gw.addr+"/agent/ws")
	waitForRegistration(t, gw.reg, deviceID)

	session1 := mcpSession(t, "http://"+gw.addr+"/mcp")
	var started agent.ProcessStartResult
	callToolOn(t, session1, "process_start", map[string]any{"device_id": deviceID, "command": "sleep 2; echo survived-restart"}, &started)
	if started.ProcessID == "" || started.OSPID == 0 {
		t.Fatalf("started = %+v", started)
	}

	gw.restart(t, deviceID, deviceSecret)
	waitForRegistration(t, gw.reg, deviceID)

	// The real OS process kept running the whole time — the Agent
	// process itself was never restarted, only the Gateway.
	if _, err := os.Stat("/proc/" + strconv.Itoa(started.OSPID)); err != nil {
		t.Fatalf("expected pid %d to still be running across the Gateway restart: %v", started.OSPID, err)
	}

	session2 := mcpSession(t, "http://"+gw.addr+"/mcp")
	var waitRes agent.ProcessWaitResult
	callToolOn(t, session2, "process_wait", map[string]any{"device_id": deviceID, "process_id": started.ProcessID, "timeout_ms": 5000}, &waitRes)
	if waitRes.TimedOut || waitRes.ExitCode == nil || *waitRes.ExitCode != 0 {
		t.Fatalf("waitRes = %+v", waitRes)
	}

	var readRes agent.ProcessReadResult
	callToolOn(t, session2, "process_read", map[string]any{
		"device_id": deviceID, "process_id": started.ProcessID,
		"stdout_offset": int64(0), "stderr_offset": int64(0), "max_bytes": 4096,
	}, &readRes)
	if readRes.Stdout != "survived-restart\n" {
		t.Fatalf("stdout = %q", readRes.Stdout)
	}
}

// TestE2E_AgentGracefulShutdownTerminatesProcessTree exercises the
// real compiled Agent binary (agent/cmd/main.go) as its own OS process
// — not just calling Manager.TerminateAll in-process — so the actual
// SIGTERM handling and shutdown wiring is what gets verified: stopping
// the Agent service terminates every process tree it started.
func TestE2E_AgentGracefulShutdownTerminatesProcessTree(t *testing.T) {
	const deviceID = "shutdown-device"
	const deviceSecret = "shutdown-secret"

	gw := startRestartableGateway(t, "127.0.0.1:0", deviceID, deviceSecret)

	binPath := filepath.Join(t.TempDir(), "command-relay-agent")
	build := exec.Command("go", "build", "-o", binPath, "command-relay-mcp/agent/cmd")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build agent/cmd: %v\n%s", err, out)
	}

	cmd := exec.Command(binPath)
	cmd.Env = append(os.Environ(),
		"DEVICE_ID="+deviceID,
		"DEVICE_SECRET="+deviceSecret,
		"GATEWAY_URL=ws://"+gw.addr+"/agent/ws",
		"HISTORY_DB_PATH="+filepath.Join(t.TempDir(), "history.db"),
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start agent binary: %v", err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	// Kill() is a harmless no-op if the process already exited via the
	// SIGTERM path below; it must not also drain waitDone, since the
	// main flow already does that exactly once.
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	waitForRegistration(t, gw.reg, deviceID)

	session := mcpSession(t, "http://"+gw.addr+"/mcp")
	var started agent.ProcessStartResult
	callToolOn(t, session, "process_start", map[string]any{"device_id": deviceID, "command": "sleep 30"}, &started)
	if started.OSPID == 0 {
		t.Fatalf("started = %+v", started)
	}
	if _, err := os.Stat("/proc/" + strconv.Itoa(started.OSPID)); err != nil {
		t.Fatalf("expected the real child process %d to be running: %v", started.OSPID, err)
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal agent process: %v", err)
	}

	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("agent process exited with error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("agent process did not exit after SIGTERM")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat("/proc/" + strconv.Itoa(started.OSPID)); err != nil {
			return // gone, as required by the kill boundary
		}
		if time.Now().After(deadline) {
			t.Fatalf("child process %d still alive after Agent shutdown", started.OSPID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestE2E_FilesystemToolsRoundTrip covers the Filesystem API's
// file_write, file_read, file_stat, file_move, and file_delete tools
// against a real file on disk, driven end to end over MCP.
func TestE2E_FilesystemToolsRoundTrip(t *testing.T) {
	h := newHarness(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")

	h.callTool(t, "file_write", map[string]any{
		"device_id": "e2e-device", "path": path,
		"content_base64": base64.StdEncoding.EncodeToString([]byte("hello")),
	}, &struct{}{})

	var readOut struct {
		ContentBase64 string `json:"content_base64"`
		Size          int64  `json:"size"`
	}
	h.callTool(t, "file_read", map[string]any{"device_id": "e2e-device", "path": path}, &readOut)
	got, err := base64.StdEncoding.DecodeString(readOut.ContentBase64)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("content = %q, want hello", got)
	}

	var statOut struct {
		Size  int64 `json:"size"`
		IsDir bool  `json:"is_dir"`
	}
	h.callTool(t, "file_stat", map[string]any{"device_id": "e2e-device", "path": path}, &statOut)
	if statOut.Size != 5 || statOut.IsDir {
		t.Fatalf("statOut = %+v", statOut)
	}

	movedPath := filepath.Join(dir, "b.txt")
	h.callTool(t, "file_move", map[string]any{"device_id": "e2e-device", "from": path, "to": movedPath}, &struct{}{})
	h.callTool(t, "file_delete", map[string]any{"device_id": "e2e-device", "path": movedPath}, &struct{}{})
	if _, err := os.Stat(movedPath); !os.IsNotExist(err) {
		t.Fatalf("file still exists after file_delete: err=%v", err)
	}
}

// TestE2E_CommandReadSandboxDeniesWriteAndNetwork covers that
// command_read must deny a write outside its scratch directory and
// deny network access, driven end to end over MCP against a real
// sandboxed OS process.
func TestE2E_CommandReadSandboxDeniesWriteAndNetwork(t *testing.T) {
	h := newHarness(t)

	// h.callTool fails the test on any tool error, so the "unsupported"
	// skip case (this Agent's kernel couldn't support the sandbox, per
	// addendum §4's capability-wiring note) is checked by calling the MCP
	// client directly instead, to inspect the tool error before deciding
	// whether to skip or fail.
	target := filepath.Join(t.TempDir(), "should-not-exist")
	res, err := h.mcpClient.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "command_read",
		Arguments: map[string]any{"device_id": "e2e-device", "command": "echo x > " + target, "timeout_ms": 5000},
	})
	if err != nil {
		t.Fatalf("CallTool command_read: %v", err)
	}
	if res.IsError {
		var msg string
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				msg += tc.Text
			}
		}
		if strings.Contains(msg, "unsupported") {
			t.Skip("Landlock not supported on this Agent's kernel")
		}
		t.Fatalf("command_read returned tool error: %s", msg)
	}
	var out struct {
		ExitCode         *int `json:"exit_code"`
		TimedOut         bool `json:"timed_out"`
		SandboxViolation bool `json:"sandbox_violation"`
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ExitCode == nil || *out.ExitCode == 0 {
		t.Fatalf("out = %+v, expected a non-zero exit from the denied write", out)
	}
	// The sandbox denying a mutation is still an RPC success,
	// distinguished from an unrelated non-zero exit by this flag.
	if !out.SandboxViolation {
		t.Fatalf("out = %+v, expected sandbox_violation=true for a denied write", out)
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("target file exists despite the sandbox: err=%v", statErr)
	}

	// The sandbox's kernel support is now confirmed (the write-denial
	// call above didn't skip), so this second call can safely use the
	// fail-fast h.callTool: addendum §4's verification requires the
	// network-denial case to also be a real e2e check, not mocked.
	ln, netErr := net.Listen("tcp", "127.0.0.1:0")
	if netErr != nil {
		t.Fatalf("Listen: %v", netErr)
	}
	defer ln.Close()

	var netOut struct {
		ExitCode *int `json:"exit_code"`
	}
	h.callTool(t, "command_read", map[string]any{
		"device_id": "e2e-device", "command": "echo connect-attempt > /dev/tcp/" + ln.Addr().String(), "timeout_ms": 5000,
	}, &netOut)
	if netOut.ExitCode == nil || *netOut.ExitCode == 0 {
		t.Fatal("network connect unexpectedly succeeded from inside the sandboxed command_read")
	}
}
