package proto

import "encoding/json"

// Message types (base spec §16.2).
const (
	TypeHello    = "hello"
	TypeRequest  = "request"
	TypeResponse = "response"
)

// Method names (base spec §16.3 example: "process.read").
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

// mutatingMethods are the methods that change Agent-side state. Base
// spec §17: if the transport is lost while one of these is in flight,
// the Gateway cannot tell whether the Agent applied it, so the caller
// must see execution_unknown rather than a plain transport_lost.
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

// Protocol-level error codes (base spec §18.1).
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
)

// Capabilities mirrors the "capabilities" object of the hello message
// (base spec §5.2).
type Capabilities struct {
	CommandRead bool `json:"command_read"`
	CommandExec bool `json:"command_exec"`
	Process     bool `json:"process"`
	Filesystem  bool `json:"filesystem"`
}

// Hello is the first message an Agent sends after connecting
// (base spec §5.2).
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
// WebSocket connection (base spec §16.3).
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

// RPCError is the "error" field of a failed Response (base spec §16.4).
// It implements the error interface so Gateway-side callers can return
// it directly.
type RPCError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return e.Code + ": " + e.Message }

// Response is the reply to a Request, correlated by RequestID
// (base spec §16.4, §16.5).
type Response struct {
	Type      string          `json:"type"`
	RequestID string          `json:"request_id"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     *RPCError       `json:"error,omitempty"`
}
