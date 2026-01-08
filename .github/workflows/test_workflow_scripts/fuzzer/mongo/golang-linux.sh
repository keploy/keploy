#!/usr/bin/env bash

# This script orchestrates the testing of the Mongo fuzzer with Keploy.
# It handles setting up a sharded, multi-cluster Mongo environment for recording,
# generating Keploy tests and mocks, and then running tests in a mock environment
# to validate Keploy's functionality.

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

  section "Stopping Mongo cluster..."
  # We are already in the correct directory, so no subshell is needed.
  docker compose down -v || true
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
  echo "No critical errors in $logfile."
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

  # Wait for the fuzzer's API to be ready on port 18082
  wait_for_http "http://localhost:18082/run" 18082

  echo "Triggering the fuzzer to generate traffic..."

  curl -X POST http://localhost:18082/run \
  -H "Content-Type: application/json" \
  -d '{
    "mode": "record",
    "seed": 424242,
    "total_ops": 50,
    "golden_path": "./golden/full_coverage.json",
    "timeout_sec": 1000,

    "use_multi_cluster": true,
    "cluster_uris": [
      "mongodb://localhost:27017",
      "mongodb://localhost:27037"
    ],
    "uri": "mongodb://localhost:27017",
    "database": "fuzztest",
    "drop_db_first": true,

    "username": "admin",
    "password": "password",
    "auth_source": "admin",
    "auth_mechanism": "SCRAM-SHA-256",

    "max_sessions": 8,
    "max_pool_size": 50,
    "min_pool_size": 5,
    "connect_timeout_sec": 10,
    "max_conn_idle_time_sec": 30,
    "max_staleness_sec": 90,
    
    "max_diffs": 20
  }'
}

# --- Main Execution Logic ---

# Initial setup and environment logging
section "Initializing Environment"
echo "MONGO_FUZZER_BIN: $MONGO_FUZZER_BIN"
echo "RECORD_KEPLOY_BIN: $RECORD_KEPLOY_BIN"
echo "REPLAY_KEPLOY_BIN: $REPLAY_KEPLOY_BIN"
rm -rf keploy/ keploy.yml golden/ record.txt test.txt go-fuzzers/
mkdir -p golden/
sudo chmod +x $MONGO_FUZZER_BIN
sudo chown -R $(whoami):$(whoami) golden
endsec

# Clone the private go-fuzzers repository and navigate to the mongo directory
section "Cloning go-fuzzers Repository"
echo "Cloning keploy/go-fuzzers using PRO_ACCESS_TOKEN..."
if [ -n "${PRO_ACCESS_TOKEN:-}" ]; then
  git clone https://${PRO_ACCESS_TOKEN}@github.com/keploy/go-fuzzers.git
else
  echo "::warning::PRO_ACCESS_TOKEN not set, attempting clone without auth (will fail for private repo)"
  git clone https://github.com/keploy/go-fuzzers.git
fi
cd go-fuzzers/mongo
echo "Now in directory: $(pwd)"
# store this path in a variable
GO_FUZZERS_PATH=$(pwd)
endsec

# Start the sharded Mongo cluster environment.
section "Starting Mongo Cluster"
chmod +x ./init_cluster.sh
./init_cluster.sh

# Generate Keploy configuration and add noise parameter
sudo "$RECORD_KEPLOY_BIN" config --generate
# --- THIS IS THE CORRECTED LINE ---
sed -i 's|globalNoise: {}|globalNoise:\n    global: { body: { "duration_ms": [] } }|' ./keploy.yml
echo "Keploy config generated and updated."
endsec

# --- Recording Phase ---
section "Start Recording Server"
# The command includes the fuzzer binary and its specific arguments
sudo -E env PATH="$PATH" "$RECORD_KEPLOY_BIN" record -c "$MONGO_FUZZER_BIN" 2>&1 | tee record.txt &
KEPLOY_PID=$!
echo "Keploy record process started with PID: $KEPLOY_PID"
endsec

section "Generate Fuzzer Traffic"
# Trigger traffic and explicitly kill the Keploy process after a delay
send_requests "$KEPLOY_PID"
# Increased sleep time to capture more of the complex, sharded operations
sleep 7
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
section "Shutting Down Mongo Cluster for Replay"
# Tear down the entire docker compose environment to ensure replay relies on mocks
docker compose down -v 
echo "✅ Mongo cluster stopped. Replay will rely on Keploy mocks."
endsec
sleep 5

# --- Replay Phase ---
section "Replaying Tests"
cd $GO_FUZZERS_PATH
# The test command must match the record command
sudo -E env PATH="$PATH" "$REPLAY_KEPLOY_BIN" test -c "$MONGO_FUZZER_BIN" --mongoPassword "password" --delay 10 --api-timeout=3000 2>&1 | tee test.txt &
TEST_PID=$!
echo "Keploy test process started with PID: $TEST_PID"
wait $TEST_PID
check_for_errors "test.txt"
check_test_report
endsec

echo "✅ All tests completed successfully."
exit 0