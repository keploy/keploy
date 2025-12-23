#!/bin/bash

# This script tests the go-grpc sample application in two modes:
# 'incoming': Tests the gRPC server by recording its incoming gRPC calls.
# 'outgoing': Tests the gRPC client by recording its outgoing gRPC calls.
#
# Expects:
#   MODE                -> 'incoming' or 'outgoing'   (argv[1])
#   RECORD_BIN          -> path to keploy record binary (env)
#   REPLAY_BIN          -> path to keploy test binary   (env)

set -Eeuo pipefail

MODE=${1:-incoming}

echo "root ALL=(ALL:ALL) ALL" | sudo tee -a /etc/sudoers

# --- Sanity Checks ---
[ -x "${RECORD_BIN:-}" ] || { echo "RECORD_BIN not set or not executable"; exit 1; }
[ -x "${REPLAY_BIN:-}" ] || { echo "REPLAY_BIN not set or not executable"; exit 1; }
command -v go >/dev/null 2>&1 || { echo "go not found"; exit 1; }
command -v curl >/dev/null 2>&1 || { echo "curl not found"; exit 1; }

echo "root ALL=(ALL:ALL) ALL" | sudo tee -a /etc/sudoers

# --- Build Application ---
echo "Building gRPC server and client binaries..."
go build -o grpc-server .
go build -o grpc-client ./client
chmod +x ./grpc-server ./grpc-client

# --- Helper Functions ---

# Kills all running application and keploy processes
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

