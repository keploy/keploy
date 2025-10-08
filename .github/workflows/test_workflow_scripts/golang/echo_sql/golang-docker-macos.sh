#!/usr/bin/env bash

# macOS variant for echo-sql (docker compose). Uses BSD sed.
set -euo pipefail

# Function to find available port
find_available_port() {
    local start_port=${1:-8080}
    local port=$start_port
    while lsof -i:$port >/dev/null 2>&1; do
        port=$((port + 1))
    done
    echo $port
}

# Find 4 available ports
APP_PORT=$(find_available_port 8080)
DB_PORT=$(find_available_port $((APP_PORT + 1)))
PROXY_PORT=$(find_available_port $((DB_PORT + 1)))
DNS_PORT=$(find_available_port $((PROXY_PORT + 1)))

# Generate unique container names with timestamp suffix
TIMESTAMP=$(date +%s)
APP_CONTAINER="echoApp_${TIMESTAMP}"
DB_CONTAINER="postgresDb_${TIMESTAMP}"

echo "Using ports - APP: $APP_PORT, DB: $DB_PORT, PROXY: $PROXY_PORT, DNS: $DNS_PORT"
echo "Using containers - APP: $APP_CONTAINER, DB: $DB_CONTAINER"

# Replace ports and container names in all files in current directory
echo "Updating configuration files with dynamic ports and container names..."
for file in $(find . -maxdepth 1 -type f \( -name "*.yml" -o -name "*.yaml" -o -name "*.go" -o -name "*.json" -o -name "*.sh" -o -name "*.env" -o -name "*.md" \)); do
    if [ -f "$file" ] && [ "$file" != "./golang-docker-macos.sh" ]; then
        # Replace 8082 with APP_PORT
        sed -i '' "s/8082/${APP_PORT}/g" "$file" 2>/dev/null || true
        # Replace echoApp with APP_CONTAINER
        sed -i '' "s/echoApp/${APP_CONTAINER}/g" "$file" 2>/dev/null || true
        # Replace postgresDb with DB_CONTAINER
        sed -i '' "s/postgresDb/${DB_CONTAINER}/g" "$file" 2>/dev/null || true
        # Replace 5432: with DB_PORT: in docker-compose files
        sed -i '' "s/5432:/${DB_PORT}:/g" "$file" 2>/dev/null || true
        echo "Updated $file"
    fi
done

# for the below shource make it such a way that if the file is not present or already present it does not error
# source ./../../.github/workflows/test_workflow_scripts/test-iid-macos.sh

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

container_kill() {
    pid=$(pgrep -n keploy)
    echo "$pid Keploy PID"
    echo "Killing keploy"
    sudo kill $pid
}

send_request(){
    echo "Sending requests to the application..."
    sleep 10
    app_started=false
    while [ "$app_started" = false ]; do
        if curl -X GET http://localhost:${APP_PORT}/health; then
            app_started=true
        fi
        sleep 3
    done
    echo "App started"
    # Make curl calls to record the test cases and mocks.
    curl --request POST \
      --url http://localhost:${APP_PORT}/url \
      --header 'content-type: application/json' \
      --data '{
      "url": "https://google.com"
    }'

    curl --request POST \
      --url http://localhost:${APP_PORT}/url \
      --header 'content-type: application/json' \
      --data '{
      "url": "https://facebook.com"
    }'

    curl -X GET http://localhost:${APP_PORT}/health

    # Wait for 3 seconds for keploy to record the test cases and mocks.
    sleep 3
    echo "Requests sent successfully."
}

for i in {1..2}; do
    container_name="$APP_CONTAINER"
    send_request &
    $RECORD_BIN record -c "docker compose up" --container-name "$container_name" --generateGithubActions=false --proxy-port=$PROXY_PORT --dns-port=$DNS_PORT |& tee "${container_name}.txt"

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
test_container="$APP_CONTAINER"
$REPLAY_BIN test -c 'docker compose up' --containerName "$test_container" --apiTimeout 60 --delay 10 --generate-github-actions=false --proxy-port=$PROXY_PORT --dns-port=$DNS_PORT &> tee "${test_container}.txt"

if grep "ERROR" "${test_container}.txt"; then
    echo "Error found in pipeline..."
    exit 1
fi

if grep "WARNING: DATA RACE" "${test_container}.txt"; then
    echo "Race condition detected in test, stopping pipeline..."
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
