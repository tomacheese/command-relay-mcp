package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"command-relay-mcp/agent"
	"command-relay-mcp/internal/proto"
	"command-relay-mcp/internal/version"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// --- devices_list / device_ping ---

type DeviceSummary struct {
	DeviceID string `json:"device_id"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
}

type DevicesListResult struct {
	Devices []DeviceSummary `json:"devices"`
}

type DeviceIDParams struct {
	DeviceID string `json:"device_id"`
}

// deviceCall resolves device_id to its AgentConn, or returns a
// device_offline tool error if it is not registered. It also logs MCP
// tool invocation metadata and routing failures — never the
// params/result payload itself, which may carry command output.
func deviceCall(ctx context.Context, reg *Registry, deviceID, method string, params any) (json.RawMessage, error) {
	conn, ok := reg.Get(deviceID)
	if !ok {
		log.Printf("gateway: routing failure: device %q is offline for %s", deviceID, method)
		return nil, &proto.RPCError{Code: proto.ErrDeviceOffline, Message: fmt.Sprintf("device %q is offline", deviceID)}
	}
	log.Printf("gateway: invoking %s on device %q", method, deviceID)
	return conn.Call(ctx, method, params)
}

// NewMCPServer registers every V1 tool group against the given
// Registry, for Linux only. Every tool is registered unconditionally
// regardless of any device's advertised capabilities
// (proto.Capabilities): the "MCP tool 呼び出しはunsupported で
// 失敗させる" requirement is met entirely by the Agent's own per-call
// capability check (e.g. CommandHandlers.Read returning ErrUnsupported
// when sandboxMgr is nil), not by the Gateway declining to register or
// route the tool.
func NewMCPServer(reg *Registry) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "command-relay-mcp", Version: version.Version}, nil)

	mcp.AddTool(server, &mcp.Tool{Name: "devices_list", Description: "List Agents currently connected to the Gateway."},
		func(ctx context.Context, req *mcp.CallToolRequest, in struct{}) (*mcp.CallToolResult, DevicesListResult, error) {
			var out DevicesListResult
			for _, d := range reg.List() {
				out.Devices = append(out.Devices, DeviceSummary{DeviceID: d.DeviceID, OS: d.OS, Arch: d.Arch})
			}
			return nil, out, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "device_ping", Description: "Check that a specific device is reachable.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, req *mcp.CallToolRequest, in DeviceIDParams) (*mcp.CallToolResult, agent.PingResult, error) {
			raw, err := deviceCall(ctx, reg, in.DeviceID, proto.MethodDevicePing, struct{}{})
			if err != nil {
				return nil, agent.PingResult{}, err
			}
			var out agent.PingResult
			if err := json.Unmarshal(raw, &out); err != nil {
				return nil, agent.PingResult{}, err
			}
			return nil, out, nil
		})

	registerCommandTools(server, reg)
	registerProcessTools(server, reg)
	registerExecutionTools(server, reg)
	registerFilesystemTools(server, reg)

	return server
}

// NewMCPHTTPHandlerNoAuth is a deliberate, temporary rollback: OAuth and
// bearer-token verification were both removed to unblock a client whose
// connector setup failed against them. Re-add a verifier before this
// runs on anything but a trusted network.
func NewMCPHTTPHandlerNoAuth(reg *Registry) http.Handler {
	server := NewMCPServer(reg)
	return mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { return server }, nil)
}

// NewMCPMux mounts the MCP handler only at /mcp: a client probing an
// unrelated well-known path (e.g. OAuth discovery) must get a plain
// 404, not the MCP handler's own 400 "Mcp-Session-Id header required"
// — some clients treat that malformed-looking response as a discovery
// failure instead of "no OAuth here".
func NewMCPMux(reg *Registry) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/mcp", NewMCPHTTPHandlerNoAuth(reg))
	return mux
}

// --- command_exec / command_read ---

type CommandExecToolParams struct {
	DeviceID  string            `json:"device_id"`
	Command   string            `json:"command"`
	Cwd       string            `json:"cwd,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	TimeoutMs int               `json:"timeout_ms,omitempty"`
}

func (p CommandExecToolParams) toAgentParams(req *mcp.CallToolRequest) agent.CommandExecParams {
	out := agent.CommandExecParams{Command: p.Command, Cwd: p.Cwd, Env: p.Env, TimeoutMs: p.TimeoutMs}
	if req.Session != nil {
		out.ClientContextID = req.Session.ID()
	}
	if req.Extra != nil && req.Extra.TokenInfo != nil {
		out.ClientSubject = req.Extra.TokenInfo.UserID
	}
	return out
}

