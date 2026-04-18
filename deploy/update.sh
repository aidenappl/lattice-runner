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

# Check Go is installed
command -v go >/dev/null 2>&1 || {
    echo "ERROR: Go is required to build the runner."
    echo "Install it from: https://go.dev/dl/"
    exit 1
}

echo "Current binary: $(ls -lh $INSTALL_DIR/lattice-runner | awk '{print $5, $6, $7, $8}')"
echo ""

# Clone and build
echo "Pulling latest source..."
rm -rf "$BUILD_DIR"
git clone --depth=1 "$REPO" "$BUILD_DIR" 2>&1 | tail -1
cd "$BUILD_DIR"

echo "Building..."
CGO_ENABLED=0 go build -ldflags="-w -s" -o lattice-runner .
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
