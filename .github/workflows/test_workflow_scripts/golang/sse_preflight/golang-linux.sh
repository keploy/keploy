#!/bin/bash

echo "$RECORD_BIN"
echo "$REPLAY_BIN"

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh
echo "iid.sh executed"

# Checkout a different branch
git fetch origin
git checkout origin/chore/add-sse-preflight-sample

# Check if there is a keploy-config file, if there is, delete it.
if [ -f "./keploy.yml" ]; then
    rm ./keploy.yml
fi

rm -rf keploy/

# Build go binaries
go build -o sse-preflight-server ./cmd/server
go build -o sse-preflight-client ./cmd/client
echo "go binaries built"

# Generate the keploy-config file.
sudo "$RECORD_BIN" config --generate

send_request() {
    sleep 6
    app_started=false
    while [ "$app_started" = false ]; do
        if curl -s http://localhost:8000/health; then
            app_started=true
        fi
        sleep 3
    done

    echo "App started"

    # Send CORS preflight (OPTIONS) via the client binary (as per README)
    ./sse-preflight-client \
        --url "http://localhost:8047/subscribe/student/events?doubtId=repro" \
        --host "doubt-service.example.com"

    # Wait for Keploy to record the tcs and mocks.
    sleep 7
    pid=$(pgrep keploy)
    echo "$pid Keploy PID"
    echo "Killing Keploy"
    sudo kill $pid
}

send_request &
"$RECORD_BIN" record -c "./sse-preflight-server" --generateGithubActions=false 2>&1 | tee record.txt
if grep "ERROR" "record.txt"; then
    echo "Error found in pipeline..."
    cat record.txt
    exit 1
fi
if grep "WARNING: DATA RACE" "record.txt"; then
    echo "Race condition detected in recording, stopping pipeline..."
    cat record.txt
    exit 1
fi
sleep 5
wait
echo "Recorded test case and mocks"

# Start the app in test mode.
"$REPLAY_BIN" test -c "./sse-preflight-server" --generateGithubActions=false 2>&1 | tee test_logs.txt

if grep "ERROR" "test_logs.txt"; then
    echo "Error found in pipeline..."
    cat "test_logs.txt"
    exit 1
fi

if grep "WARNING: DATA RACE" "test_logs.txt"; then
    echo "Race condition detected in test, stopping pipeline..."
    cat "test_logs.txt"
    exit 1
fi

all_passed=true

# Get the test results from the testReport file.
for report_file in ./keploy/reports/test-run-0/test-set-*-report.yaml; do
    [ -e "$report_file" ] || { echo "No report files found."; all_passed=false; break; }

    test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
    test_set_name=$(basename "$report_file" -report.yaml)

    echo "Test status for ${test_set_name}: $test_status"

    if [ "$test_status" != "PASSED" ]; then
        all_passed=false
        echo "${test_set_name} did not pass."
        break
    fi
done

# Check the overall test status and exit accordingly
if [ "$all_passed" = true ]; then
    echo "All tests passed"
    exit 0
else
    cat "test_logs.txt"
    exit 1
fi
