#!/bin/bash
# Regression guard: a once-per-boot outbound call — e.g. an AWS Secret
# Manager fetch performed at application startup, BEFORE the app serves
# any inbound request — must be captured as a mock in EVERY recorded test
# set.
#
# Setup: a fake secret-manager upstream (upstream.py) answers a single
# GET /secret. The app (app.py) fetches that secret ONCE at import time
# via urllib3 (botocore/boto3's transport), then serves /value. keploy
# therefore captures the fetch while `firstReqSeen` is still false: it is
# a "startup mock" owning no test window.
#
# Before the fix, syncMock's buffer reapers could drop such a window-less
# startup mock — the dedup DeleteMocksStrictlyBefore cleanup (when the
# first inbound request hashes as a duplicate), the 7s stale-cutoff in
# ResolveRange, and FlushOwnedWindows never flushing window-less mocks.
# The user-visible symptom was "the startup mock is present in some test
# sets and missing in others". This guard records TWO test sets and
# asserts the startup mock landed in BOTH.
#
# NOTE: the *deterministic* trigger is the enterprise `--static-dedup`
# path, which the OSS binary this CI uses does not expose; the precise
# regression is covered by the Go unit tests in
# pkg/agent/proxy/syncMock/syncMock_test.go. This guard exercises the OSS
# record path end-to-end so a future regression in startup-mock
# persistence is caught against a real application.

set -uo pipefail
# Explicit `set +e`: even when sourced from a workflow step with `-e`
# enabled, we capture exit codes explicitly and always run cleanup.
set +e

source "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/test-iid.sh"

# Match keploy by process name (pkill default) — `pkill -f` would
# self-match the wrapping bash/sudo argv and surface as a spurious 137.
# The upstream is killed by recorded PID since "python3" matches every
# Python process on the runner. See outbound-fin-stall for the rationale.
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

cd "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/python/startup-mock-capture"

echo "=== install python deps ==="
python3 -m pip install --upgrade pip >/dev/null
python3 -m pip install -r requirements.txt

echo "=== generate keploy config ==="
$RECORD_BIN config --generate
rm -rf keploy/

echo "=== start fake AWS Secret Manager upstream ==="
python3 upstream.py 9091 > upstream_logs.txt 2>&1 &
upstream_pid=$!

# Wait for the upstream's listening socket via ss (no extra TCP conn).
ready=0
for i in $(seq 1 20); do
    if ss -tln | awk '{print $4}' | grep -q ":9091$"; then
        echo "upstream listening after ${i}s"
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

# Drive one inbound request per record session so the test set has a test
# case alongside the startup mock, then SIGINT keploy to flush the set.
drive_request() {
    local up=0
    for _ in $(seq 1 40); do
        if curl -fsS --max-time 5 http://127.0.0.1:8000/health >/dev/null 2>&1; then
            up=1
            break
        fi
        sleep 1
    done
    if [ "$up" -ne 1 ]; then
        echo "::error::app /health did not become ready within 40s"
        sudo pkill -INT keploy 2>/dev/null || true
        return 1
    fi
    curl -fsS --max-time 10 http://127.0.0.1:8000/value >/dev/null 2>&1
    sleep 5
    sudo pkill -INT keploy 2>/dev/null || true
}

# Record two test sets. Each is a fresh app boot, so each fires the
# one-shot startup secret fetch — the mock must land in BOTH.
for i in 0 1; do
    echo "=== record test-set-$i ==="
    drive_request &
    driver_pid=$!
    sudo -E env PATH="$PATH" "$RECORD_BIN" record \
        -c "python3 app.py 127.0.0.1 9091" \
        > "record_logs_$i.txt" 2>&1
    if ! wait "$driver_pid"; then
        echo "::error::request driver failed while recording test-set-$i"
        tail -100 "record_logs_$i.txt"
        exit 1
    fi

    if grep -q 'WARNING: DATA RACE' "record_logs_$i.txt"; then
        echo "::error::data race detected during record of test-set-$i"
        tail -200 "record_logs_$i.txt"
        exit 1
    fi
    if ! grep -q 'startup secret fetched' "record_logs_$i.txt"; then
        echo "::error::app did not complete its startup secret fetch in test-set-$i"
        tail -100 "record_logs_$i.txt"
        exit 1
    fi
    sleep 3
done

echo "=== verify the startup secret mock landed in BOTH test sets ==="
missing=0
for i in 0 1; do
    mocks_file="keploy/test-set-$i/mocks.yaml"
    if [ ! -s "$mocks_file" ]; then
        echo "::error::mocks file missing or empty for test-set-$i: $mocks_file"
        ls -la "keploy/test-set-$i/" 2>/dev/null || true
        missing=1
        continue
    fi
    # The fake upstream's response body carries a unique marker; its
    # presence proves the once-per-boot startup fetch was captured as a
    # mock for THIS test set.
    if grep -q 'keploy-startup-secret-v1' "$mocks_file"; then
        echo "test-set-$i: startup secret mock present"
    else
        echo "::error::test-set-$i is MISSING the startup secret-manager mock (the regression)"
        echo "--- mocks.yaml summary for test-set-$i ---"
        grep -E '^(name:|kind:)|[Uu]rl|Host' "$mocks_file" | head -40
        echo "--- last 80 lines of record_logs_$i.txt ---"
        tail -80 "record_logs_$i.txt"
        missing=1
    fi
done

if [ "$missing" -ne 0 ]; then
    echo "::error::the once-per-boot startup mock was dropped from at least one test set"
    exit 1
fi

echo "PASS: the once-per-boot startup secret mock was captured in every test set"
