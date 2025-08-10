#!/bin/bash

# This script assumes a similar folder structure to the example provided.
# .github/workflows/test_workflow_scripts/python/django_postgres/
# Modify the source path if your structure is different.
source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

# Create a shared network for Keploy and the application containers
docker network create keploy-network || true

# Start the MySQL database using the provided docker-compose file
docker compose up -d db

# Set up environment
rm -rf keploy/ keploy.yml # Clean up old test data and config
docker build -t flask-mysql-app:1.0 .  # Build the Docker image for the app

# Configure keploy
sudo $RECORD_BIN config --generate
sed -i 's/global: {}/global: {"header": {"Allow":[]}}/' "./keploy.yml"
sleep 5  # Allow time for configuration to apply

# Function to gracefully stop the Keploy process
container_kill() {
    pid=$(pgrep -n keploy)
    echo "$pid Keploy PID" 
    echo "Killing keploy"
    sudo kill "$pid"
}

# Function to send API requests to the application
send_request(){
    local app_port=$1
    # Wait for the application to be fully started
    sleep 15
    app_started=false
    echo "Checking for app readiness on port ${app_port}..."
    while [ "$app_started" = false ]; do
        if curl --silent "http://localhost:${app_port}/"; then
            app_started=true
            echo "App is ready!"
        else
            sleep 3  # Check every 3 seconds
        fi
    done

    # 1. Login to get the JWT token
    echo "Logging in to get JWT token..."
    TOKEN=$(curl -s -X POST -H "Content-Type: application/json" \
        -d '{"username": "admin", "password": "admin123"}' \
        "http://localhost:${app_port}/login" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')

    if [ -z "$TOKEN" ]; then
        echo "Failed to retrieve JWT token. Aborting."
        container_kill
        exit 1
    fi
    echo "Token received."

    # 2. Start making curl calls to record testcases and mocks
    echo "Sending API requests..."
    curl -X POST -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" \
        -d '{"name": "Laptop Pro", "quantity": 15, "price": 1499.99, "description": "High-end laptop for professionals"}' \
        "http://localhost:${app_port}/robust-test/create"

    curl -X POST -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" \
        -d '{"name": "Wireless Mouse", "quantity": 100}' \
        "http://localhost:${app_port}/robust-test/create-with-null"

    curl -H "Authorization: Bearer $TOKEN" "http://localhost:${app_port}/robust-test/get-all"

    # Wait for 5 seconds for keploy to record the tcs and mocks.
    sleep 5
    container_kill
    wait
}

# Record sessions
for i in {1..2}; do
    container_name="flask-mysql-app-${i}"
    # Use different host ports for each recording session to avoid conflicts
    host_port=$((5000 + i - 1))
    
    send_request "$host_port" &
    # The app runs on port 5001 inside the container, as defined in demo.py
    sudo -E env PATH=$PATH $RECORD_BIN record -c "docker run -p${host_port}:5001 --env DB_HOST=simple-demo-db --net keploy-network --rm --name $container_name flask-mysql-app:1.0" --container-name "$container_name" &> "${container_name}.txt"
    
    if grep "ERROR" "${container_name}.txt"; then
        echo "Error found in pipeline..."
        cat "${container_name}.txt"
        exit 1
    fi
    if grep "WARNING: DATA RACE" "${container_name}.txt"; then
        echo "Race condition detected in recording, stopping pipeline..."
        cat "${container_name}.txt"
        exit 1
    fi
    sleep 5
    wait # Wait for the send_request background job to finish

    echo "Recorded test case and mocks for iteration ${i}"
done

# Testing phase
echo "Starting testing phase..."
test_container="flask-mysql-test"
# Map to a different port (8080) for the test run to ensure a clean environment
sudo -E env PATH=$PATH $REPLAY_BIN test -c "docker run -p8080:5001 --env DB_HOST=simple-demo-db --net keploy-network --name $test_container flask-mysql-app:1.0" --containerName "$test_container" --apiTimeout 60 --delay 20 --generate-github-actions=false &> "${test_container}.txt"

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
# The number of loops should match the number of recording sessions
for i in {0..1}; do
    report_file="./keploy/reports/test-run-0/test-set-$i-report.yaml"
    if [ ! -f "$report_file" ]; then
        echo "Report file not found: $report_file"
        all_passed=false
        break
    fi
    test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
    echo "Test status for test-set-$i: $test_status"
    if [ "$test_status" != "PASSED" ]; then
        all_passed=false
        echo "Test-set-$i did not pass."
        break 
    fi
done

# Check the overall test status and exit accordingly
if [ "$all_passed" = true ]; then
    echo "All tests passed"
    docker compose down
    exit 0
else
    echo "Some tests failed."
    cat "${test_container}.txt"
    docker compose down
    exit 1
fi
