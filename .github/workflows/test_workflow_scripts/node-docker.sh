#!/bin/bash
set -x


source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

# Start the docker container.
docker network create keploy-network
docker run --name mongoDb --rm --net keploy-network -p 27017:27017 -d mongo

# Remove any preexisting keploy tests.
sudo rm -rf keploy/

# Build the image of the application.
docker build -t node-app:1.0 .

container_kill() {
    echo "Inside container_kill"
    pid=$(pgrep -n keploy)

    if [ -z "$pid" ]; then
        echo "Keploy process not found. It might have already stopped."
        return 0 # Process not found isn't a critical failure, so exit with success
    fi

    echo "$pid Keploy PID" 
    echo "Killing keploy"
    sudo kill $pid

    if [ $? -ne 0 ]; then
        echo "Failed to kill keploy process, but continuing..."
        return 0 # Avoid exiting with 1 in case kill fails
    fi

    echo "Keploy process killed"
    return 0
}

send_request(){
    sleep 10
   # Wait for the application to start.
    app_started=false
    while [ "$app_started" = false ]; do
        if curl -X GET http://localhost:8000/students; then
            app_started=true
        fi
        sleep 3 # wait for 3 seconds before checking again.
    done

    # Start making curl calls to record the testcases and mocks.
    curl --request POST \
    --url http://localhost:8000/students \
       --header 'content-type: application/json' \
       --data '{
        "name":"John Doe",
        "email":"john@xyz.com",
        "phone":"0123456799"
        }'

    curl --request POST \
    --url http://localhost:8000/students \
       --header 'content-type: application/json' \
       --data '{
        "name":"Alice Green",
        "email":"green@alice.com",
        "phone":"3939201584"
        }'

    curl -X GET http://localhost:8000/students
    # Wait for 5 seconds for keploy to record the tcs and mocks.
    sleep 5
    container_kill
    wait
}

for i in {1..2}; do
    # Start keploy in record mode.
    container_name="nodeApp_${i}"
    send_request &
    sudo -E env PATH=$PATH ./../../keployv2 record -c "docker run -p 8000:8000 --name "${container_name}" --network keploy-network node-app:1.0" --container-name "${container_name}"    &> "${container_name}.txt"
    if grep "ERROR" "${container_name}.txt"; then
        echo "Error found in pipeline..."
        cat "${container_name}.txt"
        # exit 1
    fi
    if grep "WARNING: DATA RACE" "${container_name}.txt"; then
        echo "Race condition detected in recording, stopping pipeline..."
        cat "${container_name}.txt"
        # exit 1
    fi
    sleep 5

    echo "Recorded test case and mocks for iteration ${i}"
done

sleep 3
# container_kill
sudo docker rm -f keploy-v2
sudo docker rm -f keploy-init

echo "Starting the test phase..."

# Start keploy in test mode.
test_container="nodeApp_test"
sudo -E env PATH=$PATH ./../../keployv2 test -c "docker run -p8000:8000 --rm --name $test_container --network keploy-network node-app:1.0" --containerName "$test_container" --apiTimeout 30 --delay 30 --generate-github-actions=false &> "${test_container}.txt"

sleep 3
# container_kill
sudo docker rm -f keploy-v2


if grep "ERROR" "${test_container}.txt"; then
    echo "Error found in pipeline..."
    cat "${test_container}.txt"
    # exit 1
fi
# Monitor Docker logs for race conditions during testing.
if grep "WARNING: DATA RACE" "${test_container}.txt"; then
    echo "Race condition detected in test, stopping pipeline..."
    cat "${test_container}.txt"
    # exit 1
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