package selfupdate

import (
	"context"
	"log"
	"net/http"
	"time"
)

const releasesAPIURL = "https://api.github.com/repos/tomacheese/command-relay-mcp/releases/latest"

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
	if !opts.Enabled || opts.CurrentVersion == "dev" {
		return
	}
	client := opts.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	apiURL := opts.ReleasesAPIURL
	if apiURL == "" {
		apiURL = releasesAPIURL
	}

	go func() {
		runCheck := func() {
			if err := checkOnce(ctx, client, apiURL, opts.CurrentVersion); err != nil {
				log.Printf("selfupdate: check failed: %v", err)
			}
		}
		runCheck()

		t := time.NewTicker(opts.Interval)
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
