package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func buildTarGz(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		hdr := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(content))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader: %v", err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar Close: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip Close: %v", err)
	}
	return buf.Bytes()
}

func TestExtractBinary(t *testing.T) {
	archive := buildTarGz(t, map[string][]byte{
		"command-relay-agent": []byte("new binary contents"),
		"README.md":           []byte("ignore me"),
	})

	data, err := extractBinary(archive, "command-relay-agent")
	if err != nil {
		t.Fatalf("extractBinary: %v", err)
	}
	if string(data) != "new binary contents" {
		t.Errorf("extractBinary data = %q", data)
	}
}

func TestExtractBinary_NotFound(t *testing.T) {
	archive := buildTarGz(t, map[string][]byte{"README.md": []byte("x")})
	if _, err := extractBinary(archive, "command-relay-agent"); err == nil {
		t.Error("extractBinary: want error when binary missing from archive, got nil")
	}
}

func TestReplaceBinary(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "command-relay-agent")
	if err := os.WriteFile(dest, []byte("old contents"), 0o755); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	if err := replaceBinary(dest, []byte("new contents")); err != nil {
		t.Fatalf("replaceBinary: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "new contents" {
		t.Errorf("dest contents = %q, want %q", got, "new contents")
	}

	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("dest mode = %v, want executable", info.Mode())
	}

	// No leftover temp file in the same directory.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("dir entries = %v, want exactly the replaced binary", entries)
	}
}

func TestDownloadAsset_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := downloadAsset(context.Background(), srv.Client(), srv.URL); err == nil {
		t.Error("downloadAsset: want error for non-200 status, got nil")
	}
}
