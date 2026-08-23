package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"command-relay-mcp/internal/proto"
)

// FileHandlers implements the Filesystem API. It is stateless:
// filesystem operations are not recorded in HistoryStore, which only
// tracks command/process executions.
type FileHandlers struct{}

func NewFileHandlers() *FileHandlers { return &FileHandlers{} }

// validatePath rejects malformed-input cases (empty path, NUL byte)
// and resolves the rest through filepath.Clean/Abs so a ".." segment
// is interpreted the same way the OS interprets it, rather than
// trusted string concatenation. V1 does not fence off any part of the
// host filesystem — this is a robustness fix, not an access-control
// boundary.
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

// osErrorToRPCError maps a filesystem os.Error onto the existing
// protocol error codes: no filesystem-specific code exists, so a
// missing file/parent is treated as a bad request (the caller
// referenced a path that does not exist) and a permission failure as
// permission_denied.
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

// Read implements file.read.
func (h *FileHandlers) Read(ctx context.Context, raw json.RawMessage) (any, *proto.RPCError) {
	var p FileReadParams
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
	if info.Size() > proto.MaxFileReadBytes {
		return nil, &proto.RPCError{Code: proto.ErrFileTooLarge, Message: fmt.Sprintf("file size %d exceeds the %d byte limit", info.Size(), proto.MaxFileReadBytes)}
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

// Stat implements file.stat.
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

// List implements file.list.
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
	// Mode is "" or "truncate" (default) or "append"; any other value
	// is rejected rather than silently defaulting to truncate.
	Mode string `json:"mode,omitempty"`
}

// Write implements file.write. Not atomic — a partial write on crash
// is an accepted risk in this V1 implementation.
func (h *FileHandlers) Write(ctx context.Context, raw json.RawMessage) (any, *proto.RPCError) {
	var p FileWriteParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, &proto.RPCError{Code: proto.ErrInvalidRequest, Message: err.Error()}
	}
	switch p.Mode {
	case "", "truncate", "append":
	default:
		return nil, &proto.RPCError{Code: proto.ErrInvalidRequest, Message: `mode must be "truncate" or "append"`}
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
	// Close()'s error is checked, not deferred-and-ignored: on some
	// filesystems (e.g. NFS, or a disk-quota failure) a successful
	// Write() can still be silently discarded until Close(), so the
	// caller must not see an RPC success unless Close() also succeeded.
	if _, err := f.Write(data); err != nil {
		f.Close()
		return nil, osErrorToRPCError(err)
	}
	if err := f.Close(); err != nil {
		return nil, osErrorToRPCError(err)
	}
	return struct{}{}, nil
}

type FileMoveParams struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Move implements file.move.
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

// Delete implements file.delete.
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

// CreateDirectory implements directory.create. Non-recursive by
// default (mkdir semantics, not mkdir -p) — the smaller, more
// predictable default.
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
