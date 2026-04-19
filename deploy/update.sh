#!/bin/bash
set -e

# Lattice Runner — Update Script
# Pulls latest source, rebuilds, and restarts the service.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/aidenappl/lattice-runner/main/deploy/update.sh | bash

INSTALL_DIR="/opt/lattice-runner"
REPO="https://github.com/aidenappl/lattice-runner.git"
BUILD_DIR="/tmp/lattice-runner-build"
GO_VERSION="1.24.10"

echo ""
echo "╔══════════════════════════════════════════╗"
echo "║     Lattice Runner — Update              ║"
echo "╚══════════════════════════════════════════╝"
echo ""

# Check binary exists
if [ ! -f "$INSTALL_DIR/lattice-runner" ]; then
    echo "ERROR: $INSTALL_DIR/lattice-runner not found."
    echo "Run the installer first: lattice-runner setup"
    exit 1
fi

# ── Ensure Docker is installed ──────────────────────────────────────────────

# Ensure common paths are available (curl|bash doesn't source profile.d)
for p in /usr/local/go/bin /usr/lib/go/bin /snap/bin "$HOME/go/bin"; do
    [ -d "$p" ] && export PATH="$p:$PATH"
done

if ! command -v docker >/dev/null 2>&1; then
    echo "Docker not found — installing..."
    curl -fsSL https://get.docker.com | sh
    sudo systemctl enable --now docker
    echo "  Docker installed."
    echo ""
fi

# ── Ensure Go is installed ──────────────────────────────────────────────────

if ! command -v go >/dev/null 2>&1; then
    ARCH=$(uname -m)
    case "$ARCH" in
        x86_64|amd64) ARCH="amd64" ;;
        aarch64|arm64) ARCH="arm64" ;;
        *) echo "ERROR: Unsupported architecture: $ARCH"; exit 1 ;;
    esac

    echo "Go not found — installing go${GO_VERSION}..."
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" -o /tmp/go.tar.gz
    sudo rm -rf /usr/local/go
    sudo tar -C /usr/local -xzf /tmp/go.tar.gz
    rm -f /tmp/go.tar.gz
    export PATH="/usr/local/go/bin:$PATH"
    echo "  Go $(go version | awk '{print $3}') installed."
    echo ""
fi

# ───���────────────────────────────────────────────────────────────────────────

echo "Current version: $($INSTALL_DIR/lattice-runner version 2>/dev/null || echo 'unknown')"
echo ""

# Clone and build
echo "Pulling latest source..."
rm -rf "$BUILD_DIR"
git clone --depth=1 "$REPO" "$BUILD_DIR" 2>&1 | tail -1
cd "$BUILD_DIR"

echo "Building..."
GIT_HASH=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")
CGO_ENABLED=0 go build -ldflags="-w -s -X main.Version=v0.1.5-${GIT_HASH}" -o lattice-runner .
echo "  Built: $(ls -lh lattice-runner | awk '{print $5}')"
echo ""

# Stop service
echo "Stopping lattice-runner..."
sudo systemctl stop lattice-runner 2>/dev/null || true

# Replace binary
sudo cp lattice-runner "$INSTALL_DIR/lattice-runner"
sudo chmod +x "$INSTALL_DIR/lattice-runner"

# Start service
echo "Starting lattice-runner..."
sudo systemctl start lattice-runner

# Cleanup
rm -rf "$BUILD_DIR"

echo ""
echo "Update complete."
echo ""
sudo systemctl status lattice-runner --no-pager -l | head -10
echo ""
