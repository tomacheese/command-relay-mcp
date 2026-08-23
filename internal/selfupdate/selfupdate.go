package selfupdate

import (
	"context"
	"log"
	"net/http"
	"time"

	"command-relay-mcp/internal/version"
)

const releasesAPIURL = "https://api.github.com/repos/tomacheese/command-relay-mcp/releases/latest"

// defaultInterval is used in place of a non-positive Options.Interval —
// time.NewTicker panics on Interval <= 0, which would crash the whole
// Agent process since this runs in an unrecovered goroutine.
const defaultInterval = 6 * time.Hour

// httpTimeout bounds every GitHub API/download request. Without it, a
// stalled response hangs the check goroutine forever, silently skipping
// every future scheduled check.
const httpTimeout = 30 * time.Second

// Options configures the self-update checker.
type Options struct {
	// Enabled turns the checker on. When false, Start is a no-op.
	Enabled bool
	// Interval between checks. Ignored when Enabled is false.
	Interval time.Duration
	// CurrentVersion is the running binary's version (internal/version.Version).
	// While it equals "dev" (unreleased build), Start is a no-op — every
	// release tag would otherwise compare as newer.
	CurrentVersion string
	// HTTPClient is used for all GitHub/download requests. Defaults to
	// http.DefaultClient when nil.
	HTTPClient *http.Client
	// ReleasesAPIURL overrides the GitHub Releases endpoint. Defaults to
	// the real API; tests point it at an httptest.Server.
	ReleasesAPIURL string
}

// Start launches the self-update check loop in the background, checking
// once immediately and then every Interval until ctx is done. It returns
// immediately; the loop (if any) runs in its own goroutine.
func Start(ctx context.Context, opts Options) {
	if !opts.Enabled || opts.CurrentVersion == version.DevVersion {
		return
	}
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: httpTimeout}
	}
	apiURL := opts.ReleasesAPIURL
	if apiURL == "" {
		apiURL = releasesAPIURL
	}
	interval := opts.Interval
	if interval <= 0 {
		log.Printf("selfupdate: invalid interval %s, falling back to %s", interval, defaultInterval)
		interval = defaultInterval
	}

	go func() {
		runCheck := func() {
			if err := checkOnce(ctx, client, apiURL, opts.CurrentVersion); err != nil {
				log.Printf("selfupdate: check failed: %v", err)
			}
		}
		runCheck()

		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				runCheck()
			}
		}
	}()
}

// checkOnce fetches the latest release and applies it if it differs
// from currentVersion. Any failure (network, verification, or apply) is
// returned to the caller to log — never fatal, always deferred to the
// next scheduled check.
func checkOnce(ctx context.Context, client *http.Client, apiURL, currentVersion string) error {
	rel, err := fetchLatestRelease(ctx, client, apiURL)
	if err != nil {
		return err
	}
	if !updateNeeded(currentVersion, rel.TagName) {
		return nil
	}
	return applyUpdate(ctx, client, rel)
}
