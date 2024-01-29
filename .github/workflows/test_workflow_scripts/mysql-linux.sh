#!/bin/bash

# Source the test-iid.sh script for setting up the testing environment
source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

# Checkout the 'native-linux' branch from the git repository
git fetch origin
git checkout native-linux

# Start a MySQL container using Docker
docker run -p 3306:3306 --rm --name mysql -e MYSQL_ROOT_PASSWORD=my-secret-pw -d mysql:latest

# Export the database connection string as an environment variable
export ConnectionString="root:my-secret-pw@tcp(localhost:3306)/mysql"

# Remove the existing Keploy configuration file if it exists
if [ -f "./keploy-config.yaml" ]; then
    rm ./keploy-config.yaml
fi

# Generate a new Keploy configuration file
./../../keployv2 generate-config

# Update the Keploy configuration file to include a specific body structure
config_file="./keploy-config.yaml"
sed -i 's/body: {}/body: {"ts":[]}/' "$config_file"

# Remove any preexisting Keploy tests and mocks
rm -rf keploy/

# Build the Golang binary
go build -o main

# Loop for recording test cases and mocks with Keploy
for i in {1..2}; do
    # Start the application in record mode using Keploy
    sudo -E env PATH="$PATH" ./../../keployv2 record -c "./main" &

    # Wait for the application to start
    app_started=false
    while [ "$app_started" = false ]; do
        if curl localhost:8080/all; then
            app_started=true
        fi
        sleep 3
    done

    # Get the PID of the Keploy process
    pid=$(pgrep -f keployv2)

    # Make curl calls to record the test cases and mocks
    curl -X POST localhost:8080/create -d '{"link":"https://google.com"}'
    curl -X POST localhost:8080/create -d '{"link":"https://facebook.com"}'
    curl localhost:8080/all

    # Wait for Keploy to record the test cases and mocks
    sleep 5

    # Stop the application and Keploy process
    sudo kill $pid

    # Wait for Keploy to stop completely
    sleep 5
done

# Start the application in test mode using Keploy
sudo -E env PATH="$PATH" ./../../keployv2 test -c "./main" --delay 7

# Retrieve the test results from the Keploy test report files
report_file="./keploy/testReports/test-run-1/report-1.yaml"
test_status1=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
test_status2=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')

# Exit with code 0 if all tests passed, otherwise exit with code 1
if [ "$test_status1" = "PASSED" ] && [ "$test_status2" = "PASSED" ]; then
    exit 0
else
    exit 1
fi
