#!/bin/bash
set -euo pipefail

# Lattice Runner - Update Script
# Fetches the latest GitHub release, rebuilds from that tag, and restarts the service.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/aidenappl/lattice-runner/main/deploy/update.sh | bash

INSTALL_DIR="/opt/lattice-runner"
REPO="aidenappl/lattice-runner"
GO_VERSION="1.25.0"

# Secure cleanup on exit
BUILD_DIR=""
cleanup() {
    if [ -n "$BUILD_DIR" ] && [ -d "$BUILD_DIR" ]; then
        rm -rf "$BUILD_DIR"
    fi
}
trap cleanup EXIT

log() { echo "  $*"; }
err() { echo "ERROR: $*" >&2; exit 1; }

echo ""
echo "Lattice Runner - Update"
echo ""

# Check binary exists
if [ ! -f "$INSTALL_DIR/lattice-runner" ]; then
    err "$INSTALL_DIR/lattice-runner not found.\nRun the installer first: curl -fsSL https://lattice-api.appleby.cloud/install/runner | bash"
fi

# Ensure common paths are available (curl|bash does not source profile.d)
for p in /usr/local/go/bin /usr/lib/go/bin /snap/bin "$HOME/go/bin"; do
    [ -d "$p" ] && export PATH="$p:$PATH"
done

# Ensure Docker is installed
if ! command -v docker >/dev/null 2>&1; then
    log "Docker not found - installing..."
    curl -fsSL https://get.docker.com | sh
    sudo systemctl enable --now docker
    log "Docker installed."
    echo ""
fi

# Ensure Go is installed
if ! command -v go >/dev/null 2>&1; then
    ARCH=$(uname -m)
    case "$ARCH" in
        x86_64|amd64) ARCH="amd64" ;;
        aarch64|arm64) ARCH="arm64" ;;
        *) err "Unsupported architecture: $ARCH" ;;
    esac

    log "Go not found - installing go${GO_VERSION}..."
    GO_TARBALL="/tmp/go-${GO_VERSION}-${ARCH}.tar.gz"
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${ARCH}.tar.gz" -o "$GO_TARBALL"
    sudo rm -rf /usr/local/go
    sudo tar -C /usr/local -xzf "$GO_TARBALL"
    rm -f "$GO_TARBALL"
    export PATH="/usr/local/go/bin:$PATH"
    log "Go $(go version | awk '{print $3}') installed."
    echo ""
fi

# Resolve latest release tag
CURRENT_VERSION=$($INSTALL_DIR/lattice-runner version 2>/dev/null || echo 'unknown')
log "Current version: $CURRENT_VERSION"

LATEST_TAG=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" | grep '"tag_name"' | head -1 | cut -d'"' -f4)
if [ -z "$LATEST_TAG" ]; then
    err "Could not determine latest release tag from GitHub."
fi

log "Latest version:  ${LATEST_TAG}"

# Skip update if already on latest
if [ "$CURRENT_VERSION" = "$LATEST_TAG" ]; then
    log "Already running latest version. Nothing to do."
    exit 0
fi

echo ""

# Clone tag and build in a secure temp directory
BUILD_DIR=$(mktemp -d)
chmod 700 "$BUILD_DIR"
export GOPATH="${BUILD_DIR}/gopath"
export GOMODCACHE="${BUILD_DIR}/gomodcache"
export GOCACHE="${BUILD_DIR}/gocache"
mkdir -p "$GOPATH" "$GOMODCACHE" "$GOCACHE"

log "Pulling source for ${LATEST_TAG}..."
git clone --depth=1 --branch "${LATEST_TAG}" "https://github.com/${REPO}.git" "${BUILD_DIR}/src" 2>&1 | tail -1
cd "${BUILD_DIR}/src"

log "Building..."
CGO_ENABLED=0 go build -ldflags="-w -s -X main.Version=${LATEST_TAG}" -o lattice-runner .

# Verify binary was created
if [ ! -f "lattice-runner" ]; then
    err "Build failed — binary not created"
fi
log "Built: $(ls -lh lattice-runner | awk '{print $5}')"
echo ""

# Replace binary atomically — avoids "Text file busy" when upgrading a running binary
sudo cp lattice-runner "$INSTALL_DIR/lattice-runner.new"
sudo chmod +x "$INSTALL_DIR/lattice-runner.new"
sudo mv -f "$INSTALL_DIR/lattice-runner.new" "$INSTALL_DIR/lattice-runner"

# Cleanup build dir (trap will also clean up on failure)
rm -rf "$BUILD_DIR"
BUILD_DIR=""

# Ensure systemd service exists
SERVICE_FILE="/etc/systemd/system/lattice-runner.service"
if [ ! -f "$SERVICE_FILE" ]; then
    log "Creating systemd service..."
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
    log "Created and enabled lattice-runner.service"
fi

# Delay restart so the runner process that spawned this script can finish
# reporting success before systemd kills it.
log "Scheduling lattice-runner restart in 3 seconds..."
(sleep 3 && sudo systemctl restart lattice-runner) &

echo ""
log "Update complete — ${CURRENT_VERSION} -> ${LATEST_TAG}"
echo ""
