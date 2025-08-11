#!/bin/bash

# This script assumes a similar folder structure to the example provided.
# .github/workflows/test_workflow_scripts/python/django_postgres/
# Modify the source path if your structure is different.
source ../../.github/workflows/test_workflow_scripts/test-iid.sh

# Create a shared network for Keploy and the application containers
docker network create keploy-network || true


# Start the postgres database
docker compose up -d

# Install dependencies
pip3 install -r requirements.txt

# Setup environment variables for the application to connect to the Dockerized DB
export DB_HOST=127.0.0.1
export DB_PORT=3306
export DB_USER=demo
export DB_PASSWORD=demopass
export DB_NAME=demo

# Configuration and cleanup
sudo $RECORD_BIN config --generate
sudo rm -rf keploy/  # Clean old test data
config_file="./keploy.yml"
sed -i 's/global: {}/global: {"header": {"Allow":[]}}/' "$config_file"
sleep 5  # Allow time for configuration changes

send_request(){
    # Wait for the application to be fully started
    sleep 10
    app_started=false
    echo "Checking for app readiness on port 5001..."
    while [ "$app_started" = false ]; do
        # App runs on port 5001 as per demo.py
        if curl -s --head http://127.0.0.1:5001/ > /dev/null; then
            app_started=true
            echo "App is ready!"
        fi
        sleep 3 # wait for 3 seconds before checking again.
    done
    
    # 1. Login to get the JWT token
    echo "Logging in to get JWT token..."
    TOKEN=$(curl -s -X POST -H "Content-Type: application/json" \
        -d '{"username": "admin", "password": "admin123"}' \
        "http://127.0.0.1:5001/login" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')

    if [ -z "$TOKEN" ]; then
        echo "Failed to retrieve JWT token. Aborting."
        pid=$(pgrep keploy)
        sudo kill "$pid"
        exit 1
    fi
    echo "Token received."
    
    # 2. Start making curl calls to record the testcases and mocks.
    echo "Sending API requests..."
    curl -X POST -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" \
        -d '{"name": "Keyboard", "quantity": 50, "price": 75.00, "description": "Mechanical keyboard"}' \
        'http://127.0.0.1:5001/robust-test/create'

    curl -X POST -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" \
        -d '{"name": "Webcam", "quantity": 30}' \
        'http://127.0.0.1:5001/robust-test/create-with-null'

    curl -H "Authorization: Bearer $TOKEN" 'http://127.0.0.1:5001/robust-test/get-all'
    
    # Wait for 10 seconds for keploy to record the tcs and mocks.
    sleep 10
    pid=$(pgrep keploy)
    echo "$pid Keploy PID" 
    echo "Killing keploy"
    sudo kill "$pid"
}

# Record and Test cycles
for i in {1..2}; do
    app_name="flask-mysql-app-native-${i}"
    send_request &
    # Pass necessary environment variables to the recording session
    sudo -E env PATH="$PATH" DB_HOST=$DB_HOST DB_PORT=$DB_PORT DB_USER=$DB_USER DB_PASSWORD=$DB_PASSWORD DB_NAME=$DB_NAME $RECORD_BIN record -c "python3 demo.py" &> "${app_name}.txt"
    
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
    wait # Wait for send_request to finish
    echo "Recorded test case and mocks for iteration ${i}"
done


# Sanity: ensure we actually have recorded tests before starting attempts
if [ ! -d "./keploy/tests" ]; then
  echo "No recorded tests found in ./keploy/tests. Did recording succeed?"
  ls -la ./keploy || true
  exit 1
fi

echo "Starting testing phase with up to 5 attempts..."

for attempt in {1..5}; do
    echo "--- Test Attempt ${attempt}/5 ---"

    # Reset database state for a clean test environment before each attempt
    echo "Resetting database state for attempt ${attempt}..."
    docker compose down
    docker compose up -d

    # Wait for MySQL to be ready
    echo "Waiting for DB on 127.0.0.1:${DB_PORT}..."
    db_ready=false
    for i in {1..30}; do
        if nc -z 127.0.0.1 "${DB_PORT}" 2>/dev/null; then
            echo "DB port is open."
            db_ready=true
            break
        fi
        sleep 2
    done

    if [ "$db_ready" = false ]; then
        echo "DB failed to become ready for attempt ${attempt}. Retrying..."
        continue # Skip to the next attempt
    fi

    sleep 10 # Extra wait time for DB initialization

    # Run the test for the current attempt
    log_file="test_logs_attempt_${attempt}.txt"
    echo "Running Keploy test for attempt ${attempt}, logging to ${log_file}"

    set +e
    sudo -E env PATH="$PATH" \
      DB_HOST=$DB_HOST DB_PORT=$DB_PORT DB_USER=$DB_USER DB_PASSWORD=$DB_PASSWORD DB_NAME=$DB_NAME \
      "$REPLAY_BIN" test -c "python3 demo.py" --delay 20 &> "${log_file}"
    TEST_EXIT_CODE=$?
    set -e

    echo "Keploy test (attempt ${attempt}) exited with code: $TEST_EXIT_CODE"
    echo "----- Keploy test logs (attempt ${attempt}) -----"
    cat "${log_file}"
    echo "-------------------------------------------"

    # Check for success conditions: exit code 0 AND no 'ERROR' or 'DATA RACE' strings.
    # `grep -q` returns 0 if found, 1 if not. We want the check to succeed if grep returns 1.
    if [ $TEST_EXIT_CODE -eq 0 ] && ! grep -q "ERROR" "${log_file}" && ! grep -q "WARNING: DATA RACE" "${log_file}"; then
        echo "✅ Test Attempt ${attempt} Succeeded! No errors found."
        docker compose down
        exit 0 # Exit the entire script successfully
    else
        echo "❌ Test Attempt ${attempt} Failed."
        if [ "$attempt" -lt 5 ]; then
            echo "Retrying..."
            sleep 5 # Wait a bit before the next loop
        fi
    fi
done

# If the loop completes, all attempts have failed.
echo "🔴 All 5 test attempts failed. Exiting with failure."
docker compose down
exit 1
