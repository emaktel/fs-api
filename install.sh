#!/bin/bash

# FreeSWITCH Call Control API Installation Script
# Version: 0.1.0

set -e

echo "Installing FreeSWITCH Call Control API..."

# Detect OS and architecture
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)

# Map architecture names
case $ARCH in
    x86_64)
        ARCH="amd64"
        ;;
    aarch64|arm64)
        ARCH="arm64"
        ;;
    *)
        echo "Unsupported architecture: $ARCH"
        exit 1
        ;;
esac

# Set binary name
BINARY_NAME="fs-api-${OS}-${ARCH}"
RELEASE_URL="https://github.com/emaktel/fs-api/releases/download/v0.1.0/${BINARY_NAME}"

echo "Detected: ${OS} ${ARCH}"
echo "Downloading ${BINARY_NAME}..."

# Download binary
if command -v wget &> /dev/null; then
    wget -q --show-progress "${RELEASE_URL}" -O fs-api
elif command -v curl &> /dev/null; then
    curl -L "${RELEASE_URL}" -o fs-api
else
    echo "Error: wget or curl is required"
    exit 1
fi

# Make binary executable
chmod +x fs-api

# Install to /usr/local/bin
echo "Installing to /usr/local/bin..."
sudo mv fs-api /usr/local/bin/fs-api

# Install systemd service if on Linux
if [ "$OS" = "linux" ]; then
    echo "Installing systemd service..."
    sudo cp fs-api.service /etc/systemd/system/fs-api.service
    sudo systemctl daemon-reload
    sudo systemctl enable fs-api.service

    echo ""
    echo "Installation complete!"
    echo ""
    echo "To start the service:"
    echo "  sudo systemctl start fs-api"
    echo ""
    echo "To check status:"
    echo "  sudo systemctl status fs-api"
    echo ""
    echo "To view logs:"
    echo "  journalctl -u fs-api -f"
else
    echo ""
    echo "Installation complete!"
    echo ""
    echo "To run the API:"
    echo "  fs-api"
    echo ""
    echo "Configure using environment variables:"
    echo "  FSAPI_PORT=37274"
    echo "  ESL_HOST=localhost"
    echo "  ESL_PORT=8021"
    echo "  ESL_PASSWORD=ClueCon"
fi

echo ""
echo "API will be available at: http://localhost:37274"
echo "Health check: http://localhost:37274/health"
