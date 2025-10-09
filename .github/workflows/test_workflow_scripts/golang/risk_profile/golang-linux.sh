#!/usr/bin/env bash

# This script automates the testing of the risk profile identification feature.
# It records test cases, validates the initial failure report, then tests the
# two-stage normalization process (safe and forced), and finally confirms
# that all tests pass after forced normalization.

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
  section "Initial Test Log"
    cat test.log 2>/dev/null || echo "Initial test log not found."
  endsec
  section "Safe Normalize Log"
    cat normalize_safe.log 2>/dev/null || echo "Safe normalize log not found."
  endsec
  section "Forced Normalize Log"
    cat normalize_forced.log 2>/dev/null || echo "Forced normalize log not found."
  endsec
  section "Final Test Log"
    cat final_test.log 2>/dev/null || echo "Final test log not found."
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
  echo "Checking for critical errors in $logfile..."
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

check_report_for_risk_profiles() {
    section "validating the Keploy test report against expected risk profiles"
    
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

# Checks that the safe normalize run produced the expected warnings
check_normalize_warnings() {
    section "Validating Safe Normalize Warnings..."
    local logfile="normalize_safe.log"
    local warning_msg="failed to normalize test case.*due to a high-risk failure"
    
    echo "Checking for high-risk normalization warnings in $logfile..."
    
    local warning_count
    warning_count=$(grep -c "$warning_msg" "$logfile" || true)
    
    if [ "$warning_count" -ne 4 ]; then
        echo "::error::Expected 4 high-risk normalization warnings, but found $warning_count."
        exit 1
    fi
    
    echo "✅ Found exactly 4 high-risk normalization warnings, as expected."
    endsec
}

# --- Main Execution Logic ---

# Prerequisite check
command -v yq >/dev/null 2>&1 || { echo "::error::'yq' is not installed. Please install it to run this script (e.g., 'sudo apt-get install yq')."; exit 1; }

section "Setup Environment"
echo "Cleaning up previous runs..."
rm -rf keploy/ my-app *.log
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
endsec

section "Run Keploy Tests"
echo "Running tests with risk profile analysis..."
export KEPLOY_MODE="test"
sudo -E env PATH="$PATH" $REPLAY_BIN test -c "./my-app" --skip-coverage=false --useLocalMock 2>&1 | tee test.log
check_for_errors "test.log"
endsec

check_report_for_risk_profiles

section "Attempt Safe Normalization (Expected to Warn)"
echo "Running normalize without force flag. Expecting warnings for high-risk failures..."
sudo -E env PATH="$PATH" $REPLAY_BIN normalize 2>&1 | tee normalize_safe.log
check_for_errors "normalize_safe.log"
check_normalize_warnings
endsec

section "Run Forced Normalization (Expected to Succeed)"
echo "Running normalize with --allow-high-risk flag..."
sudo -E env PATH="$PATH" $REPLAY_BIN normalize --allow-high-risk 2>&1 | tee normalize_forced.log
check_for_errors "normalize_forced.log"
echo "Forced normalization complete. Test cases should now be updated."
endsec

section "Run Final Validation Test"
echo "Running final test run to confirm all tests now pass..."
sudo -E env PATH="$PATH" $REPLAY_BIN test -c "./my-app" --skip-coverage=false --useLocalMock 2>&1 | tee final_test.log
check_for_errors "final_test.log"
endsec

check_test_report

echo "✅ Full Risk Profile & Normalization Test Pipeline Succeeded!"
exit 0
