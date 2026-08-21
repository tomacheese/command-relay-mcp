// Package version holds the build-time version string shared by the
// Gateway and the Agent.
package version

// Version is overridden at build time via
// -ldflags "-X command-relay-mcp/internal/version.Version=...": the
// Dockerfile does this via its APPLICATION_VERSION build arg (set by
// reusable-docker.yml), and .goreleaser.yaml does it for the Agent
// binary release. Left at this default for `go run`/`go test`/an
// unreleased build.
var Version = "dev"
