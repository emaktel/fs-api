#!/bin/bash

# FreeSWITCH Call Control API Installation Script
# Version: 0.2.0
# This script will:
# 1. Try to download and use pre-built binary
# 2. If that fails (GLIBC issues), install Go and build from source

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${YELLOW}Installing FreeSWITCH Call Control API...${NC}"
echo ""

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
        echo -e "${RED}Unsupported architecture: $ARCH${NC}"
        exit 1
        ;;
esac

echo "Detected: ${OS} ${ARCH}"
echo ""

# Function to build from source
build_from_source() {
    echo -e "${YELLOW}Building fs-api from source...${NC}"
    echo ""

    # Check if Go is installed with correct version
    REQUIRED_GO_VERSION="1.25.0"
    GO_INSTALLED=false

    if command -v go &> /dev/null; then
        CURRENT_GO_VERSION=$(go version | awk '{print $3}' | sed 's/go//')
        if [ "$CURRENT_GO_VERSION" = "$REQUIRED_GO_VERSION" ]; then
            GO_INSTALLED=true
            echo -e "${GREEN}✓ Go $REQUIRED_GO_VERSION is already installed${NC}"
        else
            echo -e "${YELLOW}⚠ Go $CURRENT_GO_VERSION found, but $REQUIRED_GO_VERSION is required${NC}"
        fi
    fi

    # Install Go if needed
    if [ "$GO_INSTALLED" = false ]; then
        echo -e "${YELLOW}Installing Go $REQUIRED_GO_VERSION...${NC}"

        # Check if setup-go.sh exists
        SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
        if [ -f "$SCRIPT_DIR/setup-go.sh" ]; then
            bash "$SCRIPT_DIR/setup-go.sh"
            # Add Go to PATH for this session
            export PATH=$PATH:/usr/local/go/bin
        else
            echo -e "${RED}✗ setup-go.sh not found. Please ensure it's in the same directory as install.sh${NC}"
            exit 1
        fi
        echo ""
    fi

    # Verify Go is available
    if ! command -v go &> /dev/null; then
        # Try adding to PATH
        export PATH=$PATH:/usr/local/go/bin
        if ! command -v go &> /dev/null; then
            echo -e "${RED}✗ Go installation failed or not in PATH${NC}"
            exit 1
        fi
    fi

    # Build the application
    echo -e "${YELLOW}Compiling fs-api...${NC}"
    SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    cd "$SCRIPT_DIR"

    if ! go build -o fs-api; then
        echo -e "${RED}✗ Build failed${NC}"
        exit 1
    fi

    echo -e "${GREEN}✓ Build successful${NC}"
    echo ""
}

# Try downloading pre-built binary first
USE_PREBUILT=true
BINARY_NAME="fs-api-${OS}-${ARCH}"
RELEASE_URL="https://github.com/emaktel/fs-api/releases/download/v0.2.0/${BINARY_NAME}"

echo -e "${YELLOW}Attempting to download pre-built binary...${NC}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

if command -v wget &> /dev/null; then
    if ! wget -q --show-progress "${RELEASE_URL}" -O fs-api 2>&1; then
        echo -e "${YELLOW}⚠ Download failed, will build from source${NC}"
        USE_PREBUILT=false
    fi
elif command -v curl &> /dev/null; then
    if ! curl -L "${RELEASE_URL}" -o fs-api 2>&1; then
        echo -e "${YELLOW}⚠ Download failed, will build from source${NC}"
        USE_PREBUILT=false
    fi
else
    echo -e "${YELLOW}⚠ wget or curl not found, will build from source${NC}"
    USE_PREBUILT=false
fi

# Test if pre-built binary works (check for GLIBC issues)
if [ "$USE_PREBUILT" = true ]; then
    chmod +x fs-api

    # Test the binary
    if ! ./fs-api --version &> /dev/null; then
        # Check if it's a GLIBC issue
        if ldd fs-api 2>&1 | grep -q "GLIBC.*not found"; then
            echo -e "${YELLOW}⚠ Pre-built binary has GLIBC compatibility issues${NC}"
            echo -e "${YELLOW}⚠ Will build from source instead${NC}"
            rm -f fs-api
            USE_PREBUILT=false
        else
            echo -e "${YELLOW}⚠ Pre-built binary test failed, will build from source${NC}"
            rm -f fs-api
            USE_PREBUILT=false
        fi
    else
        echo -e "${GREEN}✓ Pre-built binary is compatible${NC}"
    fi
fi
echo ""

# Build from source if pre-built didn't work
if [ "$USE_PREBUILT" = false ]; then
    build_from_source
fi

# Verify binary exists
if [ ! -f "fs-api" ]; then
    echo -e "${RED}✗ fs-api binary not found${NC}"
    exit 1
fi

# Make binary executable
chmod +x fs-api

# Install to /usr/local/bin
echo -e "${YELLOW}Installing to /usr/local/bin...${NC}"
sudo mv fs-api /usr/local/bin/fs-api

# Install systemd service if on Linux
if [ "$OS" = "linux" ]; then
    echo -e "${YELLOW}Installing systemd service...${NC}"
    sudo cp fs-api.service /etc/systemd/system/fs-api.service
    sudo systemctl daemon-reload
    sudo systemctl enable fs-api.service
    echo -e "${GREEN}✓ Service installed and enabled${NC}"

    # Try to start the service
    echo ""
    echo -e "${YELLOW}Starting fs-api service...${NC}"
    if sudo systemctl restart fs-api.service; then
        echo -e "${GREEN}✓ Service started successfully${NC}"
    else
        echo -e "${YELLOW}⚠ Service start may have failed. Check status with: systemctl status fs-api${NC}"
    fi

    echo ""
    echo -e "${GREEN}╔════════════════════════════════════════════════════════════════╗${NC}"
    echo -e "${GREEN}║         FreeSWITCH Call Control API - Installation Complete!  ║${NC}"
    echo -e "${GREEN}╚════════════════════════════════════════════════════════════════╝${NC}"
    echo ""
    echo -e "${YELLOW}Service Management:${NC}"
    echo "  Check status:  sudo systemctl status fs-api"
    echo "  Start service: sudo systemctl start fs-api"
    echo "  Stop service:  sudo systemctl stop fs-api"
    echo "  View logs:     journalctl -u fs-api -f"
    echo ""
    echo -e "${YELLOW}API Endpoints:${NC}"
    echo "  API Base:      http://localhost:37274/v1"
    echo "  Health Check:  http://localhost:37274/health"
    echo ""
    echo -e "${YELLOW}Test the API:${NC}"
    echo "  curl http://localhost:37274/health"
else
    echo -e "${GREEN}✓ Binary installed successfully${NC}"
    echo ""
    echo -e "${GREEN}╔════════════════════════════════════════════════════════════════╗${NC}"
    echo -e "${GREEN}║         FreeSWITCH Call Control API - Installation Complete!  ║${NC}"
    echo -e "${GREEN}╚════════════════════════════════════════════════════════════════╝${NC}"
    echo ""
    echo -e "${YELLOW}To run the API:${NC}"
    echo "  fs-api"
    echo ""
    echo -e "${YELLOW}Configuration (Environment Variables):${NC}"
    echo "  FSAPI_PORT=37274"
    echo "  ESL_HOST=localhost"
    echo "  ESL_PORT=8021"
    echo "  ESL_PASSWORD=ClueCon"
    echo ""
    echo -e "${YELLOW}API Endpoints:${NC}"
    echo "  API Base:      http://localhost:37274/v1"
    echo "  Health Check:  http://localhost:37274/health"
fi
