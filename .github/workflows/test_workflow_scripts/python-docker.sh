#!/bin/bash

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

# Start mongo before starting keploy.
docker network create keploy-network
docker run --name mongo --rm --net keploy-network -p 27017:27017 -d mongo

# Set up environment
rm -rf keploy/  # Clean up old test data
docker build -t flask-app:1.0 .  # Build the Docker image

# Configure keploy
sudo -E env PATH=$PATH ./../../keployv2 config --generate
sed -i 's/global: {}/global: {"header": {"Allow":[]}}/' "./keploy.yml"
sleep 5  # Allow time for configuration to apply


container_kill() {
    local container_name=$1
    docker rm -f keploy-v2
    docker rm -f "${container_name}"
}

send_request(){
    local container_name=$1
    sleep 10
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
    sleep 5
    container_kill $container_name
    wait
}

# Record sessions
for i in {1..2}; do
    container_name="flaskApp_${i}"
    send_request $container_name &
    sudo -E env PATH=$PATH ./../../keployv2 record -c "docker run -p6000:6000 --net keploy-network --rm --name ${container_name} flask-app:1.0" --containerName "${container_name}" --generateGithubActions=false &> "${container_name}.txt"

    if grep "WARNING: DATA RACE" "${container_name}.txt"; then
        echo "Race condition detected in recording, stopping pipeline..."
        cat "${container_name}.txt"
        exit 1
    fi
    sleep 5

    echo "Recorded test case and mocks for iteration ${i}"
done

# Testing phase
test_container="flashApp_test"
sudo -E env PATH=$PATH ./../../keployv2 test -c "docker run -p8080:8080 --net keploy-network --name $test_container flask-app:1.0" --containerName "{$test_container}" --apiTimeout 60 --delay 20 --generateGithubActions=false &> "${test_container}.txt"

if grep "WARNING: DATA RACE" "${test_container}.txt"; then
    echo "Race condition detected in test, stopping pipeline..."
    cat "${test_container}.txt"
    exit 1
fi

# Collect and evaluate test results
report_file="./keploy/reports/test-run-0/test-set-0-report.yaml"
test_status1=$(awk '/status:/ {print $2}' "$report_file")
report_file2="./keploy/reports/test-run-0/test-set-1-report.yaml"
test_status2=$(awk '/status:/ {print $2}' "$report_file2")

if [ "$test_status1" = "PASSED" ] && [ "$test_status2" = "PASSED" ]; then
    exit 0
else
    exit 1
fi
