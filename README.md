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

Download the `command-relay-agent` binary for your architecture from the [Releases](../../releases) page (Linux only — Landlock-based sandboxing for `command_read` is Linux-specific), then run it as a **user** `systemd` service using [`deploy/systemd/command-relay-agent.service`](deploy/systemd/command-relay-agent.service) — the sandbox creates its own unprivileged user/mount/PID namespaces at runtime, so no elevated systemd identity (`DynamicUser`, root) is needed:

```bash
sudo cp command-relay-agent /usr/local/bin/
mkdir -p ~/.config/systemd/user
cp deploy/systemd/command-relay-agent.service ~/.config/systemd/user/
systemctl --user edit command-relay-agent  # set DEVICE_ID / DEVICE_SECRET / GATEWAY_URL
loginctl enable-linger "$USER"              # keep it running after logout
systemctl --user enable --now command-relay-agent
```

On Ubuntu 24.04+ (and other distros with `kernel.apparmor_restrict_unprivileged_userns=1`), AppArmor blocks unprivileged mount-namespace creation by default, which `command_read`'s sandbox needs. Install the accompanying profile once, as root:

```bash
sudo cp deploy/apparmor/usr.local.bin.command-relay-agent /etc/apparmor.d/
sudo apparmor_parser -r /etc/apparmor.d/usr.local.bin.command-relay-agent
```

## 🔧 Development

```bash
go test ./...
go build ./...
docker build -t command-relay-mcp .
```

## 📄 License

[MIT](LICENSE)
