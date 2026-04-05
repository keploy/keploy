#!/bin/bash

# E2E test for CONNECT tunnel support.
# Verifies that Keploy can record and replay HTTP requests that the app
# sends through an HTTP CONNECT proxy (corporate proxy pattern).
#
# Architecture during record:
#   [curl] → [app:8080] --CONNECT→ [tinyproxy:3128] --TLS→ [httpbin.org]
# Architecture during replay:
#   [keploy replayer] → [app:8080] --CONNECT→ [keploy proxy] → mock response

set -Eeuo pipefail

echo "RECORD_BIN=$RECORD_BIN"
echo "REPLAY_BIN=$REPLAY_BIN"

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

# ── Cleanup ──
if [ -f "./keploy.yml" ]; then
    rm ./keploy.yml
fi
rm -rf keploy/

# ── Install tinyproxy ──
echo "Installing tinyproxy..."
sudo apt-get update -qq && sudo apt-get install -y -qq tinyproxy > /dev/null 2>&1
echo "tinyproxy installed."

# ── Configure and start tinyproxy ──
cat > /tmp/tinyproxy.conf <<'PROXYCONF'
Port 3128
Listen 0.0.0.0
Timeout 600
MaxClients 100
Allow 0.0.0.0/0
ConnectPort 443
ConnectPort 80
LogLevel Info
PROXYCONF

tinyproxy -c /tmp/tinyproxy.conf &
PROXY_PID=$!
echo "tinyproxy started (PID: $PROXY_PID)"
sleep 2

# Verify proxy is listening
if ! nc -z localhost 3128 >/dev/null 2>&1; then
    echo "::error::tinyproxy failed to start on port 3128"
    exit 1
fi
echo "tinyproxy is ready on :3128"

# ── Build the app ──
go build -o connect-tunnel
echo "Go binary built."

# ── Generate keploy config with noise rules ──
sudo "$RECORD_BIN" config --generate
config_file="./keploy.yml"
if [ -f "$config_file" ]; then
    # httpbin.org returns dynamic fields — mark them as noise
    sed -i 's/global: {}/global: {"header": {"Date":[], "Content-Length":[]}, "body": {"origin":[], "headers.X-Amzn-Trace-Id":[]}}/' "$config_file"
fi

# ── Helper: send requests to the app ──
send_request() {
    sleep 6
    # Wait for app to be ready
    for i in {1..30}; do
        if curl -sf http://localhost:8080/health > /dev/null 2>&1; then
            break
        fi
        sleep 1
    done

    echo "App is ready, sending requests..."

    # 1. Health check (no external deps)
    curl -s http://localhost:8080/health
    echo ""

    # 2. Request via CONNECT tunnel
    curl -s --max-time 15 http://localhost:8080/via-proxy
    echo ""

    # Wait for keploy to finish recording
    sleep 7
    pid=$(pgrep -f "keploy record" || true)
    if [ -n "$pid" ]; then
        echo "Killing Keploy record process (PID: $pid)"
        sudo kill "$pid"
    fi
}

# ── Record phase (2 iterations for dedup testing) ──
for i in 1 2; do
    app_name="connect-tunnel_${i}"
    send_request &
    REQ_PID=$!
    HTTP_PROXY=http://localhost:3128 HTTPS_PROXY=http://localhost:3128 \
        "$RECORD_BIN" record -c "./connect-tunnel" --generateGithubActions=false 2>&1 | tee "${app_name}.txt"

    if grep "ERROR" "${app_name}.txt" | grep -v "tinyproxy\|WARNING"; then
        echo "::error::Error found in recording iteration $i"
        cat "${app_name}.txt"
        exit 1
    fi
    if grep -q "WARNING: DATA RACE" "${app_name}.txt"; then
        echo "::error::Race condition detected in recording"
        cat "${app_name}.txt"
        exit 1
    fi
    sleep 5
    wait "$REQ_PID" 2>/dev/null || true
    echo "Recorded test cases for iteration $i"
done

echo "Recording complete. Test sets:"
ls -la keploy/*/tests/ 2>/dev/null || echo "No test sets found"
ls -la keploy/*/mocks/ 2>/dev/null || echo "No mocks found"

# ── Stop tinyproxy before replay ──
# This ensures replay uses mocks, not the real proxy.
echo "Stopping tinyproxy for replay..."
kill "$PROXY_PID" 2>/dev/null || true
sleep 2

# ── Replay phase ──
HTTP_PROXY=http://localhost:3128 HTTPS_PROXY=http://localhost:3128 \
    "$REPLAY_BIN" test -c "./connect-tunnel" --delay 7 --generateGithubActions=false 2>&1 | tee test_logs.txt

if grep "ERROR" "test_logs.txt" | grep -v "tinyproxy\|WARNING"; then
    echo "::error::Error found in replay"
    cat "test_logs.txt"
    exit 1
fi

if grep -q "WARNING: DATA RACE" "test_logs.txt"; then
    echo "::error::Race condition detected in replay"
    cat "test_logs.txt"
    exit 1
fi

# ── Validate test reports ──
all_passed=true
for report_file in ./keploy/reports/test-run-0/test-set-*-report.yaml; do
    [ -e "$report_file" ] || { echo "No report files found!"; all_passed=false; break; }

    test_set_name=$(basename "$report_file" -report.yaml)
    test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')

    echo "Status for ${test_set_name}: $test_status"
    if [ "$test_status" != "PASSED" ]; then
        all_passed=false
        echo "::error::${test_set_name} did not pass"
    fi
done

if [ "$all_passed" = true ]; then
    echo "All CONNECT tunnel tests passed!"
    exit 0
else
    echo "::error::Some tests failed. Dumping logs..."
    cat test_logs.txt
    exit 1
fi
