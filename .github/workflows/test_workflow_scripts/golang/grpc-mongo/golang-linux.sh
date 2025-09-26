#!/bin/bash

# Exit immediately if a command exits with a non-zero status.
set -e
# Treat unset variables as an error.
set -u
# The return value of a pipeline is the status of the last command to exit with a non-zero status,
# or zero if no command exited with a non-zero status.
set -o pipefail

echo "$RECORD_BIN"
echo "$REPLAY_BIN"

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh
echo "iid.sh executed"

# Checkout a different branch
git fetch origin

# Check if there is a keploy-config file, if there is, delete it.
if [ -f "./keploy.yml" ]; then
    rm ./keploy.yml
fi

rm -rf keploy/

docker compose up mongo -d

echo "Waiting for MongoDB to start..."
sleep 10

# Generate the keploy-config file.
echo "Generating Keploy config..."
sudo "$RECORD_BIN" config --generate

# Update the global noise to updated_at.
config_file="./keploy.yml"
echo "Updating global noise in config..."
sed -i 's/global: {}/global: {"body": {"updated_at":[]}}/' "$config_file"

send_request() {
    local index=$1  

    sleep 6

    echo "Sending request for iteration ${index}..."
    go run ./client

    # Wait for 7 seconds for Keploy to record the tcs and mocks.
    sleep 7
    pid=$(pgrep keploy)
    echo "$pid Keploy PID"
    echo "Killing Keploy"
    sudo kill $pid
}

for i in {1..2}; do
    app_name="grpc-mongo_${i}"
    echo "--- Starting Recording for iteration ${i} ---"
    sudo -E env PATH="$PATH" "$RECORD_BIN" record -c "go run ./server" --generateGithubActions=false 2>&1 | tee "${app_name}.txt"

    sleep 15

    send_request $i &
    
    # Error checking remains the same, but now it checks the file after we've already seen the output.
    if grep "ERROR" "${app_name}.txt"; then
        echo "Error found in pipeline..."
        exit 1
    fi
    if grep "WARNING: DATA RACE" "${app_name}.txt"; then
      echo "Race condition detected in recording, stopping pipeline..."
      exit 1
    fi
    sleep 5
    wait
    echo "--- Recorded test case and mocks for iteration ${i} ---"
done

docker compose down
sleep 5

echo "--- Starting Keploy Test Mode ---"
sudo -E env PATH="$PATH" "$REPLAY_BIN" test -c "go run ./server" --delay 7 --generateGithubActions=false 2>&1 | tee "test_logs.txt"

# Error checking remains the same.
if grep "ERROR" "test_logs.txt"; then
    echo "Error found in pipeline during test execution..."
    exit 1
fi
if grep "WARNING: DATA RACE" "test_logs.txt"; then
    echo "Race condition detected in test, stopping pipeline..."
    exit 1
fi

echo "--- Test execution finished. Verifying results... ---"
all_passed=true

# Get the test results from the testReport file.
for i in {0..1}
do
    # Define the report file for each test set
    report_file="./keploy/reports/test-run-0/test-set-$i-report.yaml"

    if [ ! -f "$report_file" ]; then
        echo "Error: Report file not found at $report_file"
        all_passed=false
        break
    fi

    # Extract the test status
    test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')

    # Print the status for debugging
    echo "Test status for test-set-$i: $test_status"

    # Check if any test set did not pass
    if [ "$test_status" != "PASSED" ]; then
        all_passed=false
        echo "Test-set-$i did not pass."
    fi
done

# Check the overall test status and exit accordingly
if [ "$all_passed" = true ]; then
    echo "✅ All tests passed"
    exit 0
else
    echo "❌ One or more tests failed. See logs above for details."
    echo "--- Full Test Log ---"
    cat "test_logs.txt"
    exit 1
fi