func registerCommandTools(server *mcp.Server, reg *Registry) {
	mcp.AddTool(server, &mcp.Tool{Name: "command_exec", Description: "Run a shell command with normal Agent privileges. Can change host state."},
		func(ctx context.Context, req *mcp.CallToolRequest, in CommandExecToolParams) (*mcp.CallToolResult, agent.CommandExecResult, error) {
			raw, err := deviceCall(ctx, reg, in.DeviceID, proto.MethodCommandExec, in.toAgentParams(req))
			if err != nil {
				return nil, agent.CommandExecResult{}, err
			}
			var out agent.CommandExecResult
			if err := json.Unmarshal(raw, &out); err != nil {
				return nil, agent.CommandExecResult{}, err
			}
			return nil, out, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "command_read", Description: "Run a shell command inside a read-only sandbox. Returns unsupported if the target device's Agent has no working sandbox backend.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, req *mcp.CallToolRequest, in CommandExecToolParams) (*mcp.CallToolResult, agent.CommandExecResult, error) {
			raw, err := deviceCall(ctx, reg, in.DeviceID, proto.MethodCommandRead, in.toAgentParams(req))
			if err != nil {
				return nil, agent.CommandExecResult{}, err
			}
			var out agent.CommandExecResult
			if err := json.Unmarshal(raw, &out); err != nil {
				return nil, agent.CommandExecResult{}, err
			}
			return nil, out, nil
		})
}

// --- process_* ---

