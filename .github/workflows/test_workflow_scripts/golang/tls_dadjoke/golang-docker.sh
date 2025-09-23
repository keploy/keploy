#!/bin/bash

# This script tests a Dockerized go-joke-app by:
# 1. Building the Docker image.
# 2. Using Keploy to record interactions in two separate sessions.
# 3. Using Keploy to run tests with the captured mocks.
#
# This script incorporates best practices for CI automation, including:
# - Robust error handling with `trap`.
# - Collapsible log sections for readability.
# - Precise process management using PIDs.

set -Eeuo pipefail

# --- Source Common Scripts & Perform Sanity Checks ---
[ -x "${RECORD_BIN:-}" ] || { echo "::error::RECORD_BIN not set or not executable"; exit 1; }
[ -x "${REPLAY_BIN:-}" ] || { echo "::error::REPLAY_BIN not set or not executable"; exit 1; }
command -v docker >/dev/null 2>&1 || { echo "::error::docker not found"; exit 1; }
command -v curl >/dev/null 2>&1 || { echo "::error::curl not found"; exit 1; }

# --- Helper Functions for Logging & Error Handling ---
section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

# Final cleanup routine to ensure containers and processes are stopped.
cleanup() {
    section "Final Cleanup"
    echo "Stopping any running Docker containers..."
    docker stop $(docker ps -q --filter "name=jokeApp") 2>/dev/null || true
    echo "Stopping any running Keploy processes..."
    pkill -f keploy || true
    sleep 2
    pkill -9 -f keploy || true
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
    sleep 2
  done
  echo "::error::Application did not become available on port $port in time."
  endsec
  return 1
}

# Sends HTTP requests to the application to generate traffic for recording.
generate_traffic() {
    section "Generating HTTP Traffic"
    echo "Sending GET requests to http://localhost:8080/joke..."
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

container_kill() {
    pid=$(pgrep -n keploy)
    echo "$pid Keploy PID" 
    echo "Killing keploy"
    sudo kill $pid
}

# --- Main Execution Logic ---

section "Build Docker Image"
docker build -t go-joke-app .
endsec

section "Setup & Configuration"
sudo -E env PATH=$PATH $RECORD_BIN config --generate
sudo rm -rf keploy/
endsec

# --- 1. Record Application Interactions ---
section "Start Recording"
# Start Keploy record in the background, wrapping the Docker command.
sudo -E env PATH=$PATH $RECORD_BIN record -c "docker run -p8080:8080 --rm --name jokeApp --network keploy-network go-joke-app" --container-name jokeApp > record.log 2>&1 &
KEPLOY_PID=$!
echo "Keploy record process started with PID: $KEPLOY_PID for container jokeApp"
endsec

wait_for_http 8080
generate_traffic

echo "Waiting for recordings to be flushed..."
sleep 5
endsec

section "Stop Recording"
echo "Stopping Keploy record process (PID: $KEPLOY_PID)..."
container_kill
wait "$KEPLOY_PID" || true # Wait for it to exit, ignore error if already gone.
echo "Recording stopped."
check_for_errors record.log
endsec

# --- 2. Test Application with Captured Mocks ---
section "Start Testing with Mocks"
test_container="jokeApp_test"
# The test command will fail if tests fail, and the `trap` will catch it.
sudo -E env PATH=$PATH $REPLAY_BIN test -c "docker run -p8080:8080 --rm --name $test_container --network keploy-network go-joke-app" --containerName "$test_container" --apiTimeout 60 --delay 20 --generate-github-actions=false > test.log 2>&1
echo "Test run finished."
endsec

# --- 3. Validate Test Results ---
check_for_errors "test.log"
check_test_report

# --- Final Result ---
echo "✅ All tests passed successfully!"
exit 0
