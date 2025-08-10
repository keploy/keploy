#!/bin/bash

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

# Checkout to the specified branch
git fetch origin
git checkout native-linux

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
    # Wait for 10 seconds for keploy to record the tcs and mocks.
    sleep 10
    pid=$(pgrep keploy)
    echo "$pid Keploy PID" 
    echo "Killing keploy"
    sudo kill $pid
}

# Record cycles (metrics collection for recording phase)
echo "=== RECORDING PHASE WITH METRICS ==="
for i in {1..2}; do
    app_name="flaskApp_${i}"
    send_request &
    
    # Record with timing metrics using /usr/bin/time
    sudo /usr/bin/time -f "Record Phase ${i} - Elapsed: %e seconds, CPU: %P, Memory: %M KB" \
        -o "record_metrics_${i}.txt" \
        sudo -E env PATH="$PATH" $RECORD_BIN record -c "python3 manage.py runserver" &> "${app_name}.txt"
    
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
    
    # Display record metrics
    echo "=== Record Metrics for iteration ${i} ==="
    cat "record_metrics_${i}.txt"
    echo "Recorded test case and mocks for iteration ${i}"
done

# Testing phase with detailed metrics collection
echo "=== TESTING PHASE WITH METRICS ==="
echo "Starting test phase with detailed metrics collection..."

# Test with comprehensive timing and resource monitoring
sudo /usr/bin/time -f "Test Phase - Elapsed: %e seconds, CPU: %P, Memory Peak: %M KB, Sys Time: %S, User Time: %U" \
    -o "test_metrics_detailed.txt" \
    sudo -E env PATH="$PATH" $REPLAY_BIN test -c "python3 manage.py runserver" --delay 10 &> test_logs.txt

# Extract and display test metrics
echo "=== TEST EXECUTION METRICS ==="
cat "test_metrics_detailed.txt"

# Check for errors in test execution
if grep "ERROR" "test_logs.txt"; then
    echo "Error found in test pipeline..."
    cat "test_logs.txt"
    exit 1
fi
if grep "WARNING: DATA RACE" "test_logs.txt"; then
    echo "Race condition detected in test, stopping pipeline..."
    cat "test_logs.txt"
    exit 1
fi

# Test results validation
echo "=== TEST RESULTS VALIDATION ==="
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

# Generate benchmark summary
echo "=== BENCHMARK SUMMARY ==="
echo "Application: Python Django + PostgreSQL"
echo "Record Cycles: 2"
echo "Test Sets: 2"

# Aggregate metrics
echo "=== AGGREGATED METRICS ==="
for i in {1..2}; do
    if [ -f "record_metrics_${i}.txt" ]; then
        echo "Record Cycle ${i}:"
        cat "record_metrics_${i}.txt"
    fi
done

if [ -f "test_metrics_detailed.txt" ]; then
    echo "Test Execution:"
    cat "test_metrics_detailed.txt"
fi

# Final status check
if [ "$all_passed" = true ]; then
    echo "=== BENCHMARK COMPLETED SUCCESSFULLY ==="
    echo "All tests passed - Application is functioning correctly"
    exit 0
else
    echo "=== BENCHMARK FAILED ==="
    echo "Some tests failed - Review test logs below:"
    cat "test_logs.txt"
    exit 1
fi