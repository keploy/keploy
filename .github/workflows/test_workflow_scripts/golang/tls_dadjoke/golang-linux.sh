#!/bin/bash

# This script tests the go-joke-app sample application by:
# 1. Building the Go binary.
# 2. Using Keploy to record the application's interactions.
# 3. Using Keploy to run tests, replaying the captured interactions as mocks.
#
# This script incorporates best practices for CI automation, including:
# - Robust error handling with `trap`.
# - Collapsible log sections for readability.
# - Precise process management using PIDs.

set -Eeuo pipefail

# --- Source Common Scripts & Perform Sanity Checks ---
[ -x "${RECORD_BIN:-}" ] || { echo "::error::RECORD_BIN not set or not executable"; exit 1; }
[ -x "${REPLAY_BIN:-}" ] || { echo "::error::REPLAY_BIN not set or not executable"; exit 1; }
command -v go >/dev/null 2>&1 || { echo "::error::go not found"; exit 1; }
command -v curl >/dev/null 2>&1 || { echo "::error::curl not found"; exit 1; }

# --- Helper Functions for Logging & Error Handling ---
section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

# Final cleanup routine to ensure no processes are left running.
cleanup() {
    section "Final Cleanup"
    # Use pkill as a final safety net to catch any stray processes.
    pkill -f keploy || true
    pkill -f go-joke-app || true
    sleep 2
    pkill -9 -f keploy || true
    pkill -9 -f go-joke-app || true
    echo "Cleanup complete."
    endsec
}

# Error handler: dumps logs from all stages upon any script failure.
die() {
  local rc=$?
  echo "::error::Pipeline failed on line ${BASH_LINENO[0]} (exit code=$rc)."
  section "Record Log"
  cat record.log 2>/dev/null || echo "Record log not found."
  endsec
  section "Test Log"
  cat test.log 2>/dev/null || echo "Test log not found."
  endsec
  # The cleanup trap will run automatically on EXIT.
  exit "$rc"
}
trap die ERR
trap cleanup EXIT

# --- Helper Functions for Script Logic ---

# Waits for an HTTP endpoint to become available.
wait_for_http() {
  local port="$1"
  local host="localhost"
  section "Waiting for application on port $port..."
  for i in {1..30}; do
    if nc -z "$host" "$port" >/dev/null 2>&1; then
      echo "✅ Application port $port is open."
      endsec
      return 0
    fi
    echo "Waiting for app... (attempt $i/30)"
    sleep 1
  done
  echo "::error::Application did not become available on port $port in time."
  endsec
  return 1
}

# Sends HTTP requests to the application to generate traffic for recording.
generate_traffic() {
    section "Generating HTTP Traffic"
    echo "Sending GET requests to http://localhost:8080/joke..."
    # Make a few calls to get different jokes, creating multiple test cases.
    curl -s -o /dev/null -X GET http://localhost:8080/joke
    curl -s -o /dev/null -X GET http://localhost:8080/joke
    echo "Traffic generation complete."
    endsec
}

# Checks a log file for critical errors or data races.
check_for_errors() {
  local logfile=$1
  section "Checking for errors in $logfile..."
  if [ -f "$logfile" ] && (grep -q "ERROR" "$logfile" || grep -q "WARNING: DATA RACE" "$logfile"); then
    echo "::error::Critical error or race condition detected in $logfile"
    cat "$logfile"
    return 1
  fi
  echo "No critical errors found in $logfile."
  endsec
}

# Validates the Keploy test report to ensure all test sets passed.
check_test_report() {
    section "Checking Test Reports"
    local latest_run_dir
    latest_run_dir=$(ls -td ./keploy/reports/test-run-* | head -n 1)
    [ -d "$latest_run_dir" ] || { echo "::error::Test report directory not found!"; return 1; }

    local all_passed=true
    for report_file in "$latest_run_dir"/test-set-*-report.yaml; do
        [ -f "$report_file" ] || { echo "No report files found."; all_passed=false; break; }
        
        local status
        status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
        echo "Status for $(basename "$report_file"): $status"
        [ "$status" == "PASSED" ] || all_passed=false
    done

    [ "$all_passed" = true ] || { echo "::error::One or more test sets failed."; return 1; }
    echo "✅ All test sets passed."
    endsec
}

# --- Main Execution Logic ---

section "Build Application"
echo "Building the Go Joke App binary..."
go build -o go-joke-app .
chmod +x ./go-joke-app
endsec

section "Setup & Configuration"
echo "Preparing for recording..."
rm -rf ./keploy*
sudo -E env PATH="$PATH" "$RECORD_BIN" config --generate
endsec

# --- 1. Record Application Interactions ---
section "Start Recording"
# Start keploy record in the background, redirecting its output to a log file.
sudo -E env PATH="$PATH" "$RECORD_BIN" record -c "./go-joke-app" --generateGithubActions=false > record.log 2>&1 &
KEPLOY_PID=$!
echo "Keploy record process started with PID: $KEPLOY_PID"
endsec

# Wait for the app to be ready and then generate traffic.
wait_for_http 8080
generate_traffic

# Allow a moment for Keploy to capture the final interactions.
echo "Waiting for recordings to be flushed..."
sleep 5

section "Stop Recording"
echo "Stopping Keploy record process (PID: $KEPLOY_PID)..."
# Stop the specific Keploy process gracefully.
sudo kill "$KEPLOY_PID"
wait "$KEPLOY_PID" || true # Wait for it to exit, ignoring error if already gone.
echo "Recording stopped."
check_for_errors "record.log"
endsec

# --- 2. Test Application with Captured Mocks ---
section "Start Testing"
echo "Starting Keploy in test mode..."
# This command will fail if tests fail, and the `trap` will catch it.
sudo -E env PATH="$PATH" "$REPLAY_BIN" test -c "./go-joke-app" --delay 10 --generateGithubActions=false --disableMockUpload > test.log 2>&1
echo "Test run finished."
endsec

# --- 3. Validate Test Results ---
check_for_errors "test.log"
check_test_report

# --- Final Result ---
echo "✅ All tests passed successfully!"
exit 0
