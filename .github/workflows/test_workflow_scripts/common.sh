#!/bin/bash

# -------------------------------
# Common Utility Functions
# -------------------------------

# Exported function: Check Test Reports
check_test_report() {
    echo "Checking test reports..."
    if [ ! -d "./keploy/reports" ]; then
        echo "Test report directory not found!"
        return 1
    fi

    local latest_report_dir
    latest_report_dir=$(ls -td ./keploy/reports/test-run-* 2>/dev/null | head -n 1)
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
        test_status=$(grep -m 1 'status:' "$report_file" | awk '{print $2}')
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

# Exported function: Check for Critical Errors
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

section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }


container_kill() {
    REC_PID="$(pgrep -n -f 'keploy record' || true)"
    echo "$REC_PID Keploy PID"
    echo "Killing keploy"
    sudo kill -INT "$REC_PID" 2>/dev/null || true
}

wait_for_http2() {
  local url="$1" tries="${2:-60}"
  for _ in $(seq 1 "$tries"); do
    if curl -fsS "$url" >/dev/null; then return 0; fi
    sleep 1
  done
  return 1
}

wait_for_http() {
  local timeout="$1"   
  local port="$2"     
  local host="${3:-localhost}" 

  section "Waiting for application on port $port for up to $timeout seconds..."

  for ((i = 1; i <= timeout; i++)); do
    if nc -z "$host" "$port" >/dev/null 2>&1; then
      echo "Application port $port is open."
      endsec
      return 0
    fi
    echo "Waiting for app... (attempt $i/$timeout)"
    sleep 1
  done

  echo "::error::Application did not become available on port $port in $timeout seconds."
  endsec
  return 1
}



# Export functions for use in sourced scripts
export -f check_test_report check_for_errors section endsec container_kill wait_for_http2 