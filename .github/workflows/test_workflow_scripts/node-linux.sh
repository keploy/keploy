#!/bin/bash

# Load test scripts and start MongoDB container
source ./../../.github/workflows/test_workflow_scripts/test-iid.sh
docker run --name mongoDb --rm -p 27017:27017 -d mongo

# Prepare environment
npm install
sed -i "s/mongoDb:27017/localhost:27017/" "src/db/connection.js"
rm -rf keploy/

# Check if there is a keploy-config file, if there is, delete it.
if [ -f "./keploy.yml" ]; then
    rm ./keploy.yml
fi

# Generate the keploy-config file.
sudo ./../../keployv2 config --generate

# Update the global noise to ts.
config_file="./keploy.yml"
sed -i 's/global: {}/global: {"body": {"page":""}}/' "$config_file"

send_request(){
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
    curl -X GET http://localhost:8000/get
    # Wait for 10 seconds for keploy to record the tcs and mocks.
    sleep 10
    pid=$(pgrep keploy)
    echo "$pid Keploy PID" 
    echo "Killing keploy"
    sudo kill $pid
}

# Record and test sessions in a loop
for i in {1..2}; do
    app_name="nodeApp_${i}"
    send_request &
    sudo -E env PATH=$PATH ./../../keployv2 record -c 'npm start'    &> "${app_name}.txt"
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

mocks_file="keploy/test-set-0/tests/test-5.yaml"
sed -i 's/"page":1/"page":4/' "$mocks_file"

# Test modes and result checking
sudo -E env PATH=$PATH ./../../keployv2 test -c 'npm start' --delay 10    &> test_logs1.txt

if grep "ERROR" "test_logs1.txt"; then
    echo "Error found in pipeline..."
    cat "test_logs1.txt"
    exit 1
fi
if grep "WARNING: DATA RACE" "test_logs1.txt"; then
    echo "Race condition detected in test, stopping pipeline..."
    cat "test_logs1.txt"
    exit 1
fi

sudo -E env PATH=$PATH ./../../keployv2 test -c 'npm start' --delay 10 --testsets test-set-0    &> test_logs2.txt
if grep "ERROR" "test_logs2.txt"; then
    echo "Error found in pipeline..."
    cat "test_logs2.txt"
    exit 1
fi
if grep "WARNING: DATA RACE" "test_logs2.txt"; then
    echo "Race condition detected in test, stopping pipeline..."
    cat "test_logs2.txt"
    exit 1
fi

sed -i 's/selectedTests: {}/selectedTests: {"test-set-0": ["test-1", "test-2"]}/' "./keploy.yml"

sudo -E env PATH=$PATH ./../../keployv2 test -c 'npm start' --apiTimeout 30 --delay 10    &> test_logs3.txt
if grep "ERROR" "test_logs3.txt"; then
    echo "Error found in pipeline..."
    cat "test_logs3.txt"
    exit 1
fi
if grep "WARNING: DATA RACE" "test_logs3.txt"; then
    echo "Race condition detected in test, stopping pipeline..."
    cat "test_logs3.txt"
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
        cat "test_logs${i+1}.txt"
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
        cat "test_logs1.txt"
        exit 1
    fi
    echo "All tests passed"
    exit 0
else
    exit 1
fi
