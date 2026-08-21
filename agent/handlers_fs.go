package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"command-relay-mcp/internal/proto"
)

// FileHandlers implements the Filesystem API (base spec §14, addendum
// §3). It is stateless: filesystem operations are not recorded in
// HistoryStore, which only tracks command/process executions.
type FileHandlers struct{}

func NewFileHandlers() *FileHandlers { return &FileHandlers{} }

// validatePath rejects the malformed-input cases base spec §14.2 calls
// out (empty path, NUL byte) and resolves the rest through
// filepath.Clean/Abs so a ".." segment is interpreted the same way the
// OS interprets it, rather than trusted string concatenation. V1 does
// not fence off any part of the host filesystem (addendum §3) — this is
// a robustness fix, not an access-control boundary.
func validatePath(path string) (string, *proto.RPCError) {
	if path == "" {
		return "", &proto.RPCError{Code: proto.ErrInvalidRequest, Message: "path must not be empty"}
	}
	if strings.ContainsRune(path, 0) {
		return "", &proto.RPCError{Code: proto.ErrInvalidRequest, Message: "path must not contain a NUL byte"}
	}
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", &proto.RPCError{Code: proto.ErrInvalidRequest, Message: err.Error()}
	}
	return abs, nil
}

// osErrorToRPCError maps a filesystem os.Error onto the existing base
// spec §18.1 codes: no filesystem-specific code exists, so a missing
// file/parent is treated as a bad request (the caller referenced a path
// that does not exist) and a permission failure as permission_denied.
func osErrorToRPCError(err error) *proto.RPCError {
	switch {
	case os.IsNotExist(err):
		return &proto.RPCError{Code: proto.ErrInvalidRequest, Message: err.Error()}
	case os.IsPermission(err):
		return &proto.RPCError{Code: proto.ErrPermissionDenied, Message: err.Error()}
	default:
		return &proto.RPCError{Code: proto.ErrInternal, Message: err.Error()}
	}
}

type FileReadParams struct {
	Path string `json:"path"`
}

type FileReadResult struct {
	ContentBase64 string `json:"content_base64"`
	Size          int64  `json:"size"`
}

// Read implements file.read (base spec §14.1).
func (h *FileHandlers) Read(ctx context.Context, raw json.RawMessage) (any, *proto.RPCError) {
	var p FileReadParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &proto.RPCError{Code: proto.ErrInvalidRequest, Message: err.Error()}
	}
	path, rpcErr := validatePath(p.Path)
	if rpcErr != nil {
		return nil, rpcErr
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, osErrorToRPCError(err)
	}
	return FileReadResult{ContentBase64: base64.StdEncoding.EncodeToString(data), Size: int64(len(data))}, nil
}

type FileStatParams struct {
	Path string `json:"path"`
}

type FileStatResult struct {
	Size    int64  `json:"size"`
	Mode    string `json:"mode"`
	ModTime string `json:"mod_time"`
	IsDir   bool   `json:"is_dir"`
}

// Stat implements file.stat (base spec §14.1).
func (h *FileHandlers) Stat(ctx context.Context, raw json.RawMessage) (any, *proto.RPCError) {
	var p FileStatParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &proto.RPCError{Code: proto.ErrInvalidRequest, Message: err.Error()}
	}
	path, rpcErr := validatePath(p.Path)
	if rpcErr != nil {
		return nil, rpcErr
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, osErrorToRPCError(err)
	}
	return FileStatResult{
		Size: info.Size(), Mode: info.Mode().String(),
		ModTime: info.ModTime().UTC().Format(time.RFC3339), IsDir: info.IsDir(),
	}, nil
}

type FileListParams struct {
	Path string `json:"path"`
}

type FileEntry struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Mode    string `json:"mode"`
	ModTime string `json:"mod_time"`
	IsDir   bool   `json:"is_dir"`
}

type FileListResult struct {
	Entries []FileEntry `json:"entries"`
}

