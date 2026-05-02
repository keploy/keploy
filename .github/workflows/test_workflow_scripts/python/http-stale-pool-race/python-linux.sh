#!/bin/bash
# Regression test for the upstream-pool half-close race in keploy's
# HTTP/1.1 ingress proxy (issue #4165, fix in handleHttp1ZeroCopy via
# MSG_PEEK + replay-on-stale).
#
# Setup: gunicorn (sync worker, --keep-alive 2) sits behind keploy in
# record mode. A bursty client (burst_client.py) opens N persistent
# HTTP/1.1 connections to localhost:8080 and fires bursts with idle
# gaps longer than gunicorn's keep-alive. Without the fix, keploy
# silently drops the first request on each post-idle-gap burst because
# its two-goroutine io.Copy byte-pump never notices the upstream FIN
# during the gap. With the fix, MSG_PEEK detects the stale conn and
# redials before forwarding the next request.
#
# Pass/fail is driven by burst_client.py's drop-rate gate
# (MAX_DROP_PCT, default 5%). Without the fix the observed drop rate
# is ~25-50%; with the fix it is 0%.

set -uo pipefail

source "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/test-iid.sh"

cleanup() {
    echo "cleanup..."
    sudo pkill -9 -f keploy 2>/dev/null || true
    sudo pkill -9 -f gunicorn 2>/dev/null || true
    sleep 1
}
trap cleanup EXIT

cd "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/python/http-stale-pool-race"

echo "=== install python deps ==="
python -m pip install --upgrade pip >/dev/null
python -m pip install -r requirements.txt

echo "=== generate keploy config ==="
$RECORD_BIN config --generate
rm -rf keploy/

echo "=== start keploy record + gunicorn (gthread worker, --keep-alive 2) ==="
# gthread is the load-bearing worker class here — gunicorn's default
# (sync) silently DISABLES HTTP keep-alive regardless of --keep-alive,
# so connections never persist across the burst gap and the bug
# cannot manifest. gthread honors --keep-alive (closes idle conns
# after 2s), which is exactly the asymmetric timeout that exposes
# keploy's upstream-pool half-close race.
$RECORD_BIN record \
    -c "gunicorn --bind 0.0.0.0:8080 --workers 2 --threads 4 --worker-class gthread --keep-alive 2 main:app" \
    > record_logs.txt 2>&1 &
record_pid=$!

echo "=== wait for app readiness ==="
ready=0
for i in $(seq 1 40); do
    if curl -fsS --max-time 3 http://127.0.0.1:8080/api/health >/dev/null 2>&1; then
        echo "app up after $((i * 3))s"
        ready=1
        break
    fi
    sleep 3
done

if [ "$ready" -ne 1 ]; then
    echo "::error::app did not become ready within 120s"
    echo "=== record_logs.txt ==="
    tail -100 record_logs.txt
    exit 1
fi

echo "=== fire bursty load (burst_client.py gates pass/fail on drop rate) ==="
python burst_client.py
client_status=$?

echo "=== stop keploy record cleanly ==="
sudo pkill -INT -f 'keploy.*record' 2>/dev/null || true
sleep 5
sudo pkill -9 -f 'keploy.*record' 2>/dev/null || true
sleep 2

if grep -q 'WARNING: DATA RACE' record_logs.txt; then
    echo "::error::data race detected in keploy"
    tail -200 record_logs.txt
    exit 1
fi

if [ $client_status -ne 0 ]; then
    echo "::error::burst client exit=$client_status (drop rate exceeded threshold — bug NOT fixed)"
    echo "=== last 100 lines of record_logs.txt ==="
    tail -100 record_logs.txt
    exit 1
fi

echo "PASS: bursty load completed within drop threshold under keploy record"
