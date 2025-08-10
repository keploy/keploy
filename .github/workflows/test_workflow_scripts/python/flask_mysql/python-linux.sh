#!/bin/bash 

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

echo "Resetting database state for a clean test environment..."
docker compose down
docker compose up -d
# Add a delay to ensure the database is fully initialized before starting the test
sleep 5

# Testing phase
echo "Starting testing phase..."
sudo -E env PATH="$PATH" DB_HOST=$DB_HOST DB_PORT=$DB_PORT DB_USER=$DB_USER DB_PASSWORD=$DB_PASSWORD DB_NAME=$DB_NAME $REPLAY_BIN test -c "python3 demo.py" --delay 10 &> test_logs.txt


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
# The number of loops should match the number of recording sessions
for i in {0..1}; do
    report_file="./keploy/reports/test-run-0/test-set-$i-report.yaml"
     if [ ! -f "$report_file" ]; then
        echo "Report file not found: $report_file"
        all_passed=false
        break
    fi
    # Get the status, which could be PASSED, FAILED, etc.
    test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
    echo "Test status for test-set-$i: $test_status"
    
    # Fail the build only if the status is explicitly FAILED.
    if [ "$test_status" == "FAILED" ]; then
        all_passed=false
        echo "Test-set-$i has FAILED."
        break
    fi
done

# Check the overall test status and exit accordingly
if [ "$all_passed" = true ]; then
    echo "All tests passed (or were ignored). Build successful."
    docker compose down
    exit 0
else
    echo "Some test sets failed."
    cat "test_logs.txt"
    docker compose down
    exit 1
fi