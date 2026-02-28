#!/bin/bash

set -e

echo "Building Keploy Docker image with local integrations..."

# Go to parent directory
cd "$(dirname "$0")/.."

# Build Docker image from parent directory
docker build \
  -f keploy/Dockerfile \
  -t keploy/keploy:local \
  -t ghcr.io/keploy/keploy:v3-dev \
  --build-arg VERSION=3-dev \
  .

echo "Docker image built successfully!"
echo "Tags: keploy/keploy:local, ghcr.io/keploy/keploy:v3-dev"
