#!/bin/bash

source ./../../../.github/workflows/test_workflow_scripts/test-iid.sh

# Checkout to the specified branch
git fetch origin
git checkout native-linux

echo "root ALL=(ALL:ALL) ALL" | sudo tee -a /etc/sudoers

# Start the postgres database
docker compose up -d

# Install dependencies
pip3 install -r requirements.txt

# Setup environment
export PYTHON_PATH=./venv/lib/python3.10/site-packages/django

# Database migrations
python3 manage.py makemigrations
python3 manage.py migrate

# Configuration and cleanup
sudo $RECORD_BIN config --generate
sudo rm -rf keploy/  # Clean old test data
config_file="./keploy.yml"
sed -i 's/global: {}/global: {"header": {"Allow":[],}}/' "$config_file"
sleep 5  # Allow time for configuration changes

# Wait for a minimum number of test cases to be recorded
wait_for_tests() {
    local min_tests=$1
    local max_wait=${2:-60}
    local waited=0
    
    echo "Waiting for at least $min_tests test(s) to be recorded..."
    
    while [ $waited -lt $max_wait ]; do
        local test_count=0
        if [ -d "./keploy" ]; then
            test_count=$(find ./keploy -name "test-*.yaml" -path "*/tests/*" 2>/dev/null | wc -l | tr -d ' ')
        fi
        
        if [ "$test_count" -ge "$min_tests" ]; then
            echo "Found $test_count test(s) recorded."
            return 0
        fi
        
        echo "Currently $test_count test(s), waiting... ($waited/$max_wait sec)"
        sleep 5
        waited=$((waited + 5))
    done
    
    echo "Timeout waiting for tests. Only found $test_count test(s)."
    return 1
}

send_request(){
    sleep 10
    app_started=false
    while [ "$app_started" = false ]; do
        if curl -X GET http://127.0.0.1:8000/; then
            app_started=true
        fi
        sleep 3 # wait for 3 seconds before checking again.
    done
    echo "App started"
    # Start making curl calls to record the testcases and mocks.
    curl --location 'http://127.0.0.1:8000/user/' --header 'Content-Type: application/json' --data-raw '{
        "name": "Jane Smith",
        "email": "jane.smith@example.com",
        "password": "smith567",
        "website": "www.janesmith.com"
    }'
    curl --location 'http://127.0.0.1:8000/user/' --header 'Content-Type: application/json' --data-raw '{
        "name": "John Doe",
        "email": "john.doe@example.com",
        "password": "john567",
        "website": "www.johndoe.com"
    }'
    curl --location 'http://127.0.0.1:8000/user/'
    # Wait for at least 3 tests to be recorded
    wait_for_tests 3 60
    
    REC_PID="$(pgrep -n -f 'keploy record' || true)"
    echo "$REC_PID Keploy PID"
    echo "Killing keploy"
    sudo kill -INT "$REC_PID" 2>/dev/null || true
}

# Record and Test cycles
for i in {1..2}; do
    app_name="flaskApp_${i}"
    send_request &
    sudo -E env PATH="$PATH" $RECORD_BIN record -c "python3 manage.py runserver"   &> "${app_name}.txt"
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

# Shutdown postgres before test mode - Keploy should use mocks for database interactions
echo "Shutting down postgres before test mode..."
docker compose down
echo "Postgres stopped - Keploy should now use mocks for database interactions"

# Testing phase
sudo -E env PATH="$PATH" $REPLAY_BIN test -c "python3 manage.py runserver" --delay 10    &> test_logs.txt

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