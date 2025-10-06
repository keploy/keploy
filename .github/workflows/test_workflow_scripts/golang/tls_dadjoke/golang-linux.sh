#!/bin/bash
set -Eeuo pipefail

section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

echo "root ALL=(ALL:ALL) ALL" | sudo tee -a /etc/sudoers

wait_for_http() {
  local host="localhost" # Assuming localhost
  local port="$1"
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

send_requests() {
  wait_for_http 8080

  echo "Generating API calls..."
  curl -X GET http://localhost:8080/joke
  curl -X GET http://localhost:8080/joke
  echo "✅ Traffic generation complete."
}

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

# --- Main Execution ---

# Build the application
section "🟢 Building Go application..."
go build -o go-joke-app .
if [ $? -ne 0 ]; then
    echo "❌ Failed to build the application."
    exit 1
fi
chmod +x ./go-joke-app
echo "✅ Application built successfully."
endsec

# Setup Keploy configuration
section "🟢 Setting up Keploy..."
rm -rf keploy*
sudo -E env PATH="$PATH" $RECORD_BIN config --generate
if [ $? -ne 0 ]; then
    echo "❌ Failed to generate Keploy config."
    exit 1
fi
echo "✅ Keploy setup complete."
endsec

# 1️⃣ Record test cases
section "🟢 Starting to record test cases..."
generate_traffic & # Run traffic generation in the background
sudo -E env PATH="$PATH" $RECORD_BIN record -c "./go-joke-app" --generateGithubActions=false 2>&1 | tee record.log &
KEPLOY_PID=$!
echo "Keploy record process started with PID: $KEPLOY_PID"
endsec

section "🟢 Generating traffic to the application..."
send_requests
sleep 5 # Give a moment for processes to terminate cleanly
endsec

section "🟢 Stop Recording"
echo "Stopping Keploy record process (PID: $KEPLOY_PID)..."
pid=$(pgrep keploy || true) && [ -n "$pid" ] && sudo kill "$pid"
wait "$pid" 2>/dev/null || true
sleep 5
check_for_errors "record.txt"
echo "Recording stopped."
endsec

# 2️⃣ Test with captured mocks
section "🟢 Starting to test with captured mocks..."
sudo -E env PATH="$PATH" $REPLAY_BIN test -c "./go-joke-app" --delay 10 --generateGithubActions=false --disableMockUpload 2>&1 | tee test.log
check_for_errors "test.log"
check_test_report
endsec

echo "✅ All tests completed successfully."
exit 0