#!/bin/sh
# Local convenience for rebuilding the cbshim variants outside CI.
# Mirrors pkg/util/time/build.sh (the time-freezing equivalent).
#
# Prereqs on Debian/Ubuntu:
#   apt-get install -y clang-14 gcc-aarch64-linux-gnu g++-aarch64-linux-gnu libssl-dev
#
# The enterprise CI does the same two clang invocations via
# prepare-dockerfile.sh's injected RUN clause, so this script is for
# dev-loop / local-testing use only. Its output is gitignored.

set -e

ASSET_DIR="$(cd "$(dirname "$0")" && pwd)"

echo "Building amd64 (x86_64-linux-gnu)..."
clang-14 -Wno-incompatible-function-pointer-types \
    --target=x86_64-linux-gnu \
    -fPIC -shared \
    -o "$ASSET_DIR/cbshim_amd64.so" \
       "$ASSET_DIR/cbshim.c" \
    -ldl -lcrypto -lpthread

echo "Building arm64 (aarch64-linux-gnu)..."
clang-14 -Wno-incompatible-function-pointer-types \
    --target=aarch64-linux-gnu \
    -I/usr/aarch64-linux-gnu/include \
    -fPIC -shared \
    -o "$ASSET_DIR/cbshim_arm64.so" \
       "$ASSET_DIR/cbshim.c" \
    -ldl -lcrypto -lpthread

echo ""
echo "Built:"
ls -la "$ASSET_DIR/cbshim_amd64.so" "$ASSET_DIR/cbshim_arm64.so"
