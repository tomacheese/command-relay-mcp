package proto

import "encoding/json"

// Message types identify the kind of JSON payload sent over the wire.
const (
	TypeHello    = "hello"
	TypeRequest  = "request"
	TypeResponse = "response"
)

// Method names identify the RPC being invoked, e.g. "process.read".
const (
	MethodDevicePing       = "device.ping"
	MethodCommandExec      = "command.exec"
	MethodCommandRead      = "command.read"
	MethodProcessStart     = "process.start"
	MethodProcessRead      = "process.read"
	MethodProcessWrite     = "process.write"
	MethodProcessWait      = "process.wait"
	MethodProcessTerminate = "process.terminate"
	MethodProcessList      = "process.list"
	MethodExecutionList    = "execution.list"
	MethodExecutionGet     = "execution.get"
	MethodFileRead         = "file.read"
	MethodFileStat         = "file.stat"
	MethodFileList         = "file.list"
	MethodFileWrite        = "file.write"
	MethodFileMove         = "file.move"
	MethodFileDelete       = "file.delete"
	MethodDirectoryCreate  = "directory.create"
)

// mutatingMethods are the methods that change Agent-side state. If the
// transport is lost while one of these is in flight, the Gateway cannot
// tell whether the Agent applied it, so the caller must see
// execution_unknown rather than a plain transport_lost.
var mutatingMethods = map[string]bool{
	MethodCommandExec:      true,
	MethodProcessStart:     true,
	MethodProcessWrite:     true,
	MethodProcessTerminate: true,
	MethodFileWrite:        true,
	MethodFileMove:         true,
	MethodFileDelete:       true,
	MethodDirectoryCreate:  true,
}

func IsMutatingMethod(method string) bool { return mutatingMethods[method] }

// Protocol-level error codes returned in RPCError.Code.
const (
	ErrDeviceOffline    = "device_offline"
	ErrUnsupported      = "unsupported"
	ErrProcessNotFound  = "process_not_found"
	ErrPermissionDenied = "permission_denied"
	ErrSandboxViolation = "sandbox_violation"
	ErrInvalidRequest   = "invalid_request"
	ErrTimeout          = "timeout"
	ErrTransportLost    = "transport_lost"
	ErrExecutionUnknown = "execution_unknown"
	ErrInternal         = "internal_error"
	// ErrFileTooLarge is returned by file.read when the target file's
	// size exceeds MaxFileReadBytes; the file is never opened for
	// reading in that case.
	ErrFileTooLarge = "file_too_large"
)

// MaxRPCMessageBytes is the maximum size of a single WebSocket message
// (request or response) between Agent and Gateway, applied via
// (*websocket.Conn).SetReadLimit on both ends. It must stay well above
// every application-level size cap below, since JSON string-escaping
// can inflate a byte stream by up to ~6x in the worst case (every byte
// a control character).
const MaxRPCMessageBytes int64 = 32 << 20 // 32 MiB

// MaxCommandOutputBytes caps stdout/stderr returned by command.exec,
// command.read, and process.read per stream. Larger output must be
// retrieved by paging through process.start + process.read instead.
const MaxCommandOutputBytes = 2 << 20 // 2 MiB per stream

// MaxFileReadBytes caps the raw (pre-base64) file size file.read will
// return in one response; larger files are rejected with
// ErrFileTooLarge instead of being partially or fully loaded.
const MaxFileReadBytes int64 = 8 << 20 // 8 MiB

// Capabilities mirrors the "capabilities" object of the hello message.
type Capabilities struct {
	CommandRead bool `json:"command_read"`
	CommandExec bool `json:"command_exec"`
	Process     bool `json:"process"`
	Filesystem  bool `json:"filesystem"`
}

// Hello is the first message an Agent sends after connecting.
type Hello struct {
	Type         string       `json:"type"`
	DeviceID     string       `json:"device_id"`
	DeviceSecret string       `json:"device_secret"`
	AgentVersion string       `json:"agent_version"`
	OS           string       `json:"os"`
	Arch         string       `json:"arch"`
	Capabilities Capabilities `json:"capabilities"`
}

// Request is a single RPC call multiplexed on the Agent<->Gateway
// WebSocket connection.
type Request struct {
	Type      string          `json:"type"`
	RequestID string          `json:"request_id"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
}

// NewRequest marshals params and returns a Request with Type set;
// callers must still assign a unique RequestID before sending it.
func NewRequest(method string, params any) (*Request, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	return &Request{Type: TypeRequest, Method: method, Params: raw}, nil
}

// RPCError is the "error" field of a failed Response.
// It implements the error interface so Gateway-side callers can return
// it directly.
type RPCError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return e.Code + ": " + e.Message }

// Response is the reply to a Request, correlated by RequestID.
type Response struct {
	Type      string          `json:"type"`
	RequestID string          `json:"request_id"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *RPCError       `json:"error,omitempty"`
}
