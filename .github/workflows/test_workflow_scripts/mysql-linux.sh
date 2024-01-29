#!/bin/bash

# Source the test-iid.sh script
source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

# Checkout the 'native-linux' branch
git fetch origin
git checkout native-linux

# Start MySQL container
docker run -p 3306:3306 --rm --name mysql -e MYSQL_ROOT_PASSWORD=my-secret-pw -d mysql:latest

# Check and remove existing keploy-config file
if [ -f "./keploy-config.yaml" ]; then
    rm ./keploy-config.yaml
fi

# Generate the keploy-config file
./../../keployv2 generate-config

# Update the global noise to ts in the keploy-config file
config_file="./keploy-config.yaml"
sed -i 's/body: {}/body: {"ts":[]}/' "$config_file"

# Remove any preexisting keploy tests and mocks
rm -rf keploy/

# Build the Golang binary
go build -o main

# Loop for recording test cases and mocks
for i in {1..2}; do
    # Start the app in record mode with keploy
    sudo -E env PATH="$PATH" ./../../keployv2 record -c "./main" &

    # Wait for the application to start
    app_started=false
    while [ "$app_started" = false ]; do
        if curl localhost:8080/all; then
            app_started=true
        fi
        sleep 3
    done

    # Get the PID of the keploy process
    pid=$(pgrep keploy)

    # Making curl calls to record the test cases and mocks
    curl -X POST localhost:8080/create -d '{"link":"https://google.com"}'
    curl -X POST localhost:8080/create -d '{"link":"https://facebook.com"}'
    curl localhost:8080/all

    # Wait for keploy to record the test cases and mocks
    sleep 5

    # Stop the app
    sudo kill $pid

    # Wait for keploy to stop
    sleep 5
done

# Start the app in test mode
sudo -E env PATH="$PATH" ./../../keployv2 test -c "./main" --delay 7

# Get the test results from the test report file
report_file="./keploy/testReports/test-run-1/report-1.yaml"
test_status1=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
report_file2="./keploy/testReports/test-run-1/report-1.yaml"
test_status2=$(grep 'status:' "$report_file2" | head -n 1 | awk '{print $2}')

# Exit with code 0 if tests passed, otherwise exit with code 1
if [ "$test_status1" = "PASSED" ] && [ "$test_status2" = "PASSED" ]; then
    exit 0
else
    exit 1
fi
