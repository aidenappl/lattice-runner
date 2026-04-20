#!/bin/bash
set -e

# Lattice Runner - Update Script
# Fetches the latest GitHub release, rebuilds from that tag, and restarts the service.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/aidenappl/lattice-runner/main/deploy/update.sh | bash

INSTALL_DIR="/opt/lattice-runner"
REPO="aidenappl/lattice-runner"
BUILD_DIR="/tmp/lattice-runner-build"
GO_VERSION="1.25.0"

echo ""
echo "Lattice Runner - Update"
echo ""

# Check binary exists
if [ ! -f "$INSTALL_DIR/lattice-runner" ]; then
    echo "ERROR: $INSTALL_DIR/lattice-runner not found."
    echo "Run the installer first: curl -fsSL https://lattice-api.appleby.cloud/install/runner | bash"
    exit 1
fi

# Ensure common paths are available (curl|bash does not source profile.d)
for p in /usr/local/go/bin /usr/lib/go/bin /snap/bin "$HOME/go/bin"; do
    [ -d "$p" ] && export PATH="$p:$PATH"
done

# Ensure Docker is installed
if ! command -v docker >/dev/null 2>&1; then
    echo "Docker not found - installing..."
    curl -fsSL https://get.docker.com | sh
    sudo systemctl enable --now docker
    echo "  Docker installed."
    echo ""
fi

# Ensure Go is installed
if ! command -v go >/dev/null 2>&1; then
    ARCH=$(uname -m)
    case "$ARCH" in
        x86_64|amd64) ARCH="amd64" ;;
        aarch64|arm64) ARCH="arm64" ;;
        *) echo "ERROR: Unsupported architecture: $ARCH"; exit 1 ;;
    esac

    echo "Go not found - installing go${GO_VERSION}..."
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" -o /tmp/go.tar.gz
    sudo rm -rf /usr/local/go
    sudo tar -C /usr/local -xzf /tmp/go.tar.gz
    rm -f /tmp/go.tar.gz
    export PATH="/usr/local/go/bin:$PATH"
    echo "  Go $(go version | awk '{print $3}') installed."
    echo ""
fi

# Resolve latest release tag
echo "Current version: $($INSTALL_DIR/lattice-runner version 2>/dev/null || echo 'unknown')"

LATEST_TAG=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | head -1 | cut -d'"' -f4)
if [ -z "$LATEST_TAG" ]; then
    echo "ERROR: Could not determine latest release tag from GitHub."
    exit 1
fi

echo "Latest version:  ${LATEST_TAG}"
echo ""

# Clone tag and build
echo "Pulling source for ${LATEST_TAG}..."
TMP_BUILD=$(mktemp -d)
export GOPATH="${TMP_BUILD}/gopath"
export GOMODCACHE="${TMP_BUILD}/gomodcache"
export GOCACHE="${TMP_BUILD}/gocache"
mkdir -p "$GOPATH" "$GOMODCACHE" "$GOCACHE"
git clone --depth=1 --branch "${LATEST_TAG}" "https://github.com/${REPO}.git" "${TMP_BUILD}/src" 2>&1 | tail -1
cd "${TMP_BUILD}/src"

echo "Building..."
CGO_ENABLED=0 go build -ldflags="-w -s -X main.Version=${LATEST_TAG}" -o lattice-runner .
echo "  Built: $(ls -lh lattice-runner | awk '{print $5}')"
echo ""

# Replace binary atomically — avoids "Text file busy" when upgrading a running binary
sudo cp lattice-runner "$INSTALL_DIR/lattice-runner.new"
sudo chmod +x "$INSTALL_DIR/lattice-runner.new"
sudo mv -f "$INSTALL_DIR/lattice-runner.new" "$INSTALL_DIR/lattice-runner"

# Cleanup
rm -rf "$TMP_BUILD"

# Ensure systemd service exists
SERVICE_FILE="/etc/systemd/system/lattice-runner.service"
if [ ! -f "$SERVICE_FILE" ]; then
    echo "Creating systemd service..."
    sudo tee "$SERVICE_FILE" > /dev/null <<'EOF'
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
    sudo systemctl enable lattice-runner
    echo "  Created and enabled lattice-runner.service"
fi

# Delay restart so the runner process that spawned this script can finish
# reporting success before systemd kills it.
echo "Scheduling lattice-runner restart in 3 seconds..."
(sleep 3 && sudo systemctl restart lattice-runner) &

echo ""
echo "Update complete - now running ${LATEST_TAG}."
echo ""
