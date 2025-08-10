#!/bin/bash

source ../test_workflow_scripts/test-iid.sh

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
    echo "Waiting for application to be ready..."
    app_started=false
    # Try for 60 seconds before timing out
    for i in {1..20}; do
        if curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:8000/user/ | grep -q "200"; then
            app_started=true
            break
        fi
        sleep 3 # wait for 3 seconds before checking again.
    done

    if [ "$app_started" = false ]; then
        echo "Application failed to start in time. Exiting."
        exit 1
    fi

    echo "App started. Sending API requests..."
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
    
    echo "Waiting for Keploy to process recordings..."
    sleep 10
}

echo "=== RECORDING PHASE WITH METRICS ==="
# Record and Test cycles
for i in {1..2}; do
    app_name="flaskApp_${i}"
    
    echo "Starting Keploy record for iteration ${i}..."
    sudo /usr/bin/time -f "Record Phase ${i} - Elapsed: %e seconds, CPU: %P, Memory: %M KB" \
        -o "record_metrics_${i}.txt" \
        sudo -E env PATH="$PATH" $RECORD_BIN record -c "python3 manage.py runserver" &> "${app_name}.txt" &
    KEPLOY_PID=$!
    
    send_request
    
    # --- START OF FIX ---
    echo "Stopping Keploy (PID: $KEPLOY_PID) and related processes..."
    sudo kill $KEPLOY_PID
    
    # Forcefully kill any processes on the app and proxy ports to prevent "address in use" errors.
    # The '|| true' ensures the script doesn't fail if the port is already free.
    sudo fuser -k 8000/tcp || true
    sudo fuser -k 16789/tcp || true

    wait $KEPLOY_PID 2>/dev/null # Wait for the main Keploy process to terminate
    echo "Processes stopped for iteration ${i}."
    # --- END OF FIX ---

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
    
    # Display record metrics
    echo "=== Record Metrics for iteration ${i} ==="
    cat "record_metrics_${i}.txt"
    echo "Recorded test case and mocks for iteration ${i}"
    
    sleep 5 # Give OS time to release ports fully
done

echo "=== TESTING PHASE WITH METRICS ==="
# Testing phase with metrics
sudo /usr/bin/time -f "Test Phase - Elapsed: %e seconds, CPU: %P, Memory Peak: %M KB" \
    -o "test_metrics_detailed.txt" \
    sudo -E env PATH="$PATH" $REPLAY_BIN test -c "python3 manage.py runserver" --delay 10 &> test_logs.txt

# Display test metrics
echo "=== TEST EXECUTION METRICS ==="
cat "test_metrics_detailed.txt"

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

if [ -d "./keploy/reports/test-run-0" ]; then
    for i in {0..1}; do
        report_file="./keploy/reports/test-run-0/test-set-$i-report.yaml"

        if [ -f "$report_file" ]; then
            test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
            echo "Test status for test-set-$i: $test_status"
            if [ "$test_status" != "PASSED" ]; then
                all_passed=false
                echo "Test-set-$i did not pass."
                break
            fi
        else
            echo "Report file $report_file not found for test-set-$i."
            all_passed=false
            break
        fi
    done
else
    echo "Keploy reports directory not found. Assuming test failure."
    all_passed=false
fi

echo "=== BENCHMARK SUMMARY ==="
echo "Application: Python Django + PostgreSQL"
echo "Record Cycles: 2"
echo "Test Sets: 2"

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
