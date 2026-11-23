package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"command-relay-mcp/internal/proto"
)

func TestFileWriteThenRead_RoundTrips(t *testing.T) {
	h := &FileHandlers{}
	path := filepath.Join(t.TempDir(), "a.txt")

	writeParams, _ := json.Marshal(FileWriteParams{Path: path, ContentBase64: base64.StdEncoding.EncodeToString([]byte("hello"))})
	if _, rpcErr := h.Write(context.Background(), writeParams); rpcErr != nil {
		t.Fatalf("Write: %+v", rpcErr)
	}

	readParams, _ := json.Marshal(FileReadParams{Path: path})
	result, rpcErr := h.Read(context.Background(), readParams)
	if rpcErr != nil {
		t.Fatalf("Read: %+v", rpcErr)
	}
	res := result.(FileReadResult)
	got, err := base64.StdEncoding.DecodeString(res.ContentBase64)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("content = %q, want hello", got)
	}
}

func TestFileWriteAppendMode_AppendsRatherThanTruncates(t *testing.T) {
	h := &FileHandlers{}
	path := filepath.Join(t.TempDir(), "a.txt")

	for _, chunk := range []string{"ab", "cd"} {
		params, _ := json.Marshal(FileWriteParams{Path: path, ContentBase64: base64.StdEncoding.EncodeToString([]byte(chunk)), Mode: "append"})
		if _, rpcErr := h.Write(context.Background(), params); rpcErr != nil {
			t.Fatalf("Write: %+v", rpcErr)
		}
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "abcd" {
		t.Fatalf("content = %q, want abcd", got)
	}
}

func TestFileStat_ReportsSizeAndIsDir(t *testing.T) {
	h := &FileHandlers{}
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(file, []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	params, _ := json.Marshal(FileStatParams{Path: file})
	result, rpcErr := h.Stat(context.Background(), params)
	if rpcErr != nil {
		t.Fatalf("Stat: %+v", rpcErr)
	}
	res := result.(FileStatResult)
	if res.Size != 5 || res.IsDir {
		t.Fatalf("res = %+v", res)
	}
}

func TestFileList_ListsDirectoryEntries(t *testing.T) {
	h := &FileHandlers{}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}

	params, _ := json.Marshal(FileListParams{Path: dir})
	result, rpcErr := h.List(context.Background(), params)
	if rpcErr != nil {
		t.Fatalf("List: %+v", rpcErr)
	}
	res := result.(FileListResult)
	if len(res.Entries) != 2 {
		t.Fatalf("entries = %+v", res.Entries)
	}
}

func TestFileMove_RenamesFile(t *testing.T) {
	h := &FileHandlers{}
	dir := t.TempDir()
	from := filepath.Join(dir, "a.txt")
	to := filepath.Join(dir, "b.txt")
	if err := os.WriteFile(from, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	params, _ := json.Marshal(FileMoveParams{From: from, To: to})
	if _, rpcErr := h.Move(context.Background(), params); rpcErr != nil {
		t.Fatalf("Move: %+v", rpcErr)
	}
	if _, err := os.Stat(to); err != nil {
		t.Fatalf("moved file missing: %v", err)
	}
	if _, err := os.Stat(from); !os.IsNotExist(err) {
		t.Fatalf("original file still present: %v", err)
	}
}

func TestFileDelete_RemovesFile(t *testing.T) {
	h := &FileHandlers{}
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	params, _ := json.Marshal(FileDeleteParams{Path: path})
	if _, rpcErr := h.Delete(context.Background(), params); rpcErr != nil {
		t.Fatalf("Delete: %+v", rpcErr)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file still present: %v", err)
	}
}

func TestDirectoryCreate_NonRecursiveFailsOnMissingParent(t *testing.T) {
	h := &FileHandlers{}
	dir := filepath.Join(t.TempDir(), "missing-parent", "child")

	params, _ := json.Marshal(DirectoryCreateParams{Path: dir})
	_, rpcErr := h.CreateDirectory(context.Background(), params)
	if rpcErr == nil {
		t.Fatal("expected an error for a missing parent without recursive:true")
	}
}

func TestDirectoryCreate_RecursiveCreatesParents(t *testing.T) {
	h := &FileHandlers{}
	dir := filepath.Join(t.TempDir(), "missing-parent", "child")

	params, _ := json.Marshal(DirectoryCreateParams{Path: dir, Recursive: true})
	if _, rpcErr := h.CreateDirectory(context.Background(), params); rpcErr != nil {
		t.Fatalf("CreateDirectory: %+v", rpcErr)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("directory not created: err=%v", err)
	}
}

func TestFileRead_RejectsEmptyPath(t *testing.T) {
	h := &FileHandlers{}
	params, _ := json.Marshal(FileReadParams{Path: ""})
	_, rpcErr := h.Read(context.Background(), params)
	if rpcErr == nil || rpcErr.Code != proto.ErrInvalidRequest {
		t.Fatalf("rpcErr = %+v, want invalid_request", rpcErr)
	}
}

func TestFileWrite_RejectsUnknownMode(t *testing.T) {
	h := &FileHandlers{}
	path := filepath.Join(t.TempDir(), "a.txt")

	params, _ := json.Marshal(FileWriteParams{Path: path, ContentBase64: base64.StdEncoding.EncodeToString([]byte("x")), Mode: "trunc"})
	_, rpcErr := h.Write(context.Background(), params)
	if rpcErr == nil || rpcErr.Code != proto.ErrInvalidRequest {
		t.Fatalf("rpcErr = %+v, want invalid_request", rpcErr)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file should not have been created for a rejected mode: err=%v", err)
	}
}

// TestFileRead_RejectsOversizedFileWithoutLoadingIt verifies that
// file.read rejects a file larger than proto.MaxFileReadBytes with
// ErrFileTooLarge, via a size check rather than loading the file into
// memory first.
func TestFileRead_RejectsOversizedFileWithoutLoadingIt(t *testing.T) {
	h := &FileHandlers{}
	path := filepath.Join(t.TempDir(), "big.bin")

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Sparse file: Truncate sets the logical size without allocating
	// (or loading) proto.MaxFileReadBytes+1 bytes of real data, so this
	// test stays fast and light on memory regardless of the outcome
	// being verified.
	if err := f.Truncate(proto.MaxFileReadBytes + 1); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	readParams, _ := json.Marshal(FileReadParams{Path: path})
	_, rpcErr := h.Read(context.Background(), readParams)
	if rpcErr == nil {
		t.Fatal("Read: want an error for an oversized file, got nil")
	}
	if rpcErr.Code != proto.ErrFileTooLarge {
		t.Fatalf("rpcErr.Code = %q, want %q", rpcErr.Code, proto.ErrFileTooLarge)
	}
}

func TestFileRead_EnforcesLimitOnOpenedFileAfterGrowth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "growing.bin")
	if err := os.WriteFile(path, []byte("initial"), 0o600); err != nil {
		t.Fatal(err)
	}

	opened, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	initialInfo, err := opened.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if initialInfo.Size() > proto.MaxFileReadBytes {
		t.Fatalf("initial size = %d, want at most %d", initialInfo.Size(), proto.MaxFileReadBytes)
	}

	appender, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := appender.Truncate(proto.MaxFileReadBytes + 1); err != nil {
		appender.Close()
		t.Fatal(err)
	}
	if err := appender.Close(); err != nil {
		t.Fatal(err)
	}

	_, rpcErr := readOpenedFile(opened)
	if rpcErr == nil || rpcErr.Code != proto.ErrFileTooLarge {
		t.Fatalf("readOpenedFile error = %+v, want %q", rpcErr, proto.ErrFileTooLarge)
	}
}

func TestFileRead_AcceptsFileAtLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "limit.bin")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(proto.MaxFileReadBytes); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	params, _ := json.Marshal(FileReadParams{Path: path})
	result, rpcErr := (&FileHandlers{}).Read(context.Background(), params)
	if rpcErr != nil {
		t.Fatalf("Read: %+v", rpcErr)
	}
	if got := result.(FileReadResult).Size; got != proto.MaxFileReadBytes {
		t.Fatalf("Read size = %d, want %d", got, proto.MaxFileReadBytes)
	}
}

func TestFileRead_RejectsNonRegularFiles(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "pipe")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{fifo, "/dev/zero"} {
		t.Run(path, func(t *testing.T) {
			params, _ := json.Marshal(FileReadParams{Path: path})
			_, rpcErr := (&FileHandlers{}).Read(context.Background(), params)
			if rpcErr == nil || rpcErr.Code != proto.ErrInvalidRequest {
				t.Fatalf("Read error = %+v, want invalid_request", rpcErr)
			}
		})
	}
}
