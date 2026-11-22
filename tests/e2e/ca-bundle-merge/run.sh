#!/usr/bin/env bash
# run.sh — end-to-end guard for the CA-bundle-merge fix.
#
# Brings up the writer + probe compose stack, then asserts:
#   [A] curl https://sts.us-east-1.amazonaws.com/ with --cacert /shared/ca.crt
#       (the merged system+Keploy bundle) succeeds at the TLS layer. Any HTTP
#       status that requires AWS to have decrypted the request — AWS STS
#       commonly responds with 200, 301, 302, 400, or 403 depending on
#       request-method/auth/routing — is proof of a successful handshake; a
#       curl exit reporting a TLS error is the failure we want to catch.
#       The assertion downstream mirrors this exact set.
#   [B] curl https://sts.us-east-1.amazonaws.com/ with --cacert /shared/keploy-ca.crt
#       and --capath /nonexistent fails with curl exit 60 ("unable to get
#       local issuer certificate") — this is the bug we are fixing: trusting
#       ONLY the Keploy MITM CA breaks non-proxied HTTPS.
#
# And for the trust-store sources other than the image's CA bundle (see the
# corp-* and ca-writer-* services in docker-compose.yml):
#   [C] no CA bundle in the writer (/etc/ssl/certs an empty volume): its merged
#       bundle still verifies AWS STS (keploy's built-in Mozilla roots), and the
#       writer logs nothing at WARN or above and no advice to install a CA
#       package.
#   [D] SSL_CERT_DIR naming a corporate CA, no CA bundle: the merged bundle
#       verifies both the corporate server and AWS STS.
#   [E] SSL_CERT_FILE naming a corporate CA: the merged bundle verifies the
#       corporate server.
#   [F] control: the plain and the no-bundle writers' merged bundles do NOT
#       verify the corporate server (curl exit 60), so [D] and [E] pass because
#       of the variables.
#
# All probes use `--cacert` (not REQUESTS_CA_BUNDLE) because that's what the
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

# The base images are pulled first, through the retry for a registry's rate
# limit or a transport fault, so the build finds them local and fetches only
# Go modules (also retried, on a transport fault). Docker Hub's official
# images: see Dockerfile.setup for why not ECR Public.
# shellcheck source=../../../.github/workflows/test_workflow_scripts/docker-build-retry.sh
source "${SCRIPT_DIR}/../../../.github/workflows/test_workflow_scripts/docker-build-retry.sh"
export GOLANG_IMAGE="${GOLANG_IMAGE:-golang:1.27.3-bookworm@sha256:972b4110ba0f47539a975c31e52c64765e1ff19f2d74469ae4a595c6676e80a5}"
export DEBIAN_IMAGE="${DEBIAN_IMAGE:-debian:trixie-slim}"
echo "=== pulling the base images ==="
docker_pull_retry "${GOLANG_IMAGE}"
docker_pull_retry "${DEBIAN_IMAGE}"
echo "=== building the harness image ==="
docker_build_retry "${DC[@]}" build

echo "=== bringing up the writers, the corporate server and tls-probe ==="
# tls-probe waits for every writer to complete successfully, so `up` itself
# fails when one does not; print the logs before the EXIT trap removes the
# containers with them.
if ! "${DC[@]}" up -d; then
    echo "[ASSERT][FAIL] docker compose up failed; service logs follow"
    "${DC[@]}" logs --no-color || true
    exit 1
fi
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
# wait_for_writer waits for one writer service to exit, failing the script
# unless it exited 0. Prints the container ID.
wait_for_writer() {
    local service="$1" container_id="" exit_code="" done_=0 state
    for _ in $(seq 1 60); do
        # `-a` includes stopped/exited containers; omitting it would drop the
        # writer from output the instant it finishes (seen on GHA where the
        # writer exits in <1s) and cause a spurious 60s timeout.
        container_id=$("${DC[@]}" ps -aq "${service}" 2>/dev/null || true)
        if [ -n "${container_id}" ]; then
            state=$(docker inspect -f '{{.State.Status}}' "${container_id}" 2>/dev/null || true)
            if [ "${state}" = "exited" ]; then
                exit_code=$(docker inspect -f '{{.State.ExitCode}}' "${container_id}" 2>/dev/null || echo "?")
                done_=1
                break
            fi
        fi
        sleep 1
    done
    if [ "${done_}" -ne 1 ]; then
        echo "[ASSERT][FAIL] ${service} did not reach 'exited' state within 60s" >&2
        if [ -n "${container_id}" ]; then
            echo "--- ${service} last state ---" >&2
            docker inspect -f '{{.State.Status}} exitCode={{.State.ExitCode}} error={{.State.Error}}' "${container_id}" >&2 || true
            echo "--- ${service} logs (tail) ---" >&2
            docker logs --tail 200 "${container_id}" >&2 || true
        fi
        exit 1
    fi
    if [ "${exit_code}" != "0" ]; then
        echo "[ASSERT][FAIL] ${service} exited with non-zero status: ${exit_code}" >&2
        docker logs --tail 200 "${container_id}" >&2 || true
        exit 1
    fi
    echo "${service} exited cleanly (exit=${exit_code})" >&2
    echo "${container_id}"
}

echo "=== waiting for the writers to finish writing ==="
wait_for_writer ca-writer >/dev/null
nobundle_container_id=$(wait_for_writer ca-writer-nobundle)
wait_for_writer ca-writer-certdir >/dev/null
wait_for_writer ca-writer-certfile >/dev/null

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

FAIL=0