# Checks a log file for critical errors or data races
check_for_errors() {
  local logfile=$1
  echo "Checking for errors in $logfile..."
  if [ -f "$logfile" ]; then
    # This command finds lines with "ERROR" but excludes the non-critical "Unsupported DNS query type" error.
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

# Validates the Keploy test report to ensure all existing test sets passed
# Args: $1 = expected number of test sets (optional)
check_test_report() {
    local expected_test_sets=${1:-0}
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
    
    # Find all test-set report files dynamically
    local report_files
    report_files=$(ls "$latest_report_dir"/test-set-*-report.yaml 2>/dev/null)
    
    if [ -z "$report_files" ]; then
        echo "No test set reports found in $latest_report_dir"
        return 1
    fi
    
    local all_passed=true
    local test_set_count=0
    
    # Loop through all existing test set reports
    for report_file in $report_files; do
        test_set_count=$((test_set_count + 1))
        local test_set_name
        test_set_name=$(basename "$report_file" | sed 's/-report.yaml//')
        
        local test_status
        test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
        
        echo "Status for $test_set_name: $test_status"
        if [ "$test_status" != "PASSED" ]; then
            all_passed=false
            echo "Test set $test_set_name did not pass."
        fi
    done

    echo "Found $test_set_count test set(s)."
    
    # If expected count specified, verify it
    if [ "$expected_test_sets" -gt 0 ] && [ "$test_set_count" -ne "$expected_test_sets" ]; then
        echo "Expected $expected_test_sets test set(s), but found $test_set_count"
        return 1
    fi
    
    if [ "$all_passed" = false ]; then
        echo "One or more test sets failed."
        return 1
    fi

    echo "All tests passed in reports."
    return 0
}

# Wait for a minimum number of test cases to be recorded
wait_for_tests() {
    local min_tests=$1
    local max_wait=${2:-60}
    local waited=0
    
    echo "Waiting for at least $min_tests test(s) to be recorded..."
    
    while [ $waited -lt $max_wait ]; do
        local test_count=0
        # Count test files across all test sets
        if [ -d "./keploy" ]; then
            test_count=$(find ./keploy -name "test-*.yaml" -path "*/tests/*" 2>/dev/null | wc -l | tr -d ' ')
        fi
        
        if [ "$test_count" -ge "$min_tests" ]; then
            echo "Found $test_count test(s) recorded."
            return 0
        fi
        
        echo "Currently $test_count test(s), waiting... ($waited/$max_wait sec)"
        sleep 5
        waited=$((waited + 5))
    done
    
    echo "Timeout waiting for tests. Only found $test_count test(s)."
    return 1
}

# Sends HTTP requests to the client to trigger gRPC calls
send_requests() {
    echo "Waiting for application's HTTP server to start..."
    for i in {1..10}; do
        if curl -s -o /dev/null -X GET http://localhost:8080/users; then
            echo "Application is ready. Sending requests..."
            # 1. POST request
            curl -s -X POST http://localhost:8080/users -H "Content-Type: application/json" -d '{"name": "test-user", "email": "test@gmail.com", "age": 20}'
            # 2. GET request
            curl -s -X GET http://localhost:8080/users
            # 3. PUT request
            curl -s -X PUT http://localhost:8080/users -H "Content-Type: application/json" -d '{"id": 1, "name": "test-user-updated", "email": "test@gmail.com", "age": 20}'
            # 4. DELETE request
            curl -s -X DELETE http://localhost:8080/users -H "Content-Type: application/json" -d '{"id": 1}'
            echo "Requests sent."
            return 0
        fi
        echo "App not ready, retrying in 3 seconds..."
        sleep 3
    done
    echo "Application failed to start on port 8080."
    exit 1
}

wait_for_port() {
    local port=$1
    echo "Waiting for port $port to be open..."
    for i in {1..15}; do
        # Use lsof to check if ANY process is listening on the port
        if sudo lsof -i :$port -sTCP:LISTEN >/dev/null 2>&1; then
            echo "Port $port is open."
            return 0
        fi
        echo "Port $port not yet open, retrying in 2 seconds..."
        sleep 2
    done
    echo "Timed out waiting for port $port."
    # List open ports for debugging before failing
    sudo lsof -i -P -n | grep LISTEN
    exit 1
}

# Kills the keploy process and waits for it to terminate
kill_keploy_process() {
    REC_PID=$(pgrep keploy | sort -n | head -1)
    echo "$REC_PID Keploy PID"
    echo "Killing keploy"
    sudo kill -INT "$REC_PID" 2>/dev/null || true
}

# --- Main Logic ---

# Reset state before each run
rm -rf ./keploy*
sudo -E env PATH="$PATH" "$RECORD_BIN" config --generate

if [ "$MODE" = "incoming" ]; then
    echo "🧪 Testing incoming gRPC requests (testing grpc-server)"
    # Record: Keploy wraps the server to capture incoming gRPC calls. The client is just a driver.
    ./grpc-client &> client_incoming.log &
    sudo -E env PATH="$PATH" "$RECORD_BIN" record -c "./grpc-server" --generateGithubActions=false 2>&1 | tee record_incoming.log &
    wait_for_port 50051

    sleep 5
    
    send_requests
    
    # Wait for at least 4 tests to be recorded (4 HTTP requests)
    wait_for_tests 4 60
    
    kill_keploy_process

    sleep 5

    check_for_errors record_incoming.log

    # Test: Keploy replays the captured gRPC calls against the server.
    sudo -E env PATH="$PATH" "$REPLAY_BIN" test -c "./grpc-server" --generateGithubActions=false  --disableMockUpload 2>&1 | tee test_incoming.log || true

    check_for_errors test_incoming.log
    if ! check_test_report; then
        echo "Test report check failed for incoming mode."
        cat test_incoming.log
        exit 1
    fi
    echo "✅ Incoming mode passed."

elif [ "$MODE" = "outgoing" ]; then
    echo "🧪 Testing outgoing gRPC requests (testing grpc-client)"
    # Record: Keploy wraps the client to capture its outgoing gRPC calls. The server is a dependency.
    ./grpc-server &> server_outgoing.log &
    wait_for_port 50051
    sudo -E env PATH="$PATH" "$RECORD_BIN" record -c "./grpc-client" --generateGithubActions=false 2>&1 | tee record_outgoing.log &

    send_requests
    
    # Wait for at least 4 tests to be recorded (4 HTTP requests)
    wait_for_tests 4 60

    kill_keploy_process
    
    sleep 5
    
    check_for_errors record_outgoing.log

    # Test: Keploy mocks the server's responses for the client. The real server is NOT run.
    sudo -E env PATH="$PATH" "$REPLAY_BIN" test -c "./grpc-client" --generateGithubActions=false --disableMockUpload 2>&1 | tee test_outgoing.log || true

    check_for_errors test_outgoing.log
    if ! check_test_report; then
        echo "Test report check failed for outgoing mode."
        cat test_outgoing.log
        exit 1
    fi
    echo "✅ Outgoing mode passed."

else
    echo "❌ Invalid mode specified: '$MODE'. Use 'incoming' or 'outgoing'."
    exit 1
fi
