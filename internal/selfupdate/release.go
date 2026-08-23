// Package selfupdate periodically checks GitHub Releases for a newer
// Agent build, downloads and verifies it, and re-execs the running
// process into the new binary.
package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const binaryName = "command-relay-agent"

type release struct {
	TagName string  `json:"tag_name"`
	Assets  []asset `json:"assets"`
}

type asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// normalizeVersion strips the leading "v" GitHub release tags use
// (e.g. "v1.2.3") so it can be compared against internal/version.Version,
// which GoReleaser's {{.Version}} embeds without the prefix (e.g. "1.2.3").
func normalizeVersion(tag string) string {
	return strings.TrimPrefix(tag, "v")
}

// updateNeeded reports whether tagName's release differs from the
// currently running version. Callers must not call this while
// currentVersion == "dev" (unreleased build) — every tag would compare
// as newer.
func updateNeeded(currentVersion, tagName string) bool {
	return currentVersion != normalizeVersion(tagName)
}

// archiveAssetName returns the release archive filename .goreleaser.yaml
// produces for goarch, matching archives.name_template
// ("{{ .ProjectName }}_{{ title .Os }}_..." — title-cased OS, amd64
// renamed to x86_64). Only the two goarch values the Agent is built for
// are supported.
func archiveAssetName(goarch string) (string, error) {
	switch goarch {
	case "amd64":
		return binaryName + "_Linux_x86_64.tar.gz", nil
	case "arm64":
		return binaryName + "_Linux_arm64.tar.gz", nil
	default:
		return "", fmt.Errorf("selfupdate: unsupported GOARCH %q", goarch)
	}
}

// selectAsset finds the release asset with the exact given name.
func selectAsset(rel *release, name string) (*asset, error) {
	for i := range rel.Assets {
		if rel.Assets[i].Name == name {
			return &rel.Assets[i], nil
		}
	}
	return nil, fmt.Errorf("selfupdate: release %s has no asset named %q", rel.TagName, name)
}

// selectChecksumsAsset finds the checksums file GoReleaser's default
// checksum pipe attaches to every release, identified by its
// "_checksums.txt" suffix (the exact filename includes the version, so
// it can't be matched literally).
func selectChecksumsAsset(rel *release) (*asset, error) {
	for i := range rel.Assets {
		if strings.HasSuffix(rel.Assets[i].Name, "_checksums.txt") {
			return &rel.Assets[i], nil
		}
	}
	return nil, fmt.Errorf("selfupdate: release %s has no checksums asset", rel.TagName)
}

// fetchLatestRelease calls the GitHub Releases "latest" endpoint at
// apiURL and decodes the response. apiURL is a parameter (rather than a
// package constant) so tests can point it at an httptest.Server.
func fetchLatestRelease(ctx context.Context, client *http.Client, apiURL string) (*release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("selfupdate: GET %s: unexpected status %d", apiURL, resp.StatusCode)
	}

	var rel release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("selfupdate: decode release response: %w", err)
	}
	return &rel, nil
}
