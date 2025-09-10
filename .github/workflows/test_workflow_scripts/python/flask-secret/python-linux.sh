#!/bin/bash

source $GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/test-iid.sh

# Install dependencies
pip3 install -r requirements.txt

# Database migrations
sudo $RECORD_BIN config --generate
sudo rm -rf keploy/  # Clean old test data
sleep 5  # Allow time for configuration changes

send_request(){
    sleep 10
    app_started=false
    while [ "$app_started" = false ]; do
        if curl -X GET http://127.0.0.1:8000/; then
            app_started=true
        fi
        sleep 3 # wait for 3 seconds before checking again.
    done
    echo "App started"
    curl -s http://localhost:8000/secret1

    curl -s http://localhost:8000/secret2

    curl -s http://localhost:8000/secret3

    # Wait for 10 seconds for keploy to record the tcs and mocks.
    sleep 10
    pid=$(pgrep keploy)
    echo "$pid Keploy PID" 
    echo "Killing keploy"
    sudo kill $pid
}

# Record and Test cycles
for i in {1..2}; do
    app_name="flaskSecret_${i}"
    send_request &
    sudo -E env PATH="$PATH" $RECORD_BIN record -c "python3 main.py" --metadata "x=y"  &> "${app_name}.txt"
    if grep "ERROR" "${app_name}.txt"; then
        echo "Error found in pipeline..."
        cat "${app_name}.txt"
        exit 1
    fi
    if grep "WARNING: DATA RACE" "${app_name}.txt"; then
        echo "Race condition detected in recording, stopping pipeline..."
        cat "${app_name}.txt"
        exit 1
    fi
    sleep 5
    wait
    echo "Recorded test case and mocks for iteration ${i}"
done

echo "Shutting down flask before test mode..."

# Sanitize the testcases
sudo -E env PATH="$PATH" $RECORD_BIN sanitize

sleep 5

# Testing phase
sudo -E env PATH="$PATH" $REPLAY_BIN test -c "python3 main.py" --delay 10    &> test_logs.txt

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

for i in {0..1}
do
    # Define the report file for each test set
    report_file="./keploy/reports/test-run-0/test-set-$i-report.yaml"

    # Extract the test status
    test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')

    # Print the status for debugging
    echo "Test status for test-set-$i: $test_status"

    # Check if any test set did not pass
    if [ "$test_status" != "PASSED" ]; then
        all_passed=false
        echo "Test-set-$i did not pass."
        break # Exit the loop early as all tests need to pass
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