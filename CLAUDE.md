# Lattice Runner

Lightweight agent that runs on each worker VM in the Lattice platform. Connects to the central orchestrator (lattice-api) via WebSocket, executes container lifecycle operations and deployments, streams logs, and reports system metrics. Includes a local web dashboard.

## Commands

```bash
dev build    # go build -o bin/app .
dev          # go run .
dev test     # go test ./...
dev fmt      # gofmt -w -s .
dev vet      # go vet ./...
dev check    # fmt + vet + test
dev tidy     # go mod tidy
```

Runner CLI commands:
```bash
lattice-runner           # Start the runner (default)
lattice-runner setup     # Interactive config wizard
lattice-runner version   # Print version string
```

## Project Structure

```
main.go                  # Entry point, command dispatch, WebSocket message handler, heartbeat loop, graceful shutdown
config/config.go         # Config loading from environment (Load, getEnv, getEnvOrPanic)
client/websocket.go      # WebSocket client: auto-reconnect, read/write pumps, message buffering
docker/
  docker.go              # Docker Engine API wrapper: container lifecycle, image pulls, network/volume ops
  logstreamer.go          # Poll-based container log streaming with per-container goroutines
deploy/
  executor.go            # Deployment orchestration: parses spec, creates networks/volumes, delegates to strategy
  rolling.go             # Rolling deployment: sequential pull/stop/remove/create per container
  bluegreen.go           # Blue-green: start green containers, health check, swap ports
  canary.go              # Canary: start canary, monitor 30s, then rolling if healthy
  update.sh              # Runner self-update script (curl from GitHub, build, replace binary)
metrics/collector.go     # System metrics from /proc: CPU, memory, disk, network, containers, uptime
cmd/setup.go             # Interactive setup wizard: prompts, systemd service install (Linux)
web/
  server.go              # Local HTTP dashboard server (default port 9100)
  dashboard.go           # HTML dashboard template: terminal-aesthetic, auto-refreshing metrics
```

## Configuration

Environment variables (loaded in `config/config.go`):

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `ORCHESTRATOR_URL` | Yes | — | WebSocket URL to lattice-api (`wss://...`) |
| `WORKER_TOKEN` | Yes | — | API authentication token |
| `WORKER_NAME` | No | hostname | Human-readable worker name |
| `HEARTBEAT_INTERVAL` | No | `15s` | Metrics reporting interval |
| `RECONNECT_INTERVAL` | No | `5s` | WebSocket reconnect backoff |
| `DASHBOARD_PORT` | No | `9100` | Local dashboard HTTP port |
| `LATTICE_URL` | No | — | Link to orchestrator UI (shown in dashboard) |

## WebSocket Protocol

### Incoming Commands (from lattice-api)

| Command | Behavior |
|---------|----------|
| `connected` | Send registration (hostname, OS, arch, Docker version, IP, runner version) |
| `deploy` | Parse DeploymentSpec, create networks/volumes, execute deployment strategy |
| `start` | Find container by name, start it |
| `stop` | Find container by name, stop with 30s timeout |
| `kill` | Find container by name, send SIGKILL |
| `restart` | Find container by name, restart with 30s timeout |
| `pause` | Pause container execution |
| `unpause` | Resume paused container |
| `remove` | Stop (10s timeout), then force remove |
| `recreate` | Pull new image (if specified), stop, remove, recreate with same config |
| `pull_image` | Pull image from registry with optional auth |
| `reboot_os` | Execute `sudo reboot` |
| `upgrade_runner` | Run upgrade script via curl/bash from orchestrator URL |
| `stop_all` | Stop all running containers (30s timeout each) |
| `start_all` | Start all non-running containers |

### Outgoing Messages (to lattice-api)

`registration`, `heartbeat`, `container_status`, `container_health_status`, `container_sync`, `container_logs`, `deployment_progress`, `lifecycle_log`, `worker_action_status`, `worker_shutdown`, `worker_crash`

## Deployment Strategies

All strategies defined in `deploy/`:

- **Rolling** (`rolling.go`): Sequential per container — pull, stop old, remove, create new
- **Blue-Green** (`bluegreen.go`): Start green containers without ports, wait 5s for health, swap (stop old, recreate with ports)
- **Canary** (`canary.go`): Start canary with `-canary` suffix, monitor every 5s for 30s, if healthy proceed with rolling

DeploymentSpec includes containers, networks, and volumes. Progress reported to orchestrator at each step.

## Metrics Collection

`metrics/collector.go` reads from `/proc` (Linux-only):

- **CPU**: delta-based percentage from `/proc/stat`
- **Memory**: MemTotal, MemAvailable, MemFree, Swap from `/proc/meminfo`
- **Disk**: `syscall.Statfs("/")` for root filesystem
- **Network**: `/proc/net/dev` — sum physical interfaces, excludes `docker*`, `br-*`, `veth*`, `virbr*`, `lo`
- **Containers**: count and running count from Docker API
- **Uptime**: `/proc/uptime`
- **Processes**: from `/proc/loadavg`

## Log Streaming

`docker/logstreamer.go` manages per-container log goroutines:

- Polls for container changes every 10s
- Streams via Docker multiplexed format (8-byte header + payload)
- Extracts RFC3339Nano timestamps for deduplication
- Tails 100 lines on first connect, resumes from last timestamp on reconnect
- Auto-detects and restarts dead streams

## Local Dashboard

HTTP server on port 9100 (`web/`):

- `GET /` — HTML dashboard (terminal aesthetic, dark theme, auto-refresh 5s/10s)
- `GET /api/status` — JSON system metrics
- `GET /api/containers` — JSON container list
- `GET /api/containers/{id}/logs?tail=N` — container logs (default 100 lines)

## Key Patterns

- All incoming commands dispatched to goroutines via `safeGo()` wrapper with panic recovery
- Panics captured and sent to orchestrator as `worker_crash` messages with goroutine name
- `lifecycle_log` messages sent during operations for real-time progress in web UI
- `container_status` sent on completion with action + success/failure
- Heartbeat sends full system metrics + container state snapshots for DB reconciliation
- Container state mapping: Docker `running`->`running`, `paused`->`paused`, `exited`/`dead`->`stopped`, `created`/`restarting`->`pending`
- WebSocket: 60s read deadline, 10s write deadline, 54s ping interval, auto-reconnect with backoff
- Graceful shutdown: sends `worker_shutdown` message, drains send queue for 3s

## Build

```dockerfile
# Multi-stage: golang:1.25-alpine -> alpine:3.19
# Includes: ca-certificates, curl, docker-cli
# Runs as lattice user (UID 1001) in docker group
ARG VERSION=dev
RUN go build -ldflags="-w -s -X main.Version=${VERSION}" -o /lattice-runner .
```

Version set via ldflags: `-X main.Version=<tag>`. `lattice-runner version` prints it.

## Setup / Installation

`lattice-runner setup` runs an interactive wizard:
1. Prompts for orchestrator URL, worker token, worker name
2. On Linux: installs as systemd service at `/opt/lattice-runner/`, writes `.env` (mode 0600)
3. On macOS: writes `.env` in current directory

Remote install: `curl -fsSL https://<api>/install/runner | bash`
