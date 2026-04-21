#!/usr/bin/env bash
# run.sh — end-to-end guard for the CA-bundle-merge fix.
#
# Brings up the writer + probe compose stack, then asserts:
#   [A] curl https://sts.us-east-1.amazonaws.com/ with --cacert /shared/ca.crt
#       (the merged system+Keploy bundle) succeeds (HTTP 200/403 — anything
#       but a TLS error).
#   [B] curl https://sts.us-east-1.amazonaws.com/ with --cacert /shared/keploy-ca.crt
#       and --capath /nonexistent fails with curl exit 60 ("unable to get
#       local issuer certificate") — this is the bug we are fixing: trusting
#       ONLY the Keploy MITM CA breaks non-proxied HTTPS.
#
# Both probes use `--cacert` (not REQUESTS_CA_BUNDLE) because that's what the
# actual shared-volume webhook wiring ends up setting at the app runtime:
# REQUESTS_CA_BUNDLE/SSL_CERT_FILE point at `/tmp/keploy-tls/ca.crt`, curl's
# `--cacert` is the equivalent override. Using `--cacert` here avoids a stray
# env-var dance and makes the failure mode readable in the job log.
#
# The real-endpoint check (AWS STS) does pull on network egress. To reduce
# transient-DNS/routing noise we ask curl for modest retries below, and the
# run can be skipped entirely by setting `SKIP_EGRESS_PROBE=1` — useful for
# restricted CI environments where AWS STS is not reachable from the runner.
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
# Wait for the writer to finish. depends_on: service_completed_successfully on
# the probe gates on newer compose, but older docker-compose versions (which
# this script explicitly supports via the DC fallback above) ignore that
# condition, so be explicit.
#
# The portable way to query container state works on both docker-compose v1
# and docker compose v2:
#   1. resolve the service name to a container ID with `ps -aq` (the `-a`
#      is load-bearing: once the writer exits, the default `ps -q` excludes
#      it and returns empty, which would mask a clean exit as a timeout);
#   2. ask the engine directly via `docker inspect`.
# This avoids `--format json`, which v1 doesn't support, and avoids grepping
# a specific JSON field name which has varied between compose versions.
#
# We MUST fail the script if the writer has not reached an exited state
# within the timeout — falling through and running probes against a
# still-writing shared volume would produce confusing race failures.
echo "=== waiting for ca-writer to finish writing ==="
writer_container_id=""
writer_exit_code=""
writer_done=0
for _ in $(seq 1 60); do
    # `-a` includes stopped/exited containers; omitting it would drop the
    # writer from output the instant it finishes (seen on GHA where the
    # writer exits in <1s) and cause a spurious 60s timeout.
    writer_container_id=$("${DC[@]}" ps -aq ca-writer 2>/dev/null || true)
    if [ -n "${writer_container_id}" ]; then
        state=$(docker inspect -f '{{.State.Status}}' "${writer_container_id}" 2>/dev/null || true)
        if [ "${state}" = "exited" ]; then
            writer_exit_code=$(docker inspect -f '{{.State.ExitCode}}' "${writer_container_id}" 2>/dev/null || echo "?")
            writer_done=1
            break
        fi
    fi
    sleep 1
done
if [ "${writer_done}" -ne 1 ]; then
    echo "[ASSERT][FAIL] ca-writer did not reach 'exited' state within 60s"
    if [ -n "${writer_container_id}" ]; then
        echo "--- ca-writer last state ---"
        docker inspect -f '{{.State.Status}} exitCode={{.State.ExitCode}} error={{.State.Error}}' "${writer_container_id}" || true
        echo "--- ca-writer logs (tail) ---"
        docker logs --tail 200 "${writer_container_id}" || true
    fi
    exit 1
fi
if [ "${writer_exit_code}" != "0" ]; then
    echo "[ASSERT][FAIL] ca-writer exited with non-zero status: ${writer_exit_code}"
    docker logs --tail 200 "${writer_container_id}" || true
    exit 1
fi
echo "ca-writer exited cleanly (exit=${writer_exit_code})"

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

# Let callers skip both probes when there's no AWS STS egress on the
# runner (restricted build farm, offline developer laptops). In that
# mode we still run the byte-size assertion above — which is what
# directly proves the merge — and just skip the network round-trip.
if [ "${SKIP_EGRESS_PROBE:-0}" = "1" ]; then
    echo ""
    echo "=== SKIP_EGRESS_PROBE=1 — skipping AWS STS egress probes ==="
    echo "[ASSERT][SKIP] merged / keploy-only bundle probes skipped"
    echo ""
    echo "[RESULT] PASS"
    exit 0
fi

# curl retry flags:
#   --retry 5                  — up to 5 retries on transient failures
#   --retry-all-errors         — retry even on 5xx / connection errors
#                                (not just the curl-default 4xx-missing set)
#   --retry-delay 2            — 2s between retries (linear, not
#                                exponential — keeps total wall-clock
#                                bounded)
#   --retry-max-time 30        — give up retrying after 30s total so
#                                we don't stall CI on genuine outages
# --max-time covers each individual attempt. Transient DNS / TCP reset
# from GHA runners to AWS is the main thing these retries guard against;
# they do NOT mask genuine CA-verification failures because curl only
# retries on TRANSPORT errors, not on TLS errors (curl 77/60/etc.).
CURL_RETRY=(--retry 5 --retry-all-errors --retry-delay 2 --retry-max-time 30)

echo ""
echo "=== [A] TLS probe with MERGED bundle (expect success) ==="
set +e
"${DC[@]}" exec -T tls-probe \
    curl -sS --max-time 20 "${CURL_RETRY[@]}" \
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
# NOTE: curl retries are intentionally OMITTED in this probe. We WANT curl
# to fail fast with a TLS-verify error (exit 60) — retrying the same
# cert-verification failure 5 times just wastes ~10s of runner time.
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
# certificates". That's the exact failure mode this PR eliminates by
# merging the system bundle into /tmp/keploy-tls/ca.crt (see
# pkg/agent/proxy/tls/ca.go::setupSharedVolume) — seeing it here
# (probe B, keploy-only bundle) is proof the guard trips when the fix
# is absent.
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
