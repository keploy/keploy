#!/bin/bash

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

# Start mongo before starting keploy.
docker network create keploy-network
docker run --name mongo --rm --net keploy-network -p 27017:27017 -d mongo

# Set up environment
rm -rf keploy/  # Clean up old test data
docker build -t flask-app:1.0 .  # Build the Docker image

# Configure keploy
sed -i 's/global: {}/global: {"header": {"Allow":[]}}/' "./keploy.yml"
sleep 5  # Allow time for configuration to apply


container_kill() {
    # pid=$(pgrep -f "keploy record")
    # echo "$pid Keploy PID" 
    # echo "Killing keploy"
    # sudo kill $pid
    REC_PID="$(pgrep -n -f 'keploy record' || true)"
    echo "$REC_PID Keploy PID"
    echo "Killing keploy"
    sudo kill -INT "$REC_PID" 2>/dev/null || true
}

send_request(){
    local container_name=$1
    sleep 30
    app_started=false
    while [ "$app_started" = false ]; do
        if curl --silent http://localhost:6000/students; then
            app_started=true
        else
            sleep 3  # Check every 3 seconds
        fi
    done
    # Start making curl calls to record the testcases and mocks.
    curl -X POST -H "Content-Type: application/json" -d '{"student_id": "12345", "name": "John Doe", "age": 20}' http://localhost:6000/students
    curl -X POST -H "Content-Type: application/json" -d '{"student_id": "12346", "name": "Alice Green", "age": 22}' http://localhost:6000/students
    curl http://localhost:6000/students
    curl -X PUT -H "Content-Type: application/json" -d '{"name": "Jane Smith", "age": 21}' http://localhost:6000/students/12345
    curl http://localhost:6000/students
    curl -X DELETE http://localhost:6000/students/12345

    # Wait for 5 seconds for keploy to record the tcs and mocks.
    sleep 15
    container_kill
    wait
}

# Record sessions
for i in {1..2}; do
    container_name="flaskApp_${i}"
    send_request &
    sudo -E env PATH=$PATH $RECORD_BIN record -c "docker run -p 6000:6000 --net keploy-network --rm --name $container_name flask-app:1.0" --container-name "$container_name" &> "${container_name}.txt"
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

    echo "Recorded test case and mocks for iteration ${i}"
done

# Shutdown mongo before test mode - Keploy should use mocks for database interactions
echo "Shutting down mongo before test mode..."
docker stop mongo || true
docker rm mongo || true
echo "MongoDB stopped - Keploy should now use mocks for database interactions"

# Testing phase
test_container="flashApp_test"
sudo -E env PATH=$PATH $REPLAY_BIN test -c "docker run -p 6000:6000 --net keploy-network --name $test_container flask-app:1.0" --containerName "$test_container" --apiTimeout 100 --delay 15 --generate-github-actions=false --debug 
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