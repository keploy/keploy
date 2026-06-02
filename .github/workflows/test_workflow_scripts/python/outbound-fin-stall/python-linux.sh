#!/bin/bash
# Regression test for the 60s outbound tail introduced by the relay's
# missing peer-forwarder wakeup (issue #4173, fix in PR #4174).
#
# Setup: a tiny TCP server (silent_upstream.py) acts as an AWS-ALB-style
# upstream — it answers the first request on a keepalive conn, then
# sends FIN once the conn has been idle for 5 s. A urllib3 client
# (app.py) opens ONE pooled HTTP/1.1 connection, fires call-1, sleeps
# 8 s (longer than the upstream's idle budget), and fires call-2.
#
# urllib3's pool calls wait_for_read(sock, timeout=0) before reuse, so
# without keploy in the path it observes the FIN, throws the dead conn
# away, and reconnects in milliseconds.
#
# With keploy in the path, the relay's two forwarder goroutines are
# joined via wgForward.Wait(); when the upstream FINs, FromDest reads
# EOF and exits, but FromClient is still parked in src.Read on the
# client conn with no event to wake it. Without the fix in #4174, the
# relay hangs there until the application's own read_timeout fires
# (60 s for botocore's default), and call-2 fails with a
# ReadTimeoutError. With the fix, the closeStopping+nudgeDeadline
# coordinator wakes the peer forwarder; the relay tears down promptly
# and the client reconnects — call-2 succeeds in milliseconds.
#
# Pass criteria:
#   1. The app exits 0 (both POST calls returned 2xx).
#   2. keploy/test-set-0/mocks.yaml contains BOTH outbound HTTP
#      mocks. Without the fix, call-2 never receives a response so
#      its mock is incomplete and is dropped — only mock-0 (call-1)
#      lands on disk.

set -uo pipefail
# Explicit set +e: even when this script is sourced from a workflow
# step that has -e enabled by default (GitHub Actions does this for
# `run:` blocks), we want both the upstream-startup loop and the
# keploy-record invocation to NOT short-circuit the script — we
# capture exit codes explicitly and run cleanup on every path.
set +e

source "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/test-iid.sh"

# Match keploy by `comm` (process name) — pkill default mode. Avoid
# `pkill -f` regex matches against full argv: those self-match the
# wrapping bash (whose /proc/PID/cmdline includes the script path)
# and this script's sudo parent, surfacing as a 137 exit even on a
# passing run. See the cleanup comment in
# python/http-stale-pool-race/python-linux.sh for the full rationale.
# The upstream is killed by recorded PID (set after launch) rather
# than by name, since "python3" matches every Python process on the
# runner.
upstream_pid=""
cleanup() {
    echo "cleanup..."
    sudo pkill -9 keploy 2>/dev/null || true
    if [ -n "$upstream_pid" ]; then
        kill -9 "$upstream_pid" 2>/dev/null || true
    fi
    sleep 1
}
trap cleanup EXIT

cd "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/python/outbound-fin-stall"

echo "=== install python deps ==="
python3 -m pip install --upgrade pip >/dev/null
python3 -m pip install -r requirements.txt

echo "=== generate keploy config ==="
$RECORD_BIN config --generate
rm -rf keploy/

echo "=== start fake upstream (FIN after 5s idle) ==="
python3 silent_upstream.py 9090 > upstream_logs.txt 2>&1 &
upstream_pid=$!

# Wait for the upstream's listening socket. Use ss so the readiness
# check doesn't itself open a TCP conn that would skew the upstream's
# idle bookkeeping.
ready=0
for i in $(seq 1 20); do
    if ss -tln | awk '{print $4}' | grep -q ":9090$"; then
        echo "upstream listening after $((i))s"
        ready=1
        break
    fi
    sleep 1
done
if [ "$ready" -ne 1 ]; then
    echo "::error::upstream did not start within 20s"
    cat upstream_logs.txt
    exit 1
fi

echo "=== run keploy record + app.py (call-1, sleep 8s, call-2) ==="
sudo -E env PATH="$PATH" "$RECORD_BIN" record \
    -c "python3 app.py 127.0.0.1 9090 8" \
    > record_logs.txt 2>&1
record_status=$?

echo "=== record exit status: $record_status ==="

echo "=== check for data races / hard errors in record_logs.txt ==="
if grep -q 'WARNING: DATA RACE' record_logs.txt; then
    echo "::error::data race detected in keploy"
    tail -200 record_logs.txt
    exit 1
fi

echo "=== check app result (both calls must have returned 2xx) ==="
if ! grep -q 'call-1: status=200' record_logs.txt; then
    echo "::error::call-1 did not return 200"
    echo "--- last 100 lines of record_logs.txt ---"
    tail -100 record_logs.txt
    exit 1
fi
if ! grep -q 'call-2: status=200' record_logs.txt; then
    echo "::error::call-2 did not return 200 (60s tail regression of #4173)"
    grep -E '^\[app\]|FAILED|ReadTimeoutError' record_logs.txt | head -20 || true
    exit 1
fi

echo "=== check both mocks were captured (call-2 mock is the regression signal) ==="
mocks_file="keploy/test-set-0/mocks.yaml"
if [ ! -s "$mocks_file" ]; then
    echo "::error::mocks file missing or empty: $mocks_file"
    ls -la keploy/ || true
    exit 1
fi

# Each captured outbound mock starts with "name: mock-N" at the spec
# top level — count those rather than parsing YAML, to keep the
# script's runtime deps to coreutils.
mock_count=$(grep -cE '^name:\s+mock-' "$mocks_file" 2>/dev/null || echo 0)

echo "captured mocks: $mock_count"
if [ "$mock_count" -lt 2 ]; then
    echo "::error::expected 2 captured mocks (one per HTTP call); got $mock_count"
    echo "--- mocks.yaml summary ---"
    grep -E '^(name|kind|        Host:|        body:|    reqTimestampMock:)' "$mocks_file" | head -40
    echo "--- last 80 lines of record_logs.txt ---"
    tail -80 record_logs.txt
    exit 1
fi

echo "PASS: both outbound calls completed within their read_timeout AND both mocks were captured"
