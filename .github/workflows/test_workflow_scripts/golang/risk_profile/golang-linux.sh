#!/usr/bin/env bash

# This script automates the testing of the risk profile identification feature.
# It records test cases for a sample Go application and then validates that
# the Keploy test report correctly categorizes each failure.

# --- Script Configuration and Safety ---
set -Eeuo pipefail

# --- Helper Functions for Logging and Error Handling ---

# Creates a collapsible group in the GitHub Actions log
section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

dump_logs() {
  section "Record Log"
  cat record.log 2>/dev/null || echo "Record log not found."
  endsec
  section "Test Log"
  cat test.log 2>/dev/null || echo "Test log not found."
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

  section "Stopping background processes..."
  # Terminate any running instances of the app or Keploy
  pkill -f 'keploy' || true
  pkill -f './my-app' || true
  endsec

  if [[ $rc -eq 0 ]]; then
    endsec
  fi
}

trap final_cleanup EXIT

# Checks a log file for critical Keploy errors or data races
check_for_errors() {
  local logfile=$1
  echo "Checking for errors in $logfile..."
  if [ -f "$logfile" ]; then
    if grep "ERROR" "$logfile" | grep "Keploy:"; then
      echo "::error::Critical error found in $logfile. Failing the build."
      exit 1
    fi
    if grep -q "WARNING: DATA RACE" "$logfile"; then
      echo "::error::Race condition detected in $logfile"
      exit 1
    fi
  fi
  echo "No critical errors found in $logfile."
}

# Waits for the Go application's HTTP endpoint to become available
wait_for_http() {
  local port="$1"
  section "Waiting for application on port $port..."
  for i in {1..60}; do
    if nc -z "localhost" "$port" >/dev/null 2>&1; then
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

# Validates the Keploy test report against expected risk profiles
check_test_report() {
    section "Validating Test Report..."
    
    # Define the expected risk for each API endpoint path
    declare -A expected_risks
    expected_risks["/users-low-risk"]="LOW"
    expected_risks["/users-medium-risk"]="MEDIUM"
    expected_risks["/users-medium-risk-with-addition"]="MEDIUM"
    expected_risks["/users-high-risk-type"]="HIGH"
    expected_risks["/users-high-risk-removal"]="HIGH"
    expected_risks["/status-change-high-risk"]="HIGH"
    expected_risks["/content-type-change-high-risk"]="HIGH"
    expected_risks["/header-change-medium-risk"]="MEDIUM"
    expected_risks["/noisy-header"]="PASSED" # This one should pass

    local latest_report
    latest_report=$(ls -t ./keploy/reports/test-run-*/test-set-0-report.yaml | head -n 1)
    if [ -z "$latest_report" ]; then
        echo "::error::No test report YAML found!"
        exit 1
    fi
    echo "Validating report: $latest_report"

    # Assert the summary counts
    echo "Asserting summary counts..."
    [ "$(yq '.success' "$latest_report")" == "1" ] || { echo "::error::Expected 1 successful test, found $(yq '.success' "$latest_report")"; exit 1; }
    [ "$(yq '.failure' "$latest_report")" == "8" ] || { echo "::error::Expected 8 failed tests, found $(yq '.failure' "$latest_report")"; exit 1; }
    [ "$(yq '.high-risk' "$latest_report")" == "4" ] || { echo "::error::Expected 4 high-risk failures, found $(yq '.high-risk' "$latest_report")"; exit 1; }
    [ "$(yq '.medium-risk' "$latest_report")" == "3" ] || { echo "::error::Expected 3 medium-risk failures, found $(yq '.medium-risk' "$latest_report")"; exit 1; }
    # After fixing the Content-Length bug, there should be 1 low-risk failure
    [ "$(yq '.tests[] | select(.failure_info.risk == "LOW") | .failure_info.risk' "$latest_report" | wc -l)" == "1" ] || { echo "::error::Expected 1 low-risk failure, but not found."; exit 1; }
    echo "✅ Summary counts are correct."

    # Assert each test case individually
    echo "Asserting individual test case results..."
    local tests_count
    tests_count=$(yq '.tests | length' "$latest_report")
    local validation_failed=false

    for i in $(seq 0 $((tests_count - 1))); do
        local url_path
        url_path=$(yq ".tests[$i].req.url" "$latest_report" | sed 's|http://localhost:8080||')
        local actual_status
        actual_status=$(yq ".tests[$i].status" "$latest_report")
        
        # Check if we have an expectation for this URL
        if [[ -z "${expected_risks[$url_path]+_}" ]]; then
            echo "::warning::No expectation defined for URL: $url_path. Skipping."
            continue
        fi

        local expected_outcome="${expected_risks[$url_path]}"
        echo "---"
        echo "Validating test for URL: $url_path"
        
        if [ "$expected_outcome" == "PASSED" ]; then
            if [ "$actual_status" != "PASSED" ]; then
                echo "::error::Expected status PASSED, but got $actual_status"
                validation_failed=true
            else
                echo "✅ OK: Status is PASSED as expected."
            fi
        else # Expecting a failure
            local actual_risk
            actual_risk=$(yq ".tests[$i].failure_info.risk" "$latest_report")
            
            if [ "$actual_status" != "FAILED" ]; then
                echo "::error::Expected status FAILED, but got $actual_status"
                validation_failed=true
            elif [ "$actual_risk" != "$expected_outcome" ]; then
                echo "::error::Risk mismatch! Expected: $expected_outcome, Got: $actual_risk"
                validation_failed=true
            else
                echo "✅ OK: Status is FAILED with risk '$actual_risk' as expected."
            fi
        fi
    done

    if [ "$validation_failed" = true ]; then
        echo "::error::Test report validation failed."
        exit 1
    fi

    echo "✅ All test cases in the report match their expected outcomes."
    endsec
}


# --- Main Execution Logic ---

# Prerequisite check
command -v yq >/dev/null 2>&1 || { echo "::error::'yq' is not installed. Please install it to run this script (e.g., 'sudo apt-get install yq')."; exit 1; }

section "Setup Environment"
echo "Cleaning up previous runs..."
rm -rf keploy/ my-app record.log test.log
echo "Building the Go application..."
go build -o my-app
endsec

section "Record Test Cases"
echo "Starting Keploy in record mode..."
sudo -E env PATH="$PATH" $RECORD_BIN record -c "./my-app" 2>&1 | tee record.log &
KEPLOY_PID=$!
wait_for_http 8080
endsec

section "Generating traffic using curl.sh..."
bash ./curl.sh
sleep 5
endsec

section "Stopping Keploy record process (PID: $KEPLOY_PID)..."
pid=$(pgrep keploy || true) && [ -n "$pid" ] && sudo kill "$pid"
wait "$pid" 2>/dev/null || true
sleep 5
check_for_errors "record.log"
echo "Recording stopped."
endsec

section "Run Keploy Tests"
echo "Running tests with risk profile analysis..."
sudo -E env PATH="$PATH" $REPLAY_BIN test -c "./my-app" --delay 10 --skip-coverage=false --useLocalMock 2>&1 | tee test.log
check_for_errors "test.log"
endsec

section "Validate Test Report for correct Risk Profiles"
# Validate the generated report
check_test_report
echo "✅ All tests completed successfully with correct risk profiles."
endsec


exit 0
