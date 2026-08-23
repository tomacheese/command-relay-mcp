package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestCheckOnce_AppliesUpdate(t *testing.T) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skipf("unsupported GOARCH %s for this test", runtime.GOARCH)
	}
	archiveName, err := archiveAssetName(runtime.GOARCH)
	if err != nil {
		t.Fatalf("archiveAssetName: %v", err)
	}

	archiveData := buildTarGz(t, map[string][]byte{binaryName: []byte("new binary contents")})
	checksumsData := checksumsFileFor(t, archiveData, archiveName)

	mux := http.NewServeMux()
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"tag_name":"v9.9.9","assets":[` +
			`{"name":"` + archiveName + `","browser_download_url":"http://` + r.Host + `/archive"},` +
			`{"name":"command-relay-agent_9.9.9_checksums.txt","browser_download_url":"http://` + r.Host + `/checksums"}` +
			`]}`))
	})
	mux.HandleFunc("/archive", func(w http.ResponseWriter, r *http.Request) { w.Write(archiveData) })
	mux.HandleFunc("/checksums", func(w http.ResponseWriter, r *http.Request) { w.Write(checksumsData) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, binaryName)
	if err := os.WriteFile(dest, []byte("old binary contents"), 0o755); err != nil {
		t.Fatalf("seed dest: %v", err)
	}

	origExecutable := executablePath
	executablePath = func() (string, error) { return dest, nil }
	defer func() { executablePath = origExecutable }()

	var execCalled bool
	origExec := execSelf
	execSelf = func(argv0 string, argv, envv []string) error {
		execCalled = true
		if argv0 != dest {
			t.Errorf("execSelf argv0 = %q, want %q", argv0, dest)
		}
		return nil
	}
	defer func() { execSelf = origExec }()

	if err := checkOnce(context.Background(), srv.Client(), srv.URL+"/latest", "1.0.0"); err != nil {
		t.Fatalf("checkOnce: %v", err)
	}

	if !execCalled {
		t.Error("checkOnce: execSelf was not called")
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "new binary contents" {
		t.Errorf("dest contents = %q, want %q", got, "new binary contents")
	}
}

func TestCheckOnce_NoUpdateNeeded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"tag_name":"v1.0.0","assets":[]}`))
	}))
	defer srv.Close()

	var execCalled bool
	origExec := execSelf
	execSelf = func(argv0 string, argv, envv []string) error { execCalled = true; return nil }
	defer func() { execSelf = origExec }()

	if err := checkOnce(context.Background(), srv.Client(), srv.URL, "1.0.0"); err != nil {
		t.Fatalf("checkOnce: %v", err)
	}
	if execCalled {
		t.Error("checkOnce: execSelf was called even though no update was needed")
	}
}

func TestStart_NoOpWhenDisabledOrDev(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("Start: HTTP request made despite Enabled=false or CurrentVersion=dev")
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	Start(ctx, Options{Enabled: false, CurrentVersion: "1.0.0", Interval: time.Millisecond, ReleasesAPIURL: srv.URL})
	Start(ctx, Options{Enabled: true, CurrentVersion: "dev", Interval: time.Millisecond, ReleasesAPIURL: srv.URL})

	time.Sleep(20 * time.Millisecond)
}

// checksumsFileFor builds a GoReleaser-style checksums.txt entry for
// archiveData under the given asset name.
func checksumsFileFor(t *testing.T, archiveData []byte, assetName string) []byte {
	t.Helper()
	sum := sha256.Sum256(archiveData)
	return []byte(hex.EncodeToString(sum[:]) + "  " + assetName + "\n")
}