// List implements file.list (base spec §14.1).
func (h *FileHandlers) List(ctx context.Context, raw json.RawMessage) (any, *proto.RPCError) {
	var p FileListParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &proto.RPCError{Code: proto.ErrInvalidRequest, Message: err.Error()}
	}
	path, rpcErr := validatePath(p.Path)
	if rpcErr != nil {
		return nil, rpcErr
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, osErrorToRPCError(err)
	}
	out := make([]FileEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue // entry vanished between readdir and stat; skip rather than fail the whole listing
		}
		out = append(out, FileEntry{
			Name: e.Name(), Size: info.Size(), Mode: info.Mode().String(),
			ModTime: info.ModTime().UTC().Format(time.RFC3339), IsDir: e.IsDir(),
		})
	}
	return FileListResult{Entries: out}, nil
}

type FileWriteParams struct {
	Path          string `json:"path"`
	ContentBase64 string `json:"content_base64"`
	// Mode is "truncate" (default) or "append" (addendum §3).
	Mode string `json:"mode,omitempty"`
}

// Write implements file.write (base spec §14.2). Not atomic — a partial
// write on crash is an accepted V1 risk, per the base spec's own text.
func (h *FileHandlers) Write(ctx context.Context, raw json.RawMessage) (any, *proto.RPCError) {
	var p FileWriteParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &proto.RPCError{Code: proto.ErrInvalidRequest, Message: err.Error()}
	}
	path, rpcErr := validatePath(p.Path)
	if rpcErr != nil {
		return nil, rpcErr
	}
	data, err := base64.StdEncoding.DecodeString(p.ContentBase64)
	if err != nil {
		return nil, &proto.RPCError{Code: proto.ErrInvalidRequest, Message: "content_base64: " + err.Error()}
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	if p.Mode == "append" {
		flags = os.O_WRONLY | os.O_CREATE | os.O_APPEND
	}
	f, err := os.OpenFile(path, flags, 0o644)
	if err != nil {
		return nil, osErrorToRPCError(err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return nil, osErrorToRPCError(err)
	}
	return struct{}{}, nil
}

type FileMoveParams struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Move implements file.move (base spec §14.2).
func (h *FileHandlers) Move(ctx context.Context, raw json.RawMessage) (any, *proto.RPCError) {
	var p FileMoveParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &proto.RPCError{Code: proto.ErrInvalidRequest, Message: err.Error()}
	}
	from, rpcErr := validatePath(p.From)
	if rpcErr != nil {
		return nil, rpcErr
	}
	to, rpcErr := validatePath(p.To)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if err := os.Rename(from, to); err != nil {
		return nil, osErrorToRPCError(err)
	}
	return struct{}{}, nil
}

type FileDeleteParams struct {
	Path string `json:"path"`
}

// Delete implements file.delete (base spec §14.2).
func (h *FileHandlers) Delete(ctx context.Context, raw json.RawMessage) (any, *proto.RPCError) {
	var p FileDeleteParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &proto.RPCError{Code: proto.ErrInvalidRequest, Message: err.Error()}
	}
	path, rpcErr := validatePath(p.Path)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if err := os.Remove(path); err != nil {
		return nil, osErrorToRPCError(err)
	}
	return struct{}{}, nil
}

type DirectoryCreateParams struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
}

// CreateDirectory implements directory.create (base spec §14.2).
// Non-recursive by default (mkdir semantics, not mkdir -p) — the base
// spec is silent here, so this plan picks the smaller, more predictable
// default (addendum §3).
func (h *FileHandlers) CreateDirectory(ctx context.Context, raw json.RawMessage) (any, *proto.RPCError) {
	var p DirectoryCreateParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &proto.RPCError{Code: proto.ErrInvalidRequest, Message: err.Error()}
	}
	path, rpcErr := validatePath(p.Path)
	if rpcErr != nil {
		return nil, rpcErr
	}
	var err error
	if p.Recursive {
		err = os.MkdirAll(path, 0o755)
	} else {
		err = os.Mkdir(path, 0o755)
	}
	if err != nil {
		return nil, osErrorToRPCError(err)
	}
	return struct{}{}, nil
}
