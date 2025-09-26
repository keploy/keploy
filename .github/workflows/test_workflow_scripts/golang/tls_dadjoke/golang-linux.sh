#!/bin/bash
# CI: build go-joke-app, record with Keploy, then test with captured mocks — with FULL live logs

set -Eeuo pipefail

# --- Sanity checks ---
[ -x "${RECORD_BIN:-}" ] || { echo "::error::RECORD_BIN not set or not executable"; exit 1; }
[ -x "${REPLAY_BIN:-}" ] || { echo "::error::REPLAY_BIN not set or not executable"; exit 1; }
command -v go   >/dev/null 2>&1 || { echo "::error::go not found"; exit 1; }
command -v curl >/dev/null 2>&1 || { echo "::error::curl not found"; exit 1; }
command -v nc   >/dev/null 2>&1 || echo "::warning::nc (netcat) not found; port checks will use curl."

section() { echo -e "\n====== $* ======\n"; }

# PIDs for background tails / keploy
RECORD_TAIL_PID=""
TEST_TAIL_PID=""
KEPLOY_PID=""
APP_PIDS=""

cleanup() {
  section "Final Cleanup"
  # stop tails first so they don't hold file handles
  [ -n "$RECORD_TAIL_PID" ] && kill "$RECORD_TAIL_PID" 2>/dev/null || true
  [ -n "$TEST_TAIL_PID" ]   && kill "$TEST_TAIL_PID"   2>/dev/null || true

  # terminate keploy (if still around)
  [ -n "$KEPLOY_PID" ] && kill "$KEPLOY_PID" 2>/dev/null || true
  sleep 1
  [ -n "$KEPLOY_PID" ] && kill -9 "$KEPLOY_PID" 2>/dev/null || true

  # terminate the app if it somehow outlived keploy
  APP_PIDS="$(pgrep -f './go-joke-app' || true)"
  if [ -n "$APP_PIDS" ]; then
    kill $APP_PIDS 2>/dev/null || true
    sleep 1
    kill -9 $APP_PIDS 2>/dev/null || true
  fi
  echo "Cleanup complete."
}
die() {
  local rc=$?
  echo "::error::Pipeline failed on line ${BASH_LINENO[0]} (exit code=$rc)."
  section "Record Log (tail)"; [ -f record.log ] && tail -n 200 record.log || echo "record.log not found."
  section "Test Log (tail)";   [ -f test.log ]   && tail -n 200 test.log   || echo "test.log not found."
  exit "$rc"
}
trap die ERR
trap cleanup EXIT

wait_for_http() {
  local port="$1" host="localhost"
  section "Waiting for application on port $port..."
  for i in {1..30}; do
    if command -v nc >/dev/null 2>&1; then
      nc -z "$host" "$port" >/dev/null 2>&1 && { echo "✅ Port $port is open."; return 0; }
    else
      curl -s "http://$host:$port/health" >/dev/null 2>&1 || \
      curl -s "http://$host:$port"        >/dev/null 2>&1 && { echo "✅ Port $port responded."; return 0; }
    fi
    echo "Waiting... ($i/30)"; sleep 1
  done
  echo "::error::Application did not become available on port $port in time."; return 1
}

generate_traffic() {
  section "Generating HTTP Traffic"
  curl -s -o /dev/null -X GET http://localhost:8080/joke
  curl -s -o /dev/null -X GET http://localhost:8080/joke
  echo "Traffic generation complete."
}

check_for_errors() {
  local logfile=$1
  section "Checking $logfile for errors"
  if [ -f "$logfile" ] && (grep -q "ERROR" "$logfile" || grep -q "WARNING: DATA RACE" "$logfile"); then
    echo "::error::Critical error or race detected in $logfile"
    return 1
  fi
  echo "No critical errors found in $logfile."
}

check_test_report() {
  section "Checking Test Reports"
  local latest_run_dir
  latest_run_dir=$(ls -td ./keploy/reports/test-run-* 2>/dev/null | head -n 1 || true)
  [ -n "${latest_run_dir:-}" ] || { echo "::error::Test report directory not found!"; return 1; }
  local all_passed=true
  shopt -s nullglob
  for report_file in "$latest_run_dir"/test-set-*-report.yaml; do
    local status
    status=$(grep -m1 'status:' "$report_file" | awk '{print $2}')
    echo "Status for $(basename "$report_file"): $status"
    [ "$status" = "PASSED" ] || all_passed=false
  done
  $all_passed && echo "✅ All test sets passed." || { echo "::error::One or more test sets failed."; return 1; }
}

# --- Build ---
section "Build Application"
go build -o go-joke-app .
chmod +x ./go-joke-app

# --- Config ---
section "Setup & Configuration"
rm -rf ./keploy*
sudo -E env PATH="$PATH" "$RECORD_BIN" config --generate

# --- Record (live stream via tail -F) ---
section "Start Recording"
: > record.log
tail -F -n +1 record.log &
RECORD_TAIL_PID=$!
# Start keploy record in background; write to file that we’re tailing
sudo -E env PATH="$PATH" "$RECORD_BIN" record -c "./go-joke-app" --generateGithubActions=false > record.log 2>&1 &
KEPLOY_PID=$!
echo "Keploy record PID: $KEPLOY_PID"

wait_for_http 8080
generate_traffic
echo "Waiting for recordings to flush..."; sleep 5

section "Stop Recording"
# Gracefully stop keploy (it will stop the child app)
kill "$KEPLOY_PID" 2>/dev/null || true
wait "$KEPLOY_PID" || true
# stop live tail after keploy exits
kill "$RECORD_TAIL_PID" 2>/dev/null || true
RECORD_TAIL_PID=""
echo "Recording stopped."
check_for_errors record.log

# --- Test (live stream via tail -F) ---
section "Start Testing"
: > test.log
tail -F -n +1 test.log &
TEST_TAIL_PID=$!
sudo -E env PATH="$PATH" "$REPLAY_BIN" test \
  -c "./go-joke-app" \
  --delay 10 \
  --generateGithubActions=false \
  --disableMockUpload \
  > test.log 2>&1
# stop tail once test ends
kill "$TEST_TAIL_PID" 2>/dev/null || true
TEST_TAIL_PID=""
echo "Test run finished."

check_for_errors test.log
check_test_report

echo -e "\n✅ All tests passed successfully!"
