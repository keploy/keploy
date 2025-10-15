#!/usr/bin/env bash

# This script orchestrates the testing of the MySQL fuzzer with Keploy.
# It handles setting up a MySQL instance for recording, generating Keploy tests and mocks,
# and then running tests in a mock environment to validate Keploy's functionality.

# --- Script Configuration and Safety ---
set -Eeuo pipefail

# --- Helper Functions for Logging and Error Handling ---

# Creates a collapsible group in the GitHub Actions log
section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

dump_logs() {
  section "Record Log"
  cat record.txt 2>/dev/null || echo "Record log not found."
  endsec
  section "Test Log"
  cat test.txt 2>/dev/null || echo "Test log not found."
  endsec
}

final_cleanup() {
  local rc=$? # Capture the script's final exit code
  if [[ $rc -ne 0 ]]; then
    echo "::error::Pipeline failed (exit code=$rc). Dumping final logs..."
  else
    section "Script finished successfully. Dumping final logs..."
  fi
  
  dump_logs

  section "Stopping MySQL container..."
  docker stop mysql-container || true
  docker rm mysql-container || true
  endsec

  if [[ $rc -eq 0 ]]; then
    endsec
  fi
  # The script will exit with the original exit code automatically
}

trap final_cleanup EXIT

# Checks a log file for critical errors or data races
check_for_errors() {
  local logfile=$1
  echo "Checking for errors in $logfile..."
  if [ -f "$logfile" ]; then
    # Find critical Keploy errors, but exclude specific non-critical ones.
    if grep "ERROR" "$logfile" | grep "Keploy:" | grep -v "failed to read symbols, skipping coverage calculation"; then
      echo "::error::Critical error found in $logfile. Failing the build."
      # Print the specific errors that caused the failure
      echo "--- Failing Errors ---"
      grep "ERROR" "$logfile" | grep "Keploy:" | grep -v "failed to read symbols, skipping coverage calculation"
      echo "----------------------"
      exit 1
    fi
    if grep -q "WARNING: DATA RACE" "$logfile"; then
      echo "::error::Race condition detected in $logfile"
      exit 1
    fi
  fi
  echo "No critical errors found in $logfile."
}

# Validates the Keploy test report to ensure all test sets passed
check_test_report() {
    echo "Checking test reports..."
    if [ ! -d "./keploy/reports" ]; then
        echo "Test report directory not found!"
        return 1
    fi

    local latest_report_dir
    latest_report_dir=$(ls -td ./keploy/reports/test-run-* | head -n 1)
    if [ -z "$latest_report_dir" ]; then
        echo "No test run directory found in ./keploy/reports/"
        return 1
    fi
    
    local all_passed=true
    # Loop through all generated report files
    for report_file in "$latest_report_dir"/test-set-*-report.yaml; do
        [ -e "$report_file" ] || { echo "No report files found."; all_passed=false; break; }
        
        local test_set_name
        test_set_name=$(basename "$report_file" -report.yaml)
        local test_status
        test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
        
        echo "Status for ${test_set_name}: $test_status"
        if [ "$test_status" != "PASSED" ]; then
            all_passed=false
            echo "Test set ${test_set_name} did not pass."
        fi
    done

    if [ "$all_passed" = false ]; then
        echo "One or more test sets failed."
        return 1
    fi

    echo "All tests passed in reports."
    return 0
}

# Waits for the MySQL container to become ready and accept connections
wait_for_mysql() {
  section "Waiting for MySQL to become ready..."
  for i in {1..90}; do
    if docker exec mysql-container mysql -uroot -ppassword -e "SELECT 1;" >/dev/null 2>&1; then
      echo "✅ MySQL is ready."
      endsec
      return 0
    fi
    echo "Waiting for MySQL... (attempt $i/90)"
    sleep 1
  done
  echo "::error::MySQL did not become ready in the allotted time."
  endsec
  return 1
}

# Waits for an HTTP endpoint to become available
wait_for_http() {
  local host="localhost" # Assuming localhost
  local port="$2"
  section "Waiting for application on port $port..."
  for i in {1..120}; do
    # Use netcat (nc) to check if the port is open without sending app-level data
    if nc -z "$host" "$port" >/dev/null 2>&1; then
      echo "✅ Application port $port is open."
      endsec
      return 0
    fi
    sleep 1
  done
  echo "::error::Application did not become available on port $port in time."
  endsec
  return 1
}

# Triggers the fuzzer, lets it run for a short time, and then kills the Keploy process.
send_requests() {
  local kp_pid="$1"

  # Wait for the fuzzer's API to be ready
  wait_for_http "http://localhost:18080/run" 18080

  echo "Triggering the fuzzer to generate traffic..."
  curl -sS --request POST 'http://localhost:18080/run' \
    --header 'Content-Type: application/json' \
    --data-raw '{
      "dsn": "root:password@tcp(127.0.0.1:3306)/",
      "db_name": "fuzzdb",
      "drop_db_first": true,
      "seed": 42,
      "total_ops": 4000,
      "timeout_sec": 1000,
      "mode": "record",
      "golden_path": "./golden/mysqlfuzz_golden.json"
    }'
}

# --- Main Execution Logic ---

# Initial setup and environment logging
section "Initializing Environment"
echo "MYSQL_FUZZER_BIN: $MYSQL_FUZZER_BIN"
echo "RECORD_KEPLOY_BIN: $RECORD_KEPLOY_BIN"
echo "REPLAY_KEPLOY _BIN: $REPLAY_KEPLOY_BIN"
rm -rf keploy/ keploy.yml golden/ record.txt test.txt
mkdir -p golden/
sudo chmod +x $MYSQL_FUZZER_BIN
sudo chown -R $(whoami):$(whoami) golden

# Start a MySQL instance for the recording session
docker run --name mysql-container \
  -e MYSQL_ROOT_PASSWORD=password \
  -p 3306:3306 --rm -d mysql:8.0
wait_for_mysql

# Generate Keploy configuration and add noise parameter
sudo "$RECORD_KEPLOY_BIN" config --generate
sed -i 's/global: {}/global: {"body": {"duration_ms":[]}}/' ./keploy.yml
echo "Keploy config generated and updated."
endsec

# --- Recording Phase ---
section "Start Recording Server"
sudo -E env PATH="$PATH" "$RECORD_KEPLOY_BIN" record -c "$MYSQL_FUZZER_BIN" 2>&1 | tee record.txt &
KEPLOY_PID=$!
echo "Keploy record process started with PID: $KEPLOY_PID"
endsec

section "Generate Fuzzer Traffic"
# Trigger traffic and explicitly kill the Keploy process after a delay
send_requests "$KEPLOY_PID"
sleep 20
endsec

section "Stop Recording"
echo "Stopping Keploy record process (PID: $KEPLOY_PID)..."

REC_PID="$(pgrep -n -f 'keploy record' || true)"
echo "$REC_PID Keploy PID"
echo "Killing keploy"
sudo kill -INT "$REC_PID" 2>/dev/null || true

sleep 5
check_for_errors "record.txt"
echo "Recording stopped."
endsec

# --- Teardown before Replay ---
section "Shutting Down MySQL for Replay"
docker stop mysql-container || true
echo "✅ MySQL container stopped. Replay will rely on Keploy mocks."
endsec

# --- Replay Phase ---
section "Replaying Tests"
sudo -E env PATH="$PATH" "$REPLAY_KEPLOY_BIN" test -c "$MYSQL_FUZZER_BIN" --delay 15 --api-timeout=1000 2>&1 | tee test.txt
check_for_errors "test.txt"
check_test_report
endsec

echo "✅ All tests completed successfully."
exit 0
