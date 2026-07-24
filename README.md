# lattice-runner

Worker-agent daemon for the Lattice container orchestration platform. Runs on each worker VM, connects outbound to the orchestrator over WebSocket, and manages the local Docker daemon.

> **Lattice platform** · Go worker daemon · one per worker VM · `wss://lattice-api.appleby.cloud/ws/worker`

---

## Overview

`lattice-runner` is the **data plane** of Lattice. One instance runs on every worker machine. On startup it connects to the local Docker Engine API, then dials **outbound** to [`lattice-api`](https://github.com/aidenappl/lattice-api) over a single persistent WebSocket (authenticated with a worker token) and reconnects forever with backoff. From there it:

- Executes **deployments** (rolling, blue-green, canary strategies) with health-gating and post-deploy verification
- Runs **container lifecycle** actions on demand — start, stop, restart, kill, pause, unpause, remove, recreate, pull image
- Manages **database containers** and runs scheduled/on-demand **snapshots** to remote backup destinations (S3, Google Drive, Samba)
- Streams **container logs** and reports **host + container metrics** via a heartbeat
- Serves a small read-only **local status dashboard**

Because the connection is outbound-only, a worker needs **no inbound firewall rules** — only outbound access to the orchestrator. It holds no state and makes no scheduling decisions; every instruction arrives as a message from the control plane.

## Role in the Lattice ecosystem

| Repo | Relationship |
|------|--------------|
| [`lattice-api`](https://github.com/aidenappl/lattice-api) | **The control plane.** Hosts the `/ws/worker` endpoint this runner dials, issues every command, receives all telemetry, mints worker tokens, and serves the install/upgrade scripts. |
| [`lattice-web`](https://github.com/aidenappl/lattice-web) | Next.js dashboard over `lattice-api` — where operators add workers and trigger deploys/actions. |
| [`lattice-mcp`](https://github.com/aidenappl/lattice-mcp) | MCP server exposing the `lattice-api` admin surface to Claude Code as typed tools. |

This runner depends only on the Docker daemon on its host and the WebSocket to `lattice-api`.

## Tech stack

- **Go 1.25** (see `go.mod`)
- **gorilla/websocket** — the persistent outbound connection to the orchestrator
- **Docker Engine API** (`docker/docker` SDK) — container/image/network/volume/exec management
- **AWS SDK v2 · Google API + OAuth2 · `smbclient`** — S3, Google Drive, and Samba backup destinations
- **`net/http`** — the local status dashboard (no framework)

## Getting started

### Prerequisites

- **Docker** — `curl -fsSL https://get.docker.com | sh`
- **Go 1.25+** — https://go.dev/dl/ (only needed to build from source)
- A **worker token** from the Lattice dashboard (*Workers → Add Worker*)

### Setup

The fastest path is the one-liner from the dashboard, which builds the binary, writes the config, installs a `systemd` service, and starts it:

```bash
curl -fsSL https://lattice-api.appleby.cloud/install/runner | WORKER_TOKEN=<token> WORKER_NAME=<name> bash
```

To configure interactively instead:

```bash
git clone https://github.com/aidenappl/lattice-runner.git
cd lattice-runner
go build -o lattice-runner .
sudo ./lattice-runner setup    # prompts for URL/token/name, installs systemd unit on Linux
```

Or run manually with environment variables:

```bash
export ORCHESTRATOR_URL=wss://lattice-api.appleby.cloud/ws/worker
export WORKER_TOKEN=<token>
export WORKER_NAME=<name>
./lattice-runner
```

#### Configuration

Loaded in `config/config.go`. Required variables cause a startup panic if missing.

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `ORCHESTRATOR_URL` | **Yes** | — | WebSocket URL to `lattice-api` (`wss://…/ws/worker`). `ws://` is rejected unless `ALLOW_INSECURE=true`. |
| `WORKER_TOKEN` | **Yes** | — | Worker auth token; sent as `?token=` on the WS handshake. |
| `WORKER_NAME` | No | hostname | Human-readable worker name in registration. |
| `HEARTBEAT_INTERVAL` | No | `10s` | Metrics/heartbeat cadence. |
| `RECONNECT_INTERVAL` | No | `5s` | WebSocket reconnect backoff. |
| `DASHBOARD_PORT` | No | `9100` | Local dashboard HTTP port. |
| `DASHBOARD_BIND` | No | `127.0.0.1` | Dashboard bind address (localhost-only by default). |
| `LATTICE_URL` | No | — | Link to the orchestrator UI, shown on the dashboard. |
| `ALLOW_INSECURE` | No | `false` | Permit an unencrypted `ws://` URL (local dev only). |

## Development

Uses the standard `dev` CLI (`Devfile.yaml`):

| Command | What it does |
|---------|--------------|
| `dev build` | `go build -o bin/app .` |
| `dev` | `go run .` (needs `ORCHESTRATOR_URL` + `WORKER_TOKEN`) |
| `dev test` | `go test ./...` |
| `dev fmt` | `gofmt -w -s .` |
| `dev vet` | `go vet ./...` |
| `dev check` | fmt + vet + test |
| `dev tidy` | `go mod tidy` |

The binary itself has three modes: `lattice-runner` (start the daemon), `lattice-runner setup` (interactive install wizard), and `lattice-runner version` (print the build version). The version is injected at build time via `-ldflags "-X main.Version=<tag>"`.

## Project structure

```
main.go              # Entrypoint + the message-handler switch, heartbeat loop, graceful shutdown
validate.go          # validContainerName — allow-list guard for orchestrator-supplied names
config/config.go     # Config loading from env; enforces wss:// unless ALLOW_INSECURE
client/websocket.go  # WebSocket client: auto-reconnect, read/write pumps, ping/pong, send buffer
cmd/setup.go         # Interactive setup wizard + systemd install
deploy/              # Deployment executor + rolling / blue-green / canary strategies, spec validation
docker/              # Docker Engine API wrapper, log streamer, network monitor, database helpers
metrics/collector.go # Host + runner metrics from /proc and syscall.Statfs
scheduler/           # Cron-style snapshot scheduler (5-field matcher, per-instance dedupe)
backup/              # Snapshot destinations: S3, Google Drive, Samba
web/                 # Local read-only status dashboard (HTTP)
```

## Deployment

Workers run the binary directly on the host (under `systemd`), **not** as a container — the process needs access to `/var/run/docker.sock` to manage sibling containers. The install one-liner and `lattice-runner setup` create the `systemd` unit (`Restart=always`), write config to `/opt/lattice-runner/.env` (mode 0600), and start the service.

Updates are driven by the control plane: an `upgrade_runner` message tells the runner to download the install script, verify its SHA-256, run it, and let `systemd` restart the new binary (surfaced in the Lattice MCP as `lattice_upgrade_worker`). Manual update:

```bash
curl -fsSL https://lattice-api.appleby.cloud/install/update.sh | bash
```

Useful host commands:

```bash
sudo systemctl status lattice-runner      # check status
sudo journalctl -u lattice-runner -f      # view logs
sudo systemctl restart lattice-runner     # restart
```

The local dashboard is served at `http://127.0.0.1:9100` (system info, live metrics, container list, and log viewer).

## Contributing & further reading

- **[AGENTS.md](./AGENTS.md)** — the authoritative deep reference: the full WebSocket message protocol (every inbound/outbound message type), deploy strategy internals, the recreate canonical-name fallback, Docker interaction model, config surface, operations, and guardrails. Read it before making changes.
- Related repos: [`lattice-api`](https://github.com/aidenappl/lattice-api) · [`lattice-web`](https://github.com/aidenappl/lattice-web) · [`lattice-mcp`](https://github.com/aidenappl/lattice-mcp)