echo ""
echo "=== [C] no CA bundle: the writer's log ==="
nobundle_log=$(docker logs "${nobundle_container_id}" 2>&1)
grep -E 'Merged system CA bundle' <<<"${nobundle_log}" || true
if ! grep -q 'embedded:mozilla-roots.pem' <<<"${nobundle_log}"; then
    echo "[ASSERT][FAIL] the no-bundle writer did not use keploy's built-in Mozilla roots"
    FAIL=1
elif grep -E $'\t(WARN|ERROR|DPANIC|PANIC|FATAL)\t' <<<"${nobundle_log}"; then
    echo "[ASSERT][FAIL] the no-bundle writer logged at WARN or above (the built-in roots are the normal path)"
    FAIL=1
elif grep -E 'apt-get|apk add|install .ca-certificates.' <<<"${nobundle_log}"; then
    echo "[ASSERT][FAIL] the no-bundle writer told the operator to install a CA package"
    FAIL=1
else
    echo "[ASSERT][PASS] the no-bundle writer used the built-in roots quietly"
fi

# corp_probe BUNDLE: curl the corporate server trusting only BUNDLE; prints
# curl's exit code. The server is local, so no egress is needed.
corp_probe() {
    local rc
    set +e
    "${DC[@]}" exec -T tls-probe \
        curl -sS --max-time 10 --retry 5 --retry-connrefused --retry-delay 1 \
            --capath /nonexistent --cacert "$1" \
            -o /dev/null https://corp-server:8443/ >&2
    rc=$?
    set -e
    echo "${rc}"
}

echo ""
echo "=== [D] [E] [F] the corporate server ==="
CERTDIR_CORP_RC=$(corp_probe /shared-certdir/ca.crt)
CERTFILE_CORP_RC=$(corp_probe /shared-certfile/ca.crt)
PLAIN_CORP_RC=$(corp_probe /shared/ca.crt)
NOBUNDLE_CORP_RC=$(corp_probe /shared-nobundle/ca.crt)
CORP_FAIL=0
# Exit codes are compared as strings, so an empty or odd capture fails closed.
if [ "${CERTDIR_CORP_RC}" != 0 ]; then
    echo "[ASSERT][FAIL] SSL_CERT_DIR's corporate CA is not in the merged bundle: curl rc=${CERTDIR_CORP_RC}"
    CORP_FAIL=1
else
    echo "[ASSERT][PASS] SSL_CERT_DIR's corporate CA is in the merged bundle"
fi
if [ "${CERTFILE_CORP_RC}" != 0 ]; then
    echo "[ASSERT][FAIL] SSL_CERT_FILE's corporate CA is not in the merged bundle: curl rc=${CERTFILE_CORP_RC}"
    CORP_FAIL=1
else
    echo "[ASSERT][PASS] SSL_CERT_FILE's corporate CA is in the merged bundle"
fi
if [ "${PLAIN_CORP_RC}" != 60 ] || [ "${NOBUNDLE_CORP_RC}" != 60 ]; then
    echo "[ASSERT][FAIL] control: a bundle without the corporate CA did not fail with curl exit 60 (plain rc=${PLAIN_CORP_RC}, no-bundle rc=${NOBUNDLE_CORP_RC})"
    CORP_FAIL=1
else
    echo "[ASSERT][PASS] control: the plain and no-bundle merged bundles do not trust the corporate CA (curl exit 60)"
fi
if [ "${CORP_FAIL}" != 0 ]; then
    FAIL=1
    echo "--- corp-server logs ---"
    "${DC[@]}" logs --no-color corp-server || true
fi

# Let callers skip both probes when there's no AWS STS egress on the
# runner (restricted build farm, offline developer laptops). In that
# mode we still run the byte-size assertion above — which is what
# directly proves the merge — and just skip the network round-trip.
if [ "${SKIP_EGRESS_PROBE:-0}" = "1" ]; then
    echo ""
    echo "=== SKIP_EGRESS_PROBE=1 — skipping AWS STS egress probes ==="
    echo "[ASSERT][SKIP] the AWS STS probes ([A] [B] [C] [D]) skipped"
    echo ""
    if [ "${FAIL}" -ne 0 ]; then
        echo "[RESULT] FAIL"
        exit 1
    fi
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

# sts_probe BUNDLE: curl AWS STS trusting only BUNDLE; prints "rc http".
sts_probe() {
    local out rc
    set +e
    out=$("${DC[@]}" exec -T tls-probe \
        curl -sS --max-time 20 "${CURL_RETRY[@]}" \
            --capath /nonexistent --cacert "$1" \
            -o /dev/null -w "%{http_code}" \
            https://sts.us-east-1.amazonaws.com/)
    rc=$?
    set -e
    echo "${rc} ${out}"
}

echo ""
echo "=== [C] [D] AWS STS with the no-bundle and SSL_CERT_DIR bundles ==="
read -r NOBUNDLE_RC NOBUNDLE_HTTP <<<"$(sts_probe /shared-nobundle/ca.crt)"
read -r CERTDIR_RC CERTDIR_HTTP <<<"$(sts_probe /shared-certdir/ca.crt)"

echo ""
echo "=== assertions ==="
for probe in "no-bundle:${NOBUNDLE_RC}:${NOBUNDLE_HTTP}" "SSL_CERT_DIR:${CERTDIR_RC}:${CERTDIR_HTTP}"; do
    IFS=: read -r name rc http <<<"${probe}"
    if [ "${rc}" != 0 ] || ! [[ "${http}" =~ ^(200|301|302|400|403)$ ]]; then
        echo "[ASSERT][FAIL] ${name} merged bundle did not verify AWS STS: rc=${rc} http=${http}"
        FAIL=1
    else
        echo "[ASSERT][PASS] ${name} merged bundle verified AWS STS: rc=${rc} http=${http}"
    fi
done
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
