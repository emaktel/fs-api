#!/bin/bash
# Quick dev build and deploy script for fs-api
# Usage: ./dev-deploy.sh

set -e

# Use explicit Go path to ensure correct version
GO_BIN="/usr/local/go/bin/go"

echo "=== fs-api Development Deploy ==="
echo ""

# Verify Go version
REQUIRED_GO_VERSION="1.25.0"
if [ -f "$GO_BIN" ]; then
    CURRENT_GO_VERSION=$($GO_BIN version | awk '{print $3}' | sed 's/go//')
    if [ "$CURRENT_GO_VERSION" != "$REQUIRED_GO_VERSION" ]; then
        echo "⚠ Warning: Go version mismatch. Expected $REQUIRED_GO_VERSION, got $CURRENT_GO_VERSION"
    fi
else
    echo "✗ Go not found at $GO_BIN. Please run setup-go.sh first."
    exit 1
fi

echo "1. Stopping fs-api service..."
sudo systemctl stop fs-api

echo "2. Building fs-api from source (using Go $CURRENT_GO_VERSION)..."
mkdir -p builds
$GO_BIN build -o builds/fs-api

echo "3. Installing to /usr/local/bin..."
sudo cp builds/fs-api /usr/local/bin/fs-api

echo "4. Starting fs-api service..."
sudo systemctl start fs-api

echo "5. Waiting for service to start..."
sleep 2

echo ""
echo "=== Health Check ==="
if curl -s http://localhost:37274/health | grep -q "healthy"; then
    echo "✓ Service is healthy"
else
    echo "⚠ Service may still be starting, check with: systemctl status fs-api"
fi

echo ""
echo "✓ Deploy complete! Ready to test."
