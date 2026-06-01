#!/usr/bin/env bash
# Build + run the SCRAM channel-binding e2e harness, then assert that
# the app container exited 0 (all three test phases passed).
#
# Exit codes:
#   0  — all phases passed
#   1  — docker compose up failed
#   2  — app container exited non-zero (test failure; see logs)
#
# Usage: ./run.sh

set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
cd "$here"

compose() { docker compose "$@"; }

cleanup() {
    echo ""
    echo "=== cleanup ==="
    compose logs --no-color --tail=200 mitm || true
    compose down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "=== building images ==="
compose build --quiet

echo "=== bringing up postgres + mitm ==="
compose up -d postgres mitm

echo "=== running app container (single-shot) ==="
# --abort-on-container-exit ensures docker-compose terminates as soon
# as the app exits, instead of waiting for postgres/mitm to die.
set +e
compose up --abort-on-container-exit --exit-code-from app app
app_exit=$?
set -e

echo ""
echo "=== app container exit code: $app_exit ==="

if [[ $app_exit -ne 0 ]]; then
    echo "FAIL: app container reported failure"
    exit 2
fi

echo ""
echo "PASS: SCRAM-SHA-256-PLUS auth succeeded across keploy MITM"
exit 0
