# AGENTS.md

## Language

- Write all deliverables in English: commit messages, PR/Issue titles and bodies, review comments, code comments and doc comments, and documents (README, this file).
- Exceptions: quotations, technical identifiers, generated files, external source text, and user-facing strings whose wording is part of a contract (for example the Japanese fork-PR approval comment in `.github/workflows/release.yml`, which a `body-includes` match depends on).
- Commits follow Conventional Commits; branches follow Conventional Branch (`feat/...`, `fix/...`, `docs/...`).

## Overview

`command-relay-mcp` lets an MCP client run commands, manage processes, and read/write files on remote machines.

- **Agent** (`agent/`, entry `agent/cmd`): runs on each controlled machine, connects out to the Gateway over WebSocket, never listens for inbound connections. Linux only (Landlock sandbox).
- **Gateway** (`gateway/`, entry `gateway/cmd`): accepts Agent connections and exposes one MCP server (Streamable HTTP at `/mcp`).
- `internal/proto`: JSON-RPC style messages shared by both sides. `internal/backend`: process backends (plain, and Landlock sandbox for `command_read`). `internal/selfupdate`: Agent auto-update from GitHub Releases. `internal/version`: version string injected via `-ldflags`.
- `e2e/`: end-to-end tests that run a Gateway and a real Agent in-process. `deploy/systemd/`: user-unit for the Agent.

## Development commands

Go version comes from `go.mod`. There is no Makefile.

```bash
go build ./...
go vet ./...
go test ./...          # CI runs `go test -v ./...`
gofmt -l .             # CI runs `go fmt ./...` and fails on any diff
docker build -t command-relay-mcp .   # Gateway image only
```

CI (`.github/workflows/build.yml`) also runs `systemd-analyze verify --user` on `deploy/systemd/command-relay-agent.service`. Releases: the Gateway image goes to GHCR and the Agent binary (linux amd64/arm64) is released with GoReleaser (`.goreleaser.yaml`), both under one version tag (`.github/workflows/release.yml`). Do not commit build output; `dist/` is ignored, so build binaries outside the repo or with `-o /tmp/...`.

## Configuration

Environment variables, read in `gateway/config.go` and `agent/config.go`.

- Gateway: `MCP_LISTEN_ADDRESS` (default `:8080`), `AGENT_LISTEN_ADDRESS` (default `:8081`), `AGENT_SHARED_SECRET`.
- Agent: `DEVICE_ID`, `DEVICE_SECRET`, `GATEWAY_URL` (required); `DEFAULT_SHELL`, `STDOUT_BUFFER_BYTES`, `STDERR_BUFFER_BYTES`, `FINISHED_PROCESS_TTL`, `HISTORY_RETENTION`, `AUTO_UPDATE_ENABLED`, `AUTO_UPDATE_INTERVAL` (defaults in `agent/config.go`); `HISTORY_DB_PATH` (default `agent-history.db`, read in `agent/cmd/main.go`; set in the systemd unit).

## Conventions

- Tests sit next to the code as `*_test.go`; cross-component behavior goes in `e2e/`.
- Comments explain intent or constraints that the code cannot show; do not restate what the code does. Keep comments in sync when behavior changes.
- Execution history stores metadata only; stdout/stderr are never persisted.

## Security-relevant facts (as enforced by the code)

- `/mcp` has **no authentication**. Anyone who can reach it can run commands on every connected Agent. Protect it with network or reverse-proxy access control; it listens on a separate port from `/agent/ws` so only the MCP port needs to be exposed.
- Agents authenticate to the Gateway with a shared secret, compared in constant time; an empty `AGENT_SHARED_SECRET` rejects every Agent.
- `command_exec` and the process/file tools are unrestricted: file paths are cleaned (`..` resolved) but not fenced to any directory, and the Agent runs with its own user's permissions. Do not describe path validation as an access-control boundary.
- `command_read` runs inside a Landlock sandbox (read-only filesystem, write only to a per-invocation scratch dir and `/dev/null`) in new user and network namespaces. If the sandbox is not available the call returns an unsupported error, and a sandbox setup failure is reported as `sandbox_violation`; it never falls back to running unsandboxed.
- The git/docker/systemd/tmux/GitHub read-only tools accept typed selectors and bounded limits only, never a shell command or executable path.
- Agent self-update verifies the release archive against the release's `*_checksums.txt` asset (matched by that suffix, SHA-256) before replacing the binary.
- Never commit secrets (`DEVICE_SECRET`, `AGENT_SHARED_SECRET`, tokens) or log their values.
