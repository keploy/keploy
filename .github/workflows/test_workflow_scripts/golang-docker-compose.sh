#!/bin/bash

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

# Build Docker Image
docker compose build

# Remove any preexisting keploy tests and mocks.
sudo rm -rf keploy/

mkdir -p keploy/reports

# Generate the keploy-config file.
sudo -E env PATH=$PATH $RECORD_BIN config --generate

# Update the global noise to ts in the config file.
config_file="./keploy.yml"
sed -i 's/global: {}/global: {"body": {"ts":[]}}/' "$config_file"

container_kill() {
    pid=$(pgrep -n keploy)
    echo "$pid Keploy PID"
    echo "Killing keploy"
    sudo kill $pid
}

send_request(){
    sleep 10
    app_started=false
    while [ "$app_started" = false ]; do
        if curl -X GET http://localhost:8082/health; then
            app_started=true
        fi
        sleep 3
    done
    echo "App started"
    # Make curl calls to record the test cases and mocks.
    curl --request POST \
      --url http://localhost:8082/url \
      --header 'content-type: application/json' \
      --data '{
      "url": "https://google.com"
    }'

    curl --request POST \
      --url http://localhost:8082/url \
      --header 'content-type: application/json' \
      --data '{
      "url": "https://facebook.com"
    }'

    curl -X GET http://localhost:8082/health

    # Wait for 5 seconds for keploy to record the test cases and mocks.
    sleep 5
    container_kill
    wait
}

for i in {1..2}; do
    container_name="echoApp"
    send_request &
    sudo -E env PATH=$PATH $RECORD_BIN record -c "docker compose up" --container-name "$container_name" --generateGithubActions=false &> "${container_name}.txt"

    echo "--- Start of Recording Logs for Iteration ${i} ---"
    cat "${container_name}.txt"
    echo "--- End of Recording Logs for Iteration ${i} ---"

    if grep "WARNING: DATA RACE" "${container_name}.txt"; then
        echo "Race condition detected in recording, stopping pipeline..."
        cat "${container_name}.txt"
        exit 1
    fi
    if grep "ERROR" "${container_name}.txt"; then
        echo "Error found in pipeline..."
        cat "${container_name}.txt"
        exit 1
    fi
    sleep 5

    echo "Recorded test case and mocks for iteration ${i}"
done

# Shutdown services before test mode - Keploy should use mocks for dependencies
echo "Shutting down docker compose services before test mode..."
docker compose down
echo "Services stopped - Keploy should now use mocks for dependency interactions"

# Start keploy in test mode.
test_container="echoApp"
sudo -E env PATH=$PATH $REPLAY_BIN test -c 'docker compose up' --containerName "$test_container" --apiTimeout 60 --delay 20 --generate-github-actions=false --debug &> "${test_container}.txt"

if grep "ERROR" "${test_container}.txt"; then
    echo "Error found in pipeline..."
    cat "${test_container}.txt"
    exit 1
fi

if grep "WARNING: DATA RACE" "${test_container}.txt"; then
    echo "Race condition detected in test, stopping pipeline..."
    cat "${test_container}.txt"
    exit 1
fi

all_passed=true

for i in {0..1}
do
    report_file="./keploy/reports/test-run-0/test-set-$i-report.yaml"
    echo "----------------------------------------"
    echo "Verifying report file: ${report_file}"

    # First, check if the report file actually exists
    if [ -f "$report_file" ]; then
        echo "Report file found."
        # If it exists, then grep its status
        test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
        echo "Status found: $test_status"

        if [ "$test_status" != "PASSED" ]; then
            all_passed=false
            echo "Error: Test set status was NOT 'PASSED'."
            break
        fi
    else
        echo "Error: Report file NOT FOUND at this path!"
        all_passed=false
        break
    fi
done

# Add a final debug step to see all generated files
echo "----------------------------------------"
echo "Final check of all files in the keploy directory:"
ls -lR ./keploy

# Check the overall test status and exit accordingly
if [ "$all_passed" = true ]; then
    echo "All tests passed"
    exit 0
else
    echo "Pipeline failed during verification step."
    cat "${test_container}.txt"
    exit 1
fi