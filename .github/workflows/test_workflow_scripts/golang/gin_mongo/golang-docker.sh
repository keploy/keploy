#!/bin/bash

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

# Start mongo before starting keploy.
docker network create keploy-network
docker run --name mongoDb --rm --net keploy-network -p 27017:27017 -d mongo

# Generate the keploy-config file.
sudo -E env PATH=$PATH $RECORD_BIN config --generate

# Update the global noise to ts.
config_file="./keploy.yml"
sed -i 's/global: {}/global: {"body": {"ts":[]}}/' "$config_file"

# Remove any preexisting keploy tests and mocks.
sudo rm -rf keploy/
docker logs mongoDb &

# Start keploy in record mode.
docker build -t gin-mongo .
docker rm -f ginApp 2>/dev/null || true

container_kill() {
    pid=$(pgrep -n -f keploy || true)
    echo "$pid Keploy PID" 
    echo "Killing keploy"
    if [ -n "$pid" ]; then
        sudo kill $pid 2>/dev/null || true
    fi
    # Give Keploy time to cleanup eBPF resources properly
    sleep 5
}

# Checks a log file for critical errors, excluding expected shutdown messages
check_for_errors() {
    local logfile=$1
    echo "Checking for errors in $logfile..."
    if [ -f "$logfile" ]; then
        # Find critical Keploy errors, but exclude expected shutdown messages:
        # - "unknown error received from application" occurs during intentional container kill
        # - "failed to record" is a consequence of the above during normal shutdown
        # - "signal: terminated" is expected when we kill keploy
        if grep "ERROR" "$logfile" | grep "Keploy:" | grep -v "unknown error received from application" | grep -v "failed to record" | grep -v "signal: terminated" | grep -q .; then
            echo "::error::Critical error found in $logfile. Failing the build."
            echo "--- Failing Errors ---"
            grep "ERROR" "$logfile" | grep "Keploy:" | grep -v "unknown error received from application" | grep -v "failed to record" | grep -v "signal: terminated"
            echo "----------------------"
            return 1
        fi
        if grep -q "WARNING: DATA RACE" "$logfile"; then
            echo "::error::Race condition detected in $logfile"
            return 1
        fi
    fi
    echo "No critical errors found in $logfile."
    return 0
}

send_request(){
    sleep 10
    app_started=false
    while [ "$app_started" = false ]; do
        if curl -X GET http://localhost:8080/CJBKJd92; then
            app_started=true
        fi
        sleep 3
    done
    echo "App started"
    # Start making curl calls to record the testcases and mocks.
    curl --request POST \
      --url http://localhost:8080/url \
      --header 'content-type: application/json' \
      --data '{
      "url": "https://google.com"
    }'

    curl --request POST \
      --url http://localhost:8080/url \
      --header 'content-type: application/json' \
      --data '{
      "url": "https://facebook.com"
    }'

    curl -X GET http://localhost:8080/CJBKJd92

    # Test email verification endpoint
    curl --request GET \
      --url 'http://localhost:8080/verify-email?email=test@gmail.com' \
      --header 'Accept: application/json'

    curl --request GET \
      --url 'http://localhost:8080/verify-email?email=admin@yahoo.com' \
      --header 'Accept: application/json'

    # Wait for 5 seconds for keploy to record the tcs and mocks.
    sleep 5
    container_kill
    wait
}

for i in {1..2}; do
    container_name="ginApp_${i}"
    send_request &
    sudo -E env PATH=$PATH $RECORD_BIN record -c "docker run -p8080:8080 --net keploy-network --rm --name $container_name gin-mongo" --container-name "$container_name"    &> "${container_name}.txt"

    if ! check_for_errors "${container_name}.txt"; then
        cat "${container_name}.txt"
        exit 1
    fi
    sleep 5

    echo "Recorded test case and mocks for iteration ${i}"
done

# Shutdown mongo before test mode - Keploy should use mocks for database interactions
echo "Shutting down mongo before test mode..."
docker stop mongoDb || true
docker rm mongoDb || true
echo "MongoDB stopped - Keploy should now use mocks for database interactions"

# Start the keploy in test mode.
test_container="ginApp_test"
sudo -E env PATH=$PATH $REPLAY_BIN test -c 'docker run -p8080:8080 --net keploy-network --name ginApp_test gin-mongo' --containerName "$test_container" --apiTimeout 60 --delay 20 --generate-github-actions=false &> "${test_container}.txt"

if ! check_for_errors "${test_container}.txt"; then
    cat "${test_container}.txt"
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
    cat "${test_container}.txt"
    exit 1
fi