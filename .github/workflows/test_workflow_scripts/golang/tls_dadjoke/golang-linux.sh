#!/bin/bash
set -Eeuo pipefail

# This function is called whenever the script exits, for any reason.
cleanup() {
  # Capture the exit code of the last command that ran
  local exit_code=$?
  
  # Always close the last log group to prevent broken folding in CI UIs
  endsec 

  echo "--- Running cleanup ---"
  # Use pkill to find and kill the processes by name, which is more robust
  # The '|| true' prevents the script from failing if the process isn't found
  sudo pkill -f "keploy" || true
  sudo pkill -f "./go-joke-app" || true
  echo "Cleanup complete."

  # If the script failed (exit code is not 0), print an error message
  if [ $exit_code -ne 0 ]; then
    echo "::error::Script failed with exit code $exit_code."
  else
    echo "Script finished successfully."
  fi

  # Exit with the original exit code
  exit $exit_code
}

# Register the cleanup function to run on script EXIT
trap cleanup EXIT

section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

echo "root ALL=(ALL:ALL) ALL" | sudo tee -a /etc/sudoers

wait_for_http() {
  local host="localhost" # Assuming localhost
  local port="$1"
  section "Waiting for application on port $port..."
  for i in {1..120}; do
    if nc -z "$host" "$port" >/dev/null 2>&1; then
      echo "✅ Application port $port is open."
      endsec
      return 0
    fi
    sleep 1
  done
  echo "::error::Application did not become available on port $port in time."
  # No endsec here, failure will trigger the trap which handles it.
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
    if grep "ERROR" "$logfile" | grep "Keploy:" | grep -v "failed to read symbols, skipping coverage calculation"; then
      echo "::error::Critical error found in $logfile. Failing the build."
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

section "Building Go application..."
go build -o go-joke-app .
chmod +x ./go-joke-app
echo "✅ Application built successfully."
endsec

section "Setting up Keploy..."
rm -rf keploy*
sudo -E env PATH="$PATH" $RECORD_BIN config --generate
echo "✅ Keploy setup complete."
endsec

# 1️⃣ Record test cases
section "Starting to record test cases..."
# Run traffic generation in the background
sudo -E env PATH="$PATH" $RECORD_BIN record -c "./go-joke-app" --generateGithubActions=false 2>&1 | tee record.log &
KEPLOY_PID=$!
echo "Keploy record process started with PID: $KEPLOY_PID"
endsec

section "Generating traffic to the application..."
send_requests
sleep 5 # Give a moment for logs to flush
endsec

# 2️⃣ Stop Recording Process before testing
section "Stopping the recording process..."
# The trap will handle cleanup on failure, but for the happy path, we need to stop record before starting test.
echo "Stopping Keploy record process (PID: $KEPLOY_PID)..."
pid=$(pgrep keploy || true) && [ -n "$pid" ] && sudo kill "$pid"
wait "$pid" 2>/dev/null || true
sleep 5
check_for_errors "record.log"
echo "Recording stopped."
endsec

# 3️⃣ Test with captured mocks
section "Starting to test with captured mocks..."
sudo -E env PATH="$PATH" $REPLAY_BIN test -c "./go-joke-app" --delay 10 --generateGithubActions=false --disableMockUpload 2>&1 | tee test.log
check_for_errors "test.log"
check_test_report
endsec

echo "✅ All tests completed successfully."
# The 'trap' will run on this successful exit as well, but the exit_code will be 0.
