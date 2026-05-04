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

cleanup() {
    if [ -n "${PROXY_PID:-}" ] && kill -0 "$PROXY_PID" 2>/dev/null; then
        kill "$PROXY_PID" 2>/dev/null || true
        wait "$PROXY_PID" 2>/dev/null || true
    fi
}
trap cleanup EXIT

# ── Cleanup ──
if [ -f "./keploy.yml" ]; then
    rm ./keploy.yml
fi
rm -rf keploy/

# ── Start a local CONNECT proxy ──
# Avoid apt/tinyproxy in CI. GitHub-hosted apt mirrors can stall, while Go is
# already provisioned for this workflow.
cat > /tmp/connect-proxy.go <<'EOF'
package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

func main() {
	ln, err := net.Listen("tcp", "127.0.0.1:3128")
	if err != nil {
		log.Fatal(err)
	}
	log.Println("connect proxy listening on 127.0.0.1:3128")
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Println(err)
			continue
		}
		go handle(conn)
	}
}

func handle(client net.Conn) {
	defer client.Close()

	reader := bufio.NewReader(client)
	req, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	if req.Method != http.MethodConnect {
		fmt.Fprint(client, "HTTP/1.1 405 Method Not Allowed\r\n\r\n")
		return
	}

	target := req.Host
	if !strings.Contains(target, ":") {
		target += ":443"
	}

	upstream, err := net.DialTimeout("tcp", target, 10*time.Second)
	if err != nil {
		fmt.Fprint(client, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer upstream.Close()

	fmt.Fprint(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
	errc := make(chan error, 2)
	go func() {
		_, err := io.Copy(upstream, reader)
		errc <- err
	}()
	go func() {
		_, err := io.Copy(client, upstream)
		errc <- err
	}()
	<-errc
}
EOF

go build -o /tmp/connect-proxy /tmp/connect-proxy.go
/tmp/connect-proxy &
PROXY_PID=$!
echo "CONNECT proxy started (PID: $PROXY_PID)"

# Verify proxy is listening
for attempt in {1..20}; do
    if (echo > /dev/tcp/127.0.0.1/3128) >/dev/null 2>&1; then
        echo "CONNECT proxy is ready on :3128"
        break
    fi
    if ! kill -0 "$PROXY_PID" 2>/dev/null; then
        echo "::error::CONNECT proxy exited before it became ready"
        exit 1
    fi
    sleep 1
done
if ! (echo > /dev/tcp/127.0.0.1/3128) >/dev/null 2>&1; then
    echo "::error::CONNECT proxy failed to start on port 3128"
    exit 1
fi

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

stop_recording() {
    local rec_pid
    rec_pid="$(pgrep -n -f "$(basename "${RECORD_BIN:-keploy}") record" || true)"
    if [ -n "$rec_pid" ]; then
        sudo kill -INT "$rec_pid" 2>/dev/null || true
        (
            sleep 15
            if kill -0 "$rec_pid" 2>/dev/null; then
                echo "===== FORCE KILLING KEPLOY RECORD PID $rec_pid ====="
                sudo kill -9 "$rec_pid" 2>/dev/null || true
            fi
        ) &
    fi
}

# ── Helper: send requests to the app ──
send_request() {
    sleep 6
    # Wait for app to be ready
    app_ready=false
    for i in {1..30}; do
        if curl -fsS --max-time 5 http://127.0.0.1:8080/health > /dev/null 2>&1; then
            app_ready=true
            break
        fi
        sleep 1
    done
    if [ "$app_ready" = false ]; then
        echo "::error::App failed to become ready on :8080 after 30s"
        stop_recording
        return 1
    fi

    echo "App is ready, sending requests..."

    # 1. Health check (no external deps)
    curl -fsS --max-time 10 http://127.0.0.1:8080/health || { stop_recording; return 1; }
    echo ""

    # 2. Request via CONNECT tunnel
    if ! curl -sS --max-time 15 http://127.0.0.1:8080/via-proxy; then
        echo "::warning::CONNECT tunnel request failed during record; report validation will decide if this is expected for the selected binary"
    fi
    echo ""

    # Wait for keploy to finish recording
    sleep 7
    stop_recording
    echo "Sent SIGINT to keploy record process"
}

# ── Record phase (2 iterations for dedup testing) ──
for i in 1 2; do
    app_name="connect-tunnel_${i}"
    send_request &
    REQ_PID=$!
    if ! timeout --kill-after=30s 8m env HTTP_PROXY=http://127.0.0.1:3128 HTTPS_PROXY=http://127.0.0.1:3128 \
        "$RECORD_BIN" record -c "./connect-tunnel" --generateGithubActions=false 2>&1 | tee "${app_name}.txt"; then
        echo "::error::connect-tunnel recording iteration $i failed or timed out"
        stop_recording
        exit 1
    fi

    if grep "ERROR" "${app_name}.txt" | grep "Keploy" | grep -v "tinyproxy\|WARNING\|CONNECT\|connection refused\|no matching.*mock"; then
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
    if ! wait "$REQ_PID"; then
        echo "::error::Request driver failed in recording iteration $i"
        exit 1
    fi
    echo "Recorded test cases for iteration $i"
done
echo "Recording complete. Test sets:"
ls -la keploy/*/tests/ 2>/dev/null || echo "No test sets found"
ls -la keploy/*/mocks/ 2>/dev/null || echo "No mocks found"

# ── Stop CONNECT proxy before replay ──
# This ensures replay uses mocks, not the real proxy.
echo "Stopping CONNECT proxy for replay..."
kill "$PROXY_PID" 2>/dev/null || true
wait "$PROXY_PID" 2>/dev/null || true
sleep 2

# ── Replay phase ──
# Allow non-zero exit from replay (some tests may fail with latest binary).
# We validate results from the report files below.
set +e
timeout --kill-after=30s 8m env HTTP_PROXY=http://127.0.0.1:3128 HTTPS_PROXY=http://127.0.0.1:3128 \
    "$REPLAY_BIN" test -c "./connect-tunnel" --delay 7 --generateGithubActions=false 2>&1 | tee test_logs.txt
replay_rc=${PIPESTATUS[0]}
set -e
if [ "$replay_rc" -eq 124 ] || [ "$replay_rc" -eq 137 ]; then
    echo "::error::connect-tunnel replay timed out"
    exit 1
fi

if grep "ERROR" "test_logs.txt" | grep "Keploy" | grep -v "tinyproxy\|WARNING\|CONNECT\|connection refused\|no matching.*mock"; then
    echo "::error::Error found in replay"
    cat "test_logs.txt"
    exit 1
fi

if grep -q "WARNING: DATA RACE" "test_logs.txt"; then
    echo "::error::Race condition detected in replay"
    cat "test_logs.txt"
    exit 1
fi

# ── Determine expected behavior ──
# CONNECT tunnel support only exists in the build from this branch.
# When either record or replay uses the "latest" release binary,
# the /via-proxy test (which depends on CONNECT) is expected to fail.
# Only the /health test (no CONNECT dependency) must always pass.
both_build=false
case "${RECORD_BIN:-}" in
    */build/keploy|*/build-no-race/keploy)
        case "${REPLAY_BIN:-}" in
            */build/keploy|*/build-no-race/keploy)
                both_build=true
                ;;
        esac
        ;;
esac

echo "Both binaries are build (CONNECT-capable): $both_build"

# ── Validate test reports ──
if [ "$both_build" = true ]; then
    # Full validation: all test sets must pass.
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

    if [ "$all_passed" != true ]; then
        echo "::error::Some tests failed. Dumping logs..."
        cat test_logs.txt
        exit 1
    fi
    exit 0
else
    # Partial validation: at least /health tests must pass.
    # /via-proxy failures are expected when latest binary lacks CONNECT support.
    echo "Latest binary lacks CONNECT support — validating /health tests only."
    health_passed=false
    if grep -q "test passed" test_logs.txt 2>/dev/null; then
        health_passed=true
    fi
    # Check that the test report exists and has at least 1 pass.
    for report_file in ./keploy/reports/test-run-0/test-set-*-report.yaml; do
        [ -e "$report_file" ] || continue
        pass_count=$(grep -c 'status: PASSED' "$report_file" 2>/dev/null || echo "0")
        if [ "$pass_count" -gt 0 ]; then
            health_passed=true
        fi
    done

    if [ "$health_passed" = true ]; then
        echo "Health tests passed (CONNECT tests expected to fail with latest binary)."
        exit 0
    else
        echo "::error::Even health tests failed — unexpected."
        cat test_logs.txt
        exit 1
    fi
fi
