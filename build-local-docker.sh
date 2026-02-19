#!/bin/bash

set -e

echo "Building Keploy Docker image with local integrations..."

# Get the parent directory (where both keploy and integrations are)
PARENT_DIR="$(cd "$(dirname "$0")/.." && pwd)"

echo "Building from: $PARENT_DIR"
echo "This directory should contain both 'keploy' and 'integrations' folders"

# Build from parent directory
cd "$PARENT_DIR"
docker build \
  -f keploy/Dockerfile.local \
  -t ghcr.io/keploy/keploy:v3-dev \
  --build-arg VERSION=3-dev \
  .

echo ""
echo "✅ Docker image built successfully!"
echo "Tag: ghcr.io/keploy/keploy:v3-dev"
echo ""
echo "This image includes your local changes from:"
echo "  - keploy/"
echo "  - integrations/"
