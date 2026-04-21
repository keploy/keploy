#!/usr/bin/env bash
# run.sh — end-to-end guard for the CA-bundle-merge fix.
#
# Brings up the writer + probe compose stack, then asserts:
#   [A] curl https://sts.us-east-1.amazonaws.com/ with REQUESTS_CA_BUNDLE=
#       /shared/ca.crt succeeds (HTTP 200/403 — anything but a TLS error).
#   [B] curl https://sts.us-east-1.amazonaws.com/ with REQUESTS_CA_BUNDLE=
#       /shared/keploy-ca.crt and --capath /nonexistent fails with curl exit
#       60 ("unable to get local issuer certificate") — this is the bug we
#       are fixing: trusting ONLY the Keploy MITM CA breaks non-proxied
#       HTTPS.
#
# PASS/FAIL is grepped from the emitted [ASSERT] lines at the end.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${SCRIPT_DIR}"

DC=("docker" "compose")
if ! docker compose version >/dev/null 2>&1; then
    DC=("docker-compose")
fi

cleanup() {
    echo ""
    echo "=== tearing down compose stack ==="
    "${DC[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "=== pruning any prior state ==="
"${DC[@]}" down -v --remove-orphans >/dev/null 2>&1 || true

echo "=== bringing up ca-writer + tls-probe ==="
"${DC[@]}" up -d --build
# Wait for the writer to finish (depends_on: service_completed_successfully
# on the probe already gates, but on older compose versions that's ignored —
# be explicit).
echo "=== waiting for ca-writer to finish writing ==="
for _ in $(seq 1 60); do
    status=$("${DC[@]}" ps --format json ca-writer 2>/dev/null | grep -oE '"State":"[a-zA-Z]+"' | head -1 || true)
    case "${status}" in
        *exited*) break ;;
        *) sleep 1 ;;
    esac
done

echo "=== verifying files on the shared volume ==="
"${DC[@]}" exec -T tls-probe ls -la /shared

CA_MERGED=$("${DC[@]}" exec -T tls-probe sh -c 'wc -c </shared/ca.crt' | awk '{print $1}')
CA_KEPLOY=$("${DC[@]}" exec -T tls-probe sh -c 'wc -c </shared/keploy-ca.crt' | awk '{print $1}')
echo "ca.crt: ${CA_MERGED} bytes    keploy-ca.crt: ${CA_KEPLOY} bytes"
# Sanity: merged must be strictly larger than keploy-only.
if [ "${CA_MERGED}" -le "${CA_KEPLOY}" ]; then
    echo "[ASSERT][FAIL] merged ca.crt is not larger than keploy-ca.crt"
    exit 1
fi

echo ""
echo "=== [A] TLS probe with MERGED bundle (expect success) ==="
set +e
"${DC[@]}" exec -T tls-probe \
    curl -sS --max-time 20 \
        --capath /nonexistent \
        --cacert /shared/ca.crt \
        -o /dev/null -w "HTTP=%{http_code} exit=%{exitcode}\n" \
        https://sts.us-east-1.amazonaws.com/ >/tmp/merged.out 2>&1
MERGED_RC=$?
set -e
cat /tmp/merged.out
MERGED_HTTP=$(grep -oE 'HTTP=[0-9]+' /tmp/merged.out | cut -d= -f2 || echo "")

echo ""
echo "=== [B] TLS probe with KEPLOY-ONLY bundle (expect failure, proves the guard) ==="
set +e
"${DC[@]}" exec -T tls-probe \
    curl -sS --max-time 20 \
        --capath /nonexistent \
        --cacert /shared/keploy-ca.crt \
        -o /dev/null -w "HTTP=%{http_code} exit=%{exitcode}\n" \
        https://sts.us-east-1.amazonaws.com/ >/tmp/keploy-only.out 2>&1
KEPLOY_RC=$?
set -e
cat /tmp/keploy-only.out

echo ""
echo "=== assertions ==="
FAIL=0
if [ "${MERGED_RC}" -ne 0 ] || ! [[ "${MERGED_HTTP}" =~ ^(200|301|302|400|403)$ ]]; then
    echo "[ASSERT][FAIL] merged bundle did not succeed: rc=${MERGED_RC} http=${MERGED_HTTP}"
    FAIL=1
else
    echo "[ASSERT][PASS] merged bundle succeeded: rc=${MERGED_RC} http=${MERGED_HTTP}"
fi
# curl exit 60 = "Peer certificate cannot be authenticated with given CA
# certificates". That's the exact symptom the customer saw in
# /home/shubham/globality/recording-issues/e2e/bug2/REPRODUCED.md — and it's
# the symptom this PR eliminates by merging the system bundle in.
if [ "${KEPLOY_RC}" -ne 60 ]; then
    echo "[ASSERT][FAIL] keploy-only bundle did not fail as expected: rc=${KEPLOY_RC} (expected curl exit 60 — CERT_VERIFY)"
    FAIL=1
else
    echo "[ASSERT][PASS] keploy-only bundle failed with curl exit 60 as expected (proves the bug would exist without the merge)"
fi

if [ "${FAIL}" -ne 0 ]; then
    echo ""
    echo "[RESULT] FAIL"
    exit 1
fi
echo ""
echo "[RESULT] PASS"
