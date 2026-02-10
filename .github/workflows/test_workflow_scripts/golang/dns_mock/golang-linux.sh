#!/usr/bin/env bash
set -Eeuxo pipefail

# Ensure jq is installed
if ! command -v jq &> /dev/null; then
    sudo apt-get update && sudo apt-get install -y jq
fi

# --- Helper Functions ---
section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

check_for_errors() {
  local logfile=$1
  echo "Checking for errors in $logfile..."
  if [ -f "$logfile" ]; then
    if grep -q "WARNING: DATA RACE" "$logfile"; then
      echo "::error::Race condition detected in $logfile"
      return 1
    fi
    # Add other critical checks if needed
  fi
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
            echo "::error::Test set ${test_set_name} failed with status: ${test_status}"
        fi
    done

    if [ "$all_passed" = false ]; then
        echo "One or more test sets failed."
        return 1
    fi

    echo "All tests passed in reports."
    return 0
}

send_request() {
  section "Sending Requests"
  echo "Waiting for app to start..."
  for i in {1..30}; do
    if curl -s http://localhost:8086/health >/dev/null; then
      echo "App is healthy"
      break
    fi
    sleep 1
  done

  echo "Running curl.sh..."
  chmod +x ./curl.sh
  ./curl.sh || true
  endsec
}

# --- Main Execution ---

# Clean up
rm -rf keploy/ record.txt test.txt
sudo rm -f /tmp/keploy-logs.txt

# Build
section "Build App"
echo "Building app..."
go mod tidy
go build -o dns-test
endsec

# Record
section "Start Recording"
echo "Starting Recording..."
# Run keploy in background with tee
sudo -E env PATH=$PATH "$RECORD_BIN" record -c "./dns-test" --generateGithubActions=false 2>&1 | tee record.txt &
KEPLOY_PID=$!
# Wait briefly for Keploy to initialize
sleep 5
endsec

send_request

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

# Replay
section "Start Replay"
echo "Starting Replay..."
sudo -E env PATH=$PATH "$REPLAY_BIN" test -c "./dns-test" --delay 10 --generateGithubActions=false 2>&1 | tee test.txt || true
check_for_errors "test.txt"
check_test_report
endsec
