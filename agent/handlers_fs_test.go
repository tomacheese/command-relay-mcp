package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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
