#!/bin/bash

set -e  # Exit immediately if a command exits with a non-zero status
set -o pipefail  # Return the exit status of the last command in the pipe that failed

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

# Start MySQL before starting Keploy.
docker run -p 3306:3306 --rm --name mysql \
    -e MYSQL_ROOT_PASSWORD=my-secret-pw \
    -e MYSQL_ROOT_HOST=% \
    -d mysql:latest

# Wait until MySQL is ready to accept connections
echo "Waiting for MySQL to be ready..."
until docker exec mysql mysqladmin ping -h "127.0.0.1" --silent; do
    echo "MySQL is not ready yet. Waiting..."
    sleep 2
done
echo "MySQL is up and running."

# Test MySQL connection
echo "Testing MySQL connection..."
docker exec mysql mysql -uroot -pmy-secret-pw -e "SELECT 1;" && echo "MySQL connection successful." || { echo "MySQL connection failed."; exit 1; }

# Optionally, display MySQL logs for further debugging
echo "Fetching MySQL logs..."
docker logs mysql

# Check if there is a keploy-config file, if there is, delete it.
if [ -f "./keploy.yml" ]; then
    rm ./keploy.yml
fi
export ConnectionString="root:my-secret-pw@tcp(localhost:3306)/mysql"

rm -rf keploy/

# Update the global noise to ts.
go build -o urlShort

send_request() {
    sleep 10
    app_started=false
    while [ "$app_started" = false ]; do
        if curl -X GET http://localhost:9090/healthcheck; then
            app_started=true
        else
            echo "Waiting for app to start..."
        fi
        sleep 3
    done

    echo "App started"
    curl -X POST http://localhost:9090/shorten -H "Content-Type: application/json" -d '{"url": "https://github.com"}'
    curl -X GET http://localhost:9090/resolve/4KepjkTT

    # Wait for 10 seconds for Keploy to record the tcs and mocks.
    sleep 10
    pid=$(pgrep keploy)
    echo "$pid Keploy PID"
    echo "Killing Keploy"
    sudo kill $pid
}

for i in {1..2}; do
    app_name="urlShort_${i}"
    send_request &
    sudo -E env PATH="$PATH" ./../../keployv2 record -c "./urlShort" --generateGithubActions=false &> "${app_name}.txt"
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

# Start the gin-mongo app in test mode.
sudo -E env PATH="$PATH" ./../../keployv2 test -c "./urlShort" --delay 7 --generateGithubActions=false &> test_logs.txt

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
