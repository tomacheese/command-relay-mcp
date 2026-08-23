package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
)

// maxAssetBytes bounds every downloaded/decompressed self-update asset
// (release archive, checksums.txt, extracted binary). A generous ceiling
// for one Go binary, chosen to reject a decompression bomb or a stalled/
// oversized response before it can exhaust memory.
const maxAssetBytes = 100 << 20 // 100MiB

// extractBinary reads a gzip-compressed tar archive and returns the
// contents of the regular file named name at its root (GoReleaser's
// archives.wrap_in_directory defaults to false, so the binary sits at
// the archive root).
func extractBinary(tarGzData []byte, name string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(tarGzData))
	if err != nil {
		return nil, fmt.Errorf("selfupdate: open gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("selfupdate: read tar: %w", err)
		}
		if hdr.Typeflag == tar.TypeReg && filepath.Base(hdr.Name) == name {
			if hdr.Size > maxAssetBytes {
				return nil, fmt.Errorf("selfupdate: %q in archive exceeds %d byte limit", name, maxAssetBytes)
			}
			return io.ReadAll(io.LimitReader(tr, maxAssetBytes+1))
		}
	}
	return nil, fmt.Errorf("selfupdate: %q not found in archive", name)
}

// replaceBinary atomically overwrites destPath with data: it writes a
// temp file in destPath's own directory (so the following rename stays
// on the same filesystem), makes it executable, then renames it over
// destPath. A process already running destPath keeps its old inode open
// and is unaffected until it re-execs.
func replaceBinary(destPath string, data []byte) error {
	dir := filepath.Dir(destPath)
	tmp, err := os.CreateTemp(dir, ".command-relay-agent-update-*")
	if err != nil {
		return fmt.Errorf("selfupdate: create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("selfupdate: write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("selfupdate: close temp file: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return fmt.Errorf("selfupdate: chmod temp file: %w", err)
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		return fmt.Errorf("selfupdate: rename into place: %w", err)
	}
	return nil
}

// executablePath resolves the path of the running binary. It is a
// package variable (wrapping os.Executable) so tests can point
// checkOnce at a scratch file instead of the real test binary.
var executablePath = os.Executable

// execSelf replaces the current process image with argv0, keeping the
// same PID and cgroup — this is what lets the Agent "restart" after a
// self-update without involving systemd's Restart= policy. It is a
// package variable so tests can observe the call without actually
// exec'ing (a successful syscall.Exec never returns).
var execSelf = func(argv0 string, argv, envv []string) error {
	return syscall.Exec(argv0, argv, envv)
}

func downloadAsset(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("selfupdate: GET %s: unexpected status %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAssetBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxAssetBytes {
		return nil, fmt.Errorf("selfupdate: GET %s: response exceeds %d byte limit", url, maxAssetBytes)
	}
	return data, nil
}

func applyUpdate(ctx context.Context, client *http.Client, rel *release) error {
	archiveName, err := archiveAssetName(runtime.GOARCH)
	if err != nil {
		return err
	}
	archiveAsset, err := selectAsset(rel, archiveName)
	if err != nil {
		return err
	}
	checksumsAsset, err := selectChecksumsAsset(rel)
	if err != nil {
		return err
	}

	archiveData, err := downloadAsset(ctx, client, archiveAsset.BrowserDownloadURL)
	if err != nil {
		return fmt.Errorf("selfupdate: download %q: %w", archiveAsset.Name, err)
	}
	checksumsData, err := downloadAsset(ctx, client, checksumsAsset.BrowserDownloadURL)
	if err != nil {
		return fmt.Errorf("selfupdate: download %q: %w", checksumsAsset.Name, err)
	}

	if err := verifySHA256(archiveData, string(checksumsData), archiveAsset.Name); err != nil {
		return err
	}

	binData, err := extractBinary(archiveData, binaryName)
	if err != nil {
		return err
	}

	destPath, err := executablePath()
	if err != nil {
		return fmt.Errorf("selfupdate: resolve running binary path: %w", err)
	}
	if err := replaceBinary(destPath, binData); err != nil {
		return err
	}

	// If execSelf fails, destPath now holds the new binary but this
	// process's in-memory CurrentVersion is unchanged, so the next check
	// (Interval from now) will see the same update again and retry — this
	// matches the package's log-and-defer error policy for every other
	// applyUpdate failure; automatic backoff/rollback here is an explicit
	// non-goal.
	if err := execSelf(destPath, os.Args, os.Environ()); err != nil {
		return fmt.Errorf("selfupdate: re-exec %q: %w", destPath, err)
	}
	return nil
}
