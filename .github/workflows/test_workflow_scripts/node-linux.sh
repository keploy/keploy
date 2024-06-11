#!/bin/bash

# Load test scripts and start MongoDB container
source ./../../.github/workflows/test_workflow_scripts/test-iid.sh
docker run --name mongoDb --rm -p 27017:27017 -d mongo

# Prepare environment
npm install
sed -i "s/mongoDb:27017/localhost:27017/" "src/db/connection.js"
rm -rf keploy/

send_request(){
    pid=$(pgrep keploy)
    sleep 10
    app_started=false
    while [ "$app_started" = false ]; do
        if curl -X GET http://localhost:8000/students; then
            app_started=true
        fi
        sleep 3 # wait for 3 seconds before checking again.
    done
    echo "App started"
    # Start making curl calls to record the testcases and mocks.
    curl --request POST --url http://localhost:8000/students --header 'content-type: application/json' --data '{"name":"John Doe","email":"john@xyiz.com","phone":"0123456799"}'
    curl --request POST --url http://localhost:8000/students --header 'content-type: application/json' --data '{"name":"Alice Green","email":"green@alice.com","phone":"3939201584"}'
    curl -X GET http://localhost:8000/students
    # Wait for 10 seconds for keploy to record the tcs and mocks.
    sleep 10
    sudo kill $pid
}

# Record and test sessions in a loop
for i in {1..2}; do
    app_name="nodeApp_${i}"
    send_request &
    sudo -E env PATH=$PATH ./../../keployv2 record -c 'npm start' --generateGithubActions=false &> "${app_name}.txt"
    if grep "WARNING: DATA RACE" "${app_name}.txt"; then
        echo "Race condition detected in recording, stopping pipeline..."
        cat "${app_name}.txt"
        exit 1
    fi
    sleep 5
    echo "Recorded test case and mocks for iteration ${i}"
done

# Test modes and result checking
sudo -E env PATH=$PATH ./../../keployv2 test -c 'npm start' --delay 10 --generateGithubActions=false &> test_logs.txt
if grep "WARNING: DATA RACE" "test_logs.txt"; then
    echo "Race condition detected in test, stopping pipeline..."
    cat "test_logs.txt"
    exit 1
fi

sudo -E env PATH=$PATH ./../../keployv2 config --generate

sed -i 's/selectedTests: {}/selectedTests: {"test-set-0": ["test-1", "test-2"]}/' "./keploy.yml"

sudo -E env PATH=$PATH ./../../keployv2 test -c 'npm start' --apiTimeout 30 --delay 10 --generateGithubActions=false &> test_logs2.txt
if grep "WARNING: DATA RACE" "test_logs2.txt"; then
    echo "Race condition detected in test, stopping pipeline..."
    cat "test_logs.txt"
    exit 1
fi

all_passed=true

for i in {0..2}
do
    report_file="./keploy/reports/test-run-$i/test-set-0-report.yaml"
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
    report_file="./keploy/reports/test-run-0/test-set-1-report.yaml"
    test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')

    # Print the status for debugging
    echo "Test status for test-set-0: $test_status"

    # Check if any test set did not pass
    if [ "$test_status" != "PASSED" ]; then
        all_passed=false
        echo "Test-set-1 did not pass."
        exit 1
    fi
    echo "All tests passed"
    exit 0
else
    exit 1
fi
