# lattice-runner

Lightweight agent that runs on each worker VM in the Lattice platform. Connects to the central orchestrator via WebSocket, executes container deployments, and reports system metrics. Includes a local web dashboard for at-a-glance status.

---

## Quick Install

The fastest way to set up a worker is from the Lattice dashboard:

1. Go to **Workers** -> **Add Worker**, enter a name and hostname
2. Copy the one-liner shown after creation and run it on the target VM:

```bash
curl -fsSL https://lattice-api.appleby.cloud/install/runner | WORKER_TOKEN=<token> WORKER_NAME=<name> bash
```

This clones the repo, builds the binary, writes the config, installs a systemd service, and starts it.

### Prerequisites

- **Docker** — `curl -fsSL https://get.docker.com | sh`
- **Go 1.24+** — `https://go.dev/dl/`

---

## Interactive Setup

If you prefer to configure step by step:

```bash
git clone https://github.com/aidenappl/lattice-runner.git
cd lattice-runner
go build -o lattice-runner .
sudo ./lattice-runner setup
```

The setup wizard prompts for:

```
  Orchestrator URL [wss://lattice-api.appleby.cloud/ws/worker]:
  Worker Token (from dashboard): <paste>
  Worker Name [hostname]:
  Install as systemd service? [Y/n]:
```

It writes the config to `/opt/lattice-runner/.env`, copies the binary, creates the systemd unit, and starts the service.

---

## Manual Setup

```bash
# Build
go build -o lattice-runner .

# Configure
cat > .env << 'EOF'
ORCHESTRATOR_URL=wss://lattice-api.appleby.cloud/ws/worker
WORKER_TOKEN=your-token
WORKER_NAME=your-worker
EOF

# Run
source .env && ./lattice-runner
```

### Systemd (manual)

```bash
sudo mkdir -p /opt/lattice-runner
sudo cp lattice-runner /opt/lattice-runner/
sudo cp .env /opt/lattice-runner/

# Note: .env for systemd must NOT have `export` — just KEY=value

sudo tee /etc/systemd/system/lattice-runner.service << 'EOF'
[Unit]
Description=Lattice Runner
After=network.target docker.service
Requires=docker.service

[Service]
Type=simple
WorkingDirectory=/opt/lattice-runner
EnvironmentFile=/opt/lattice-runner/.env
ExecStart=/opt/lattice-runner/lattice-runner
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now lattice-runner
```

---

## Environment Variables

| Variable             | Required | Description                                                                          |
| -------------------- | -------- | ------------------------------------------------------------------------------------ |
| `ORCHESTRATOR_URL`   | Yes      | WebSocket URL of the orchestrator (e.g. `wss://lattice-api.appleby.cloud/ws/worker`) |
| `WORKER_TOKEN`       | Yes      | API token generated from the Lattice dashboard                                       |
| `WORKER_NAME`        | No       | Human-readable worker name (defaults to hostname)                                    |
| `HEARTBEAT_INTERVAL` | No       | Metrics reporting interval (default `15s`)                                           |
| `RECONNECT_INTERVAL` | No       | Reconnect backoff on disconnect (default `5s`)                                       |
| `DASHBOARD_PORT`     | No       | Local dashboard port (default `9100`)                                                |

---

## Tech Stack

- **Go 1.24** with gorilla/websocket
- **Docker Engine API** — container lifecycle management
- **WebSocket** — persistent connection to the orchestrator
- **Built-in HTTP server** — local status dashboard

---

## Features

- **One-liner install** — `curl | bash` from the dashboard sets up everything including systemd
- **Interactive setup** — `lattice-runner setup` walks through configuration
- **Auto-reconnect** — Maintains persistent WebSocket connection with backoff
- **Heartbeat** — Reports CPU, memory, disk, network, swap, load average, and container count at configurable intervals
- **Deployment strategies** — Rolling, blue-green, and canary deployments
- **Container lifecycle** — Pull, create, start, stop, restart, remove containers on demand
- **Registry auth** — Supports authenticated image pulls via credentials from the orchestrator
- **Local dashboard** — Web UI showing real-time system metrics and container status

---

## Update

To update an existing runner to the latest version:

```bash
curl -fsSL https://lattice-api.appleby.cloud/install/update.sh | bash
```

---

## Version Check

```bash
lattice-runner version
```

Prints the current version string (e.g. `v0.0.3`). The version is hardcoded in the binary and can be overridden at build time:

```bash
go build -ldflags "-X main.Version=v1.2.3" -o lattice-runner .
```

---

## Local Dashboard

Each runner serves a status dashboard on its configured port:

```
http://<ip>:9100
```

The dashboard shows:

- System info (hostname, OS, arch, Docker version, runner version)
- Real-time CPU, memory, disk, swap, and network metrics
- Load average and process count
- Running containers with state and image info
- Container log viewer

---

## Useful Commands

```bash
sudo systemctl status lattice-runner      # check status
sudo journalctl -u lattice-runner -f      # view logs
sudo systemctl restart lattice-runner     # restart
sudo systemctl stop lattice-runner        # stop
sudo systemctl enable lattice-runner      # enable on boot
sudo systemctl disable lattice-runner     # disable on boot
```