type ProcessStartToolParams struct {
	DeviceID string            `json:"device_id"`
	Command  string            `json:"command"`
	Cwd      string            `json:"cwd,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
}

func processStartParams(req *mcp.CallToolRequest, in ProcessStartToolParams) agent.ProcessStartParams {
	out := agent.ProcessStartParams{Command: in.Command, Cwd: in.Cwd, Env: in.Env}
	if req.Session != nil {
		out.ClientContextID = req.Session.ID()
	}
	if req.Extra != nil && req.Extra.TokenInfo != nil {
		out.ClientSubject = req.Extra.TokenInfo.UserID
	}
	return out
}

type ProcessReadToolParams struct {
	DeviceID     string `json:"device_id"`
	ProcessID    string `json:"process_id"`
	StdoutOffset int64  `json:"stdout_offset"`
	StderrOffset int64  `json:"stderr_offset"`
	MaxBytes     int    `json:"max_bytes,omitempty"`
}

type ProcessWriteToolParams struct {
	DeviceID  string `json:"device_id"`
	ProcessID string `json:"process_id"`
	Data      string `json:"data"`
}

type ProcessWaitToolParams struct {
	DeviceID  string `json:"device_id"`
	ProcessID string `json:"process_id"`
	TimeoutMs int    `json:"timeout_ms,omitempty"`
}

type ProcessTerminateToolParams struct {
	DeviceID  string `json:"device_id"`
	ProcessID string `json:"process_id"`
	GraceMs   int    `json:"grace_ms,omitempty"`
}

func registerProcessTools(server *mcp.Server, reg *Registry) {
	mcp.AddTool(server, &mcp.Tool{Name: "process_start", Description: "Start a long-running process without waiting for it to finish."},
		func(ctx context.Context, req *mcp.CallToolRequest, in ProcessStartToolParams) (*mcp.CallToolResult, agent.ProcessStartResult, error) {
			raw, err := deviceCall(ctx, reg, in.DeviceID, proto.MethodProcessStart, processStartParams(req, in))
			if err != nil {
				return nil, agent.ProcessStartResult{}, err
			}
			var out agent.ProcessStartResult
			if err := json.Unmarshal(raw, &out); err != nil {
				return nil, agent.ProcessStartResult{}, err
			}
			return nil, out, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "process_read", Description: "Pull stdout/stderr from a tracked process, by byte offset.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, req *mcp.CallToolRequest, in ProcessReadToolParams) (*mcp.CallToolResult, agent.ProcessReadResult, error) {
			raw, err := deviceCall(ctx, reg, in.DeviceID, proto.MethodProcessRead, agent.ProcessReadParams{
				ProcessID: in.ProcessID, StdoutOffset: in.StdoutOffset, StderrOffset: in.StderrOffset, MaxBytes: in.MaxBytes,
			})
			if err != nil {
				return nil, agent.ProcessReadResult{}, err
			}
			var out agent.ProcessReadResult
			if err := json.Unmarshal(raw, &out); err != nil {
				return nil, agent.ProcessReadResult{}, err
			}
			return nil, out, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "process_write", Description: "Write UTF-8 text to a tracked process's stdin."},
		func(ctx context.Context, req *mcp.CallToolRequest, in ProcessWriteToolParams) (*mcp.CallToolResult, struct{}, error) {
			_, err := deviceCall(ctx, reg, in.DeviceID, proto.MethodProcessWrite, agent.ProcessWriteParams{ProcessID: in.ProcessID, Data: in.Data})
			return nil, struct{}{}, err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "process_wait", Description: "Wait for a tracked process to exit. A timeout never kills it.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, req *mcp.CallToolRequest, in ProcessWaitToolParams) (*mcp.CallToolResult, agent.ProcessWaitResult, error) {
			raw, err := deviceCall(ctx, reg, in.DeviceID, proto.MethodProcessWait, agent.ProcessWaitParams{ProcessID: in.ProcessID, TimeoutMs: in.TimeoutMs})
			if err != nil {
				return nil, agent.ProcessWaitResult{}, err
			}
			var out agent.ProcessWaitResult
			if err := json.Unmarshal(raw, &out); err != nil {
				return nil, agent.ProcessWaitResult{}, err
			}
			return nil, out, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "process_terminate", Description: "Terminate a tracked process's whole process tree."},
		func(ctx context.Context, req *mcp.CallToolRequest, in ProcessTerminateToolParams) (*mcp.CallToolResult, agent.ProcessTerminateResult, error) {
			raw, err := deviceCall(ctx, reg, in.DeviceID, proto.MethodProcessTerminate, agent.ProcessTerminateParams{ProcessID: in.ProcessID, GraceMs: in.GraceMs})
			if err != nil {
				return nil, agent.ProcessTerminateResult{}, err
			}
			var out agent.ProcessTerminateResult
			if err := json.Unmarshal(raw, &out); err != nil {
				return nil, agent.ProcessTerminateResult{}, err
			}
			return nil, out, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "process_list", Description: "List processes tracked by a device's Agent.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, req *mcp.CallToolRequest, in DeviceIDParams) (*mcp.CallToolResult, agent.ProcessListResult, error) {
			raw, err := deviceCall(ctx, reg, in.DeviceID, proto.MethodProcessList, struct{}{})
			if err != nil {
				return nil, agent.ProcessListResult{}, err
			}
			var out agent.ProcessListResult
			if err := json.Unmarshal(raw, &out); err != nil {
				return nil, agent.ProcessListResult{}, err
			}
			return nil, out, nil
		})
}

// --- execution_list / execution_get ---

type ExecutionListToolParams struct {
	DeviceID string `json:"device_id"`
	Limit    int    `json:"limit,omitempty"`
}

type ExecutionGetToolParams struct {
	DeviceID    string `json:"device_id"`
	ExecutionID string `json:"execution_id"`
}

func registerExecutionTools(server *mcp.Server, reg *Registry) {
	mcp.AddTool(server, &mcp.Tool{Name: "execution_list", Description: "List recent command/process executions recorded by a device's Agent history.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, req *mcp.CallToolRequest, in ExecutionListToolParams) (*mcp.CallToolResult, agent.ExecutionListResult, error) {
			raw, err := deviceCall(ctx, reg, in.DeviceID, proto.MethodExecutionList, agent.ExecutionListParams{Limit: in.Limit})
			if err != nil {
				return nil, agent.ExecutionListResult{}, err
			}
			var out agent.ExecutionListResult
			if err := json.Unmarshal(raw, &out); err != nil {
				return nil, agent.ExecutionListResult{}, err
			}
			return nil, out, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "execution_get", Description: "Get one recorded execution by execution_id.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, req *mcp.CallToolRequest, in ExecutionGetToolParams) (*mcp.CallToolResult, agent.ExecutionGetResult, error) {
			raw, err := deviceCall(ctx, reg, in.DeviceID, proto.MethodExecutionGet, agent.ExecutionGetParams{ExecutionID: in.ExecutionID})
			if err != nil {
				return nil, agent.ExecutionGetResult{}, err
			}
			var out agent.ExecutionGetResult
			if err := json.Unmarshal(raw, &out); err != nil {
				return nil, agent.ExecutionGetResult{}, err
			}
			return nil, out, nil
		})
}

// --- file_* / directory_create ---

type FilePathToolParams struct {
	DeviceID string `json:"device_id"`
	Path     string `json:"path"`
}

type FileWriteToolParams struct {
	DeviceID      string `json:"device_id"`
	Path          string `json:"path"`
	ContentBase64 string `json:"content_base64"`
	Mode          string `json:"mode,omitempty"`
}

type FileMoveToolParams struct {
	DeviceID string `json:"device_id"`
	From     string `json:"from"`
	To       string `json:"to"`
}

type DirectoryCreateToolParams struct {
	DeviceID  string `json:"device_id"`
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
}

func registerFilesystemTools(server *mcp.Server, reg *Registry) {
	mcp.AddTool(server, &mcp.Tool{Name: "file_read", Description: "Read a file's content as base64.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, req *mcp.CallToolRequest, in FilePathToolParams) (*mcp.CallToolResult, agent.FileReadResult, error) {
			raw, err := deviceCall(ctx, reg, in.DeviceID, proto.MethodFileRead, agent.FileReadParams{Path: in.Path})
			if err != nil {
				return nil, agent.FileReadResult{}, err
			}
			var out agent.FileReadResult
			if err := json.Unmarshal(raw, &out); err != nil {
				return nil, agent.FileReadResult{}, err
			}
			return nil, out, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "file_stat", Description: "Get a file's size, mode, mtime, and whether it's a directory.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, req *mcp.CallToolRequest, in FilePathToolParams) (*mcp.CallToolResult, agent.FileStatResult, error) {
			raw, err := deviceCall(ctx, reg, in.DeviceID, proto.MethodFileStat, agent.FileStatParams{Path: in.Path})
			if err != nil {
				return nil, agent.FileStatResult{}, err
			}
			var out agent.FileStatResult
			if err := json.Unmarshal(raw, &out); err != nil {
				return nil, agent.FileStatResult{}, err
			}
			return nil, out, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "file_list", Description: "List a directory's entries.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		func(ctx context.Context, req *mcp.CallToolRequest, in FilePathToolParams) (*mcp.CallToolResult, agent.FileListResult, error) {
			raw, err := deviceCall(ctx, reg, in.DeviceID, proto.MethodFileList, agent.FileListParams{Path: in.Path})
			if err != nil {
				return nil, agent.FileListResult{}, err
			}
			var out agent.FileListResult
			if err := json.Unmarshal(raw, &out); err != nil {
				return nil, agent.FileListResult{}, err
			}
			return nil, out, nil
		})

	mcp.AddTool(server, &mcp.Tool{Name: "file_write", Description: "Write base64 content to a file. Not atomic."},
		func(ctx context.Context, req *mcp.CallToolRequest, in FileWriteToolParams) (*mcp.CallToolResult, struct{}, error) {
			_, err := deviceCall(ctx, reg, in.DeviceID, proto.MethodFileWrite, agent.FileWriteParams{Path: in.Path, ContentBase64: in.ContentBase64, Mode: in.Mode})
			return nil, struct{}{}, err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "file_move", Description: "Rename/move a file."},
		func(ctx context.Context, req *mcp.CallToolRequest, in FileMoveToolParams) (*mcp.CallToolResult, struct{}, error) {
			_, err := deviceCall(ctx, reg, in.DeviceID, proto.MethodFileMove, agent.FileMoveParams{From: in.From, To: in.To})
			return nil, struct{}{}, err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "file_delete", Description: "Delete a file."},
		func(ctx context.Context, req *mcp.CallToolRequest, in FilePathToolParams) (*mcp.CallToolResult, struct{}, error) {
			_, err := deviceCall(ctx, reg, in.DeviceID, proto.MethodFileDelete, agent.FileDeleteParams{Path: in.Path})
			return nil, struct{}{}, err
		})

	mcp.AddTool(server, &mcp.Tool{Name: "directory_create", Description: "Create a directory. Non-recursive unless recursive:true."},
		func(ctx context.Context, req *mcp.CallToolRequest, in DirectoryCreateToolParams) (*mcp.CallToolResult, struct{}, error) {
			_, err := deviceCall(ctx, reg, in.DeviceID, proto.MethodDirectoryCreate, agent.DirectoryCreateParams{Path: in.Path, Recursive: in.Recursive})
			return nil, struct{}{}, err
		})
}
