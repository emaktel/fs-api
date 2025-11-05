#!/bin/bash
# Quick dev build and deploy script for fs-api
# Usage: ./dev-deploy.sh

set -e

echo "=== fs-api Development Deploy ==="
echo ""

echo "1. Stopping fs-api service..."
sudo systemctl stop fs-api

echo "2. Building fs-api from source..."
go build -o builds/fs-api

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
