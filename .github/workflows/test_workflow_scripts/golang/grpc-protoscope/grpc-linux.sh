#!/bin/bash

# This script tests the grpc-protoscope sample application.
# Keploy wraps the server to record incoming gRPC calls.
# The client is a one-shot process: it sends one Search RPC and exits.
#
# Expects:
#   RECORD_BIN          -> path to keploy record binary (env)
#   REPLAY_BIN          -> path to keploy test binary   (env)

set -Eeuo pipefail

echo "root ALL=(ALL:ALL) ALL" | sudo tee -a /etc/sudoers

# --- Sanity Checks ---
[ -x "${RECORD_BIN:-}" ] || { echo "RECORD_BIN not set or not executable"; exit 1; }
[ -x "${REPLAY_BIN:-}" ] || { echo "REPLAY_BIN not set or not executable"; exit 1; }
command -v go >/dev/null 2>&1 || { echo "go not found"; exit 1; }

# --- Build Application ---
echo "Building gRPC server and client binaries..."
go build -o grpc-server ./server
go build -o grpc-client ./client
chmod +x ./grpc-server ./grpc-client

# --- Helper Functions ---

cleanup() {
    echo "Cleaning up running processes..."
    pkill -f keploy || true
    pkill -f grpc-server || true
    pkill -f grpc-client || true
    sleep 2
    pkill -9 -f keploy || true
    pkill -9 -f grpc-server || true
    pkill -9 -f grpc-client || true
    echo "Cleanup complete."
}
trap cleanup EXIT

check_for_errors() {
  local logfile=$1
  echo "Checking for errors in $logfile..."
  if [ -f "$logfile" ]; then
    if awk '/ERROR/ && !/Unsupported DNS query type/ { exit 0 } END { exit 1 }' "$logfile"; then
        echo "Error found in $logfile"
        cat "$logfile"
        exit 1
    fi
    if grep -q "WARNING: DATA RACE" "$logfile"; then
      echo "Race condition detected in $logfile"
      cat "$logfile"
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

wait_for_port() {
    local port=$1
    echo "Waiting for port $port to be open..."
    for i in {1..15}; do
        if sudo lsof -i :$port -sTCP:LISTEN >/dev/null 2>&1; then
            echo "Port $port is open."
            return 0
        fi
        echo "Port $port not yet open, retrying in 2 seconds..."
        sleep 2
    done
    echo "Timed out waiting for port $port."
    sudo lsof -i -P -n | grep LISTEN
    exit 1
}

kill_keploy_process() {
    REC_PID=$(pgrep keploy | sort -n | head -1)
    echo "$REC_PID Keploy PID"
    echo "Killing keploy"
    sudo kill -INT "$REC_PID" 2>/dev/null || true
}

# --- Main Logic ---

rm -rf ./keploy*
"$RECORD_BIN" config --generate

# Record: Keploy wraps the server to capture incoming gRPC calls.
echo "🧪 Recording gRPC server with Keploy..."
"$RECORD_BIN" record -c "./grpc-server" --generateGithubActions=false 2>&1 | tee record.log &
wait_for_port 50051

sleep 3

# The client sends one Search RPC and exits.
./grpc-client &> client.log
echo "Client finished sending gRPC request."

sleep 10

kill_keploy_process

sleep 5

check_for_errors record.log

# Replay: Keploy replays the captured gRPC calls against the server.
echo "🧪 Replaying recorded tests..."
"$REPLAY_BIN" test -c "./grpc-server" --generateGithubActions=false --disableMockUpload 2>&1 | tee test.log || true

check_for_errors test.log
if ! check_test_report; then
    echo "Test report check failed."
    cat test.log
    exit 1
fi
echo "✅ grpc-protoscope tests passed."
