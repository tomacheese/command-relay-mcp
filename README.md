# command-relay-mcp

Run shell commands, manage processes, and read/write files on remote machines through an [MCP](https://modelcontextprotocol.io/) server — so an LLM client (Claude, ChatGPT, etc.) can operate them directly.

## 🏗️ Architecture

- **Agent** — a small process that runs on each machine you want to control. Connects out to the Gateway over WebSocket; never listens for inbound connections itself.
- **Gateway** — a single server that Agents connect to, and that exposes one MCP server (Streamable HTTP) for LLM clients to call. Multiplexes many Agents behind one MCP endpoint.

```
LLM client  --MCP (HTTPS)-->  Gateway  <--WebSocket--  Agent (device A)
                                  ^
                                  '-----WebSocket-----  Agent (device B)
```

## Read-only MCP tools

The Gateway exposes focused inspection tools so an Agent can read common state without running an arbitrary command. Each tool selects a fixed operation on the selected device and is marked read-only in MCP metadata:

- **Git:** `git_status`, `git_diff`, `git_log` read a working tree at a supplied repository path. Status disables optional index locks; diff disables external diff and text conversion.
- **Docker:** `docker_ps`, `docker_inspect`, `docker_logs` list containers, show selected state and image fields, or read a bounded log tail. Container logs are limited to 500 lines and at most the last seven days.
- **systemd:** `systemd_status`, `systemd_units`, `journal_query` inspect the current user's services and unit logs. Journal reads are limited to 500 lines and the last 30 days.
- **tmux:** `tmux_sessions`, `tmux_panes`, `tmux_capture` inspect existing sessions and read a bounded pane history without starting a tmux server.
- **GitHub:** `github_repo`, `github_issue`, `github_pull_request` read repository, issue, and pull request metadata from `api.github.com`. Public resources do not require a token; set `GH_TOKEN` or `GITHUB_TOKEN` in the Agent environment to access private repositories or authenticated API limits.

Outputs are bounded, and the Agent accepts only typed selectors and limits for these tools; it does not accept a shell command or executable path. Tools that need a host utility return an unsupported error if that utility is unavailable. Existing `command_exec` and sandboxed `command_read` remain available for other tasks.

## 🚀 Installation

### Gateway

> ⚠️ **`/mcp` has no authentication.** Anyone who can reach it can run commands on every connected Agent. Do not expose it to an untrusted network without your own reverse-proxy-level access control. `/mcp` (`MCP_LISTEN_ADDRESS`, default `:8080`) and `/agent/ws` (`AGENT_LISTEN_ADDRESS`, default `:8081`) listen on separate ports, so you can expose only the `/mcp` port through a public tunnel while keeping the Agent port scoped to Agents only.

Published to GHCR on every merge to `master`:

```bash
docker run -p 8080:8080 -p 8081:8081 \
  -e AGENT_SHARED_SECRET=secret \
  ghcr.io/tomacheese/command-relay-mcp:latest
```

See the [Releases](../../releases) page for versioned tags.

### Agent

Download the `command-relay-agent` binary for your architecture from the [Releases](../../releases) page (Linux only — Landlock-based sandboxing for `command_read` is Linux-specific), then run it as a **user** `systemd` service using [`deploy/systemd/command-relay-agent.service`](deploy/systemd/command-relay-agent.service) — the sandbox creates its own unprivileged user/network namespaces at runtime, so no elevated systemd identity (`DynamicUser`, root) is needed:

```bash
sudo cp command-relay-agent /usr/local/bin/
mkdir -p ~/.config/systemd/user
cp deploy/systemd/command-relay-agent.service ~/.config/systemd/user/
systemctl --user edit command-relay-agent  # set DEVICE_ID / DEVICE_SECRET / GATEWAY_URL
loginctl enable-linger "$USER"              # keep it running after logout
systemctl --user enable --now command-relay-agent
```

自動更新(デフォルトで有効。`AUTO_UPDATE_ENABLED=false` で無効化できる)を使う場合、Agent は起動中バイナリ自身のファイルを新しいバイナリで置き換える。上記の `sudo cp ... /usr/local/bin/` のように root 所有・非特権ユーザー書き込み不可のパスに配置していると、この置き換えは失敗しログに記録されるだけで自動更新は機能しない。自動更新を使うなら、バイナリは Agent を実行するユーザー自身が書き込み可能な場所(例: `~/.local/bin/command-relay-agent`)に配置し、`ExecStart` をそのパスに合わせて `systemctl --user edit` で上書きすること。

## 🔧 Development

```bash
go test ./...
go build ./...
docker build -t command-relay-mcp .
```

## 📄 License

[MIT](LICENSE)
