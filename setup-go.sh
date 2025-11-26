#!/bin/bash

# FreeSWITCH Call Control API - Go Installation Script
# This script installs Go 1.25.0 and sets up the environment

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Configuration
GO_VERSION="1.25.0"
GO_INSTALL_PATH="/usr/local/go"
TEMP_DIR="${TMPDIR:-/tmp}"

echo -e "${YELLOW}FreeSWITCH Call Control API - Go Setup${NC}"
echo "========================================"
echo ""

# Check if already installed
if command -v go &> /dev/null; then
    INSTALLED_VERSION=$(go version | awk '{print $3}' | sed 's/go//')
    if [ "$INSTALLED_VERSION" = "$GO_VERSION" ]; then
        echo -e "${GREEN}✓ Go $GO_VERSION is already installed${NC}"
        go version
        exit 0
    else
        echo -e "${YELLOW}⚠ Go $INSTALLED_VERSION is installed, but $GO_VERSION is required${NC}"
    fi
fi

# Detect OS and architecture
OS=$(uname -s)
ARCH=$(uname -m)

case "$OS" in
    Linux)
        OS_TYPE="linux"
        case "$ARCH" in
            x86_64) ARCH_TYPE="amd64" ;;
            aarch64) ARCH_TYPE="arm64" ;;
            armv7l) ARCH_TYPE="armv6l" ;;
            *)
                echo -e "${RED}✗ Unsupported architecture: $ARCH${NC}"
                exit 1
                ;;
        esac
        ;;
    Darwin)
        OS_TYPE="darwin"
        case "$ARCH" in
            x86_64) ARCH_TYPE="amd64" ;;
            arm64) ARCH_TYPE="arm64" ;;
            *)
                echo -e "${RED}✗ Unsupported architecture: $ARCH${NC}"
                exit 1
                ;;
        esac
        ;;
    *)
        echo -e "${RED}✗ Unsupported OS: $OS${NC}"
        exit 1
        ;;
esac

GO_FILENAME="go${GO_VERSION}.${OS_TYPE}-${ARCH_TYPE}.tar.gz"
GO_URL="https://go.dev/dl/${GO_FILENAME}"

echo "Detected system:"
echo "  OS: $OS ($OS_TYPE)"
echo "  Architecture: $ARCH ($ARCH_TYPE)"
echo "  Go version: $GO_VERSION"
echo ""

# Download Go
echo -e "${YELLOW}Downloading Go ${GO_VERSION}...${NC}"
GO_TARBALL="${TEMP_DIR}/${GO_FILENAME}"

if [ -f "$GO_TARBALL" ]; then
    echo "  Using cached download: $GO_TARBALL"
else
    if ! wget -O "$GO_TARBALL" "$GO_URL" 2>&1 | grep -E "^(--|--)"; then
        echo -e "${RED}✗ Failed to download Go${NC}"
        rm -f "$GO_TARBALL"
        exit 1
    fi
fi

echo -e "${GREEN}✓ Downloaded${NC}"
echo ""

# Check if we need sudo
SUDO=""
if [ ! -w "$GO_INSTALL_PATH" ] && [ -d "$GO_INSTALL_PATH" ]; then
    SUDO="sudo"
fi

# Extract Go
echo -e "${YELLOW}Installing Go to ${GO_INSTALL_PATH}...${NC}"
if [ -d "$GO_INSTALL_PATH" ]; then
    echo "  Removing existing installation..."
    $SUDO rm -rf "$GO_INSTALL_PATH"
fi

$SUDO mkdir -p "$(dirname "$GO_INSTALL_PATH")"
$SUDO tar -C "$(dirname "$GO_INSTALL_PATH")" -xzf "$GO_TARBALL"
echo -e "${GREEN}✓ Extracted${NC}"
echo ""

# Add to PATH
SHELL_RC="$HOME/.bashrc"
if [ -f "$HOME/.zshrc" ] && ! grep -q "go/bin" "$HOME/.bashrc"; then
    SHELL_RC="$HOME/.zshrc"
fi

echo -e "${YELLOW}Setting up PATH...${NC}"

# Configure system-wide PATH (affects all users and new sessions)
if [ ! -f /etc/profile.d/go.sh ]; then
    echo "  Creating system-wide Go PATH configuration"
    $SUDO tee /etc/profile.d/go.sh > /dev/null << 'EOF'
# Go installation PATH
export PATH=/usr/local/go/bin:$PATH
EOF
    $SUDO chmod +x /etc/profile.d/go.sh
else
    echo "  System-wide Go PATH already configured"
fi

# Also update user shell config for backward compatibility
if ! grep -q "/usr/local/go/bin" "$SHELL_RC"; then
    echo "  Adding Go to $SHELL_RC"
    echo "" >> "$SHELL_RC"
    echo "# Go installation" >> "$SHELL_RC"
    echo "export PATH=/usr/local/go/bin:\$PATH" >> "$SHELL_RC"
else
    echo "  Go PATH already configured in $SHELL_RC"
fi

# Add to current shell session (prepend to take precedence)
export PATH=/usr/local/go/bin:$PATH

echo -e "${GREEN}✓ PATH configured${NC}"
echo ""

# Verify installation
echo -e "${YELLOW}Verifying installation...${NC}"
GO_INSTALLED=$(/usr/local/go/bin/go version)
echo "  $GO_INSTALLED"

if echo "$GO_INSTALLED" | grep -q "$GO_VERSION"; then
    echo -e "${GREEN}✓ Go $GO_VERSION successfully installed!${NC}"
    echo ""
    echo -e "${YELLOW}Next steps:${NC}"
    echo "  1. Reload your shell: source $SHELL_RC"
    echo "  2. Build fs-api: cd $(dirname "$0") && go build -o fs-api"
    echo "  3. Install: sudo mv fs-api /usr/local/bin/"
else
    echo -e "${RED}✗ Installation verification failed${NC}"
    exit 1
fi

# Cleanup
rm -f "$GO_TARBALL"
