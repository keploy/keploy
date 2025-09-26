#!/usr/bin/env bash

# macOS variant for echo-sql (docker compose). Uses BSD sed.
set -euo pipefail


# for the below shource make it such a way that if the file is not present or already present it does not error
source ./../../.github/workflows/test_workflow_scripts/test-iid-macos.sh

# Build Docker Image(s)
docker compose build

# Remove any preexisting keploy tests and mocks.
rm -rf keploy/
rm ./keploy.yml >/dev/null 2>&1 || true

# Generate the keploy-config file.
$RECORD_BIN config --generate

# Update the global noise to ts in the config file.
config_file="./keploy.yml"
if [ -f "$config_file" ]; then
  sed -i '' 's/global: {}/global: {"body": {"ts":[]}}/' "$config_file" || true
else
  echo "⚠️ Config file $config_file not found, skipping sed replace."
fi

send_request(){
    echo "Sending requests to the application..."
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

    # Wait for 3 seconds for keploy to record the test cases and mocks.
    sleep 3
    echo "Requests sent successfully."
}

for i in {1..2}; do
    container_name="echoApp"
    send_request &
   ($RECORD_BIN record -c "docker compose up" --container-name "$container_name" --generateGithubActions=false --record-timer=16s)  2>&1 | tee  "${container_name}.txt"

    if grep "WARNING: DATA RACE" "${container_name}.txt"; then
        echo "Race condition detected in recording, stopping pipeline..."
        exit 1
    fi
    if grep "ERROR" "${container_name}.txt"; then
        echo "Error found in pipeline..."
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

($REPLAY_BIN test -c 'docker compose up' --containerName "$test_container" --apiTimeout 60 --delay 10 --generate-github-actions=false || true)   2>&1 | tee "${test_container}.txt"

if grep "ERROR" "${test_container}.txt"; then
    echo "Error found in pipeline..."
    exit 1
fi

if grep "WARNING: DATA RACE" "${test_container}.txt"; then
    echo "Race condition detected in test, stopping pipeline..."
    cat "${test_container}.txt"
    exit 1
fi

cat "${test_container}.txt"  # For visibility in logs
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
        break
    fi

done

# Check the overall test status and exit accordingly
if [ "$all_passed" = true ]; then
    echo "All tests passed"
    exit 0
else
    exit 1
fi
