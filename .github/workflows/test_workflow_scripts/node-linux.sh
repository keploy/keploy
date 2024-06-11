#!/bin/bash

# Load test scripts and start MongoDB container
source ./../../.github/workflows/test_workflow_scripts/test-iid.sh
docker run --name mongoDb --rm -p 27017:27017 -d mongo

# Prepare environment
npm install
sed -i "s/mongoDb:27017/localhost:27017/" "src/db/connection.js"
rm -rf keploy/

# Function to check app started
check_app_started() {
    while ! curl -X GET http://localhost:8000/students; do
        sleep 3
    done
}

# Function to perform API calls
perform_api_calls() {
    curl --request POST --url http://localhost:8000/students --header 'content-type: application/json' --data '{"name":"John Doe","email":"john@xyiz.com","phone":"0123456799"}'
    curl --request POST --url http://localhost:8000/students --header 'content-type: application/json' --data '{"name":"Alice Green","email":"green@alice.com","phone":"3939201584"}'
    curl -X GET http://localhost:8000/students
}

# Record and test sessions in a loop
for i in {1..2}; do
    sudo -E env PATH=$PATH ./../../keployv2 record -c 'npm start' --generateGithubActions=false &> record_logs.txt &
    check_app_started
    perform_api_calls
    sleep 5
    sudo kill $(pgrep keploy)
    sleep 5
done

# Test modes and result checking
sudo -E env PATH=$PATH ./../../keployv2 test -c 'npm start' --delay 10 --generateGithubActions=false &> test_logs.txt

grep -q "race condition detected" test_logs.txt && echo "Race condition detected in testing, stopping tests..." && exit 1

sudo -E env PATH=$PATH ./../../keployv2 config --generate
sed -i 's/selectedTests: {}/selectedTests: {"test-set-0": ["test-1", "test-2"]}/' "./keploy.yml"
sudo -E env PATH=$PATH ./../../keployv2 test -c 'npm start' --apiTimeout 30 --delay 10 --generateGithubActions=false &> test_logs2.txt

grep -q "race condition detected" test_logs2.txt && echo "Race condition detected in testing, stopping tests..." && exit 1

# Consolidate test results
report_file="./keploy/reports/test-run-0/test-set-0-report.yaml"
test_status1=$(awk '/status:/ {print $2}' $report_file)
report_file2="./keploy/reports/test-run-0/test-set-1-report.yaml"
test_status2=$(awk '/status:/ {print $2}' $report_file2)
report_file5="./keploy/reports/test-run-1/test-set-0-report.yaml"
test_status5=$(awk '/status:/ {print $2}' $report_file5)
report_file6="./keploy/reports/test-run-2/test-set-0-report.yaml"
test_status6=$(awk '/status:/ {print $2}' $report_file6)
test_total6=$(awk '/total:/ {print $2}' $report_file6)
test_failure=$(awk '/failure:/ {print $2}' $report_file6)

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
