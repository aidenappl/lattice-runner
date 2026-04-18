# lattice-runner

Lightweight agent that runs on each worker VM in the Lattice platform. Connects to the central orchestrator via WebSocket, executes container deployments, and reports system metrics.

---

## Tech Stack

- **Go 1.24** with gorilla/websocket
- **Docker Engine API** — container lifecycle management
- **WebSocket** — persistent connection to the orchestrator

---

## Environment Variables

| Variable | Required | Description |
|---|---|---|
| `ORCHESTRATOR_URL` | Yes | WebSocket URL of the orchestrator (e.g. `wss://lattice-api.appleby.cloud/ws/worker`) |
| `WORKER_TOKEN` | Yes | API token generated from the Lattice web UI |
| `WORKER_NAME` | No | Human-readable worker name (defaults to hostname) |
| `HEARTBEAT_INTERVAL` | No | Metrics reporting interval (default `15s`) |
| `RECONNECT_INTERVAL` | No | Reconnect backoff on disconnect (default `5s`) |

---

## Features

- **Auto-reconnect** — Maintains persistent WebSocket connection with exponential backoff
- **Heartbeat** — Reports CPU, memory, disk, network, and container count at configurable intervals
- **Deployment strategies** — Rolling, blue-green, and canary deployments
- **Container lifecycle** — Pull, create, start, stop, restart, remove containers on demand
- **Registry auth** — Supports authenticated image pulls via credentials from the orchestrator

---

## Quick Start

```bash
# Copy env and configure
cp .env.example .env

# Run (requires Docker)
go run .
```

The runner requires Docker to be running and accessible. In production, mount the Docker socket:

```bash
docker run -v /var/run/docker.sock:/var/run/docker.sock \
  -e ORCHESTRATOR_URL=wss://lattice-api.appleby.cloud/ws/worker \
  -e WORKER_TOKEN=your-token \
  lattice-runner
```
