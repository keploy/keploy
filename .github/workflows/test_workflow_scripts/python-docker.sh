#!/bin/bash

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

# Set up environment
docker network create backend
rm -rf keploy/  # Clean up old test data
docker build -t flask-app:1.0 .  # Build the Docker image

# Configure keploy
sudo -E env PATH=$PATH ./../../keployv2 config --generate
sed -i 's/global: {}/global: {"header": {"Allow":[]}}/' "./keploy.yml"
sleep 5  # Allow time for configuration to apply

# Function to wait for the app to start
wait_for_app_to_start() {
    app_started=false
    while [ "$app_started" = false ]; do
        if curl --silent http://localhost:6000/students; then
            app_started=true
        else
            sleep 3  # Check every 3 seconds
        fi
    done
}

# Function to perform API calls
perform_api_calls() {
    curl -X POST -H "Content-Type: application/json" -d '{"student_id": "12345", "name": "John Doe", "age": 20}' http://localhost:6000/students
    curl -X POST -H "Content-Type: application/json" -d '{"student_id": "12346", "name": "Alice Green", "age": 22}' http://localhost:6000/students
    curl http://localhost:6000/students
    curl -X PUT -H "Content-Type: application/json" -d '{"name": "Jane Smith", "age": 21}' http://localhost:6000/students/12345
    curl http://localhost:6000/students
    curl -X DELETE http://localhost:6000/students/12345
}

# Record sessions
for i in {1..2}; do
    sudo -E env PATH=$PATH ./../../keployv2 record -c "docker compose up" --containerName flask-app --buildDelay 40 --generateGithubActions=false &> record_logs.txt &
    wait_for_app_to_start
    perform_api_calls
    sleep 5  # Wait for keploy to record
    docker rm -f keploy-v2 flask-app
done

# Testing phase
sudo -E env PATH=$PATH ./../../keployv2 test -c "docker compose up" --containerName flask-app --buildDelay 40 --apiTimeout 60 --delay 20 --generateGithubActions=false &> test_logs.txt

grep -q "race condition detected" test_logs.txt && echo "Race condition detected in testing, stopping tests..." && exit 1

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
