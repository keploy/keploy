#!/bin/bash
set -euo pipefail

# Load helper script
source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

# --- Setup ---
NETWORK="keploy-network"
MONGO_CONTAINER="mongo"
APP_IMAGE="flask-app:1.0"
RECORD_ITERATIONS=2
REPORT_DIR="./keploy/reports/test-run-0"

# Ensure cleanup on exit
cleanup() {
    echo "Cleaning up..."
    docker stop "$MONGO_CONTAINER" >/dev/null 2>&1 || true
    docker rm "$MONGO_CONTAINER" >/dev/null 2>&1 || true
    docker network rm "$NETWORK" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# --- Start MongoDB ---
docker network create "$NETWORK" || true
docker run --name "$MONGO_CONTAINER" --rm --net "$NETWORK" -p 27017:27017 -d mongo

# --- Prepare environment ---
rm -rf keploy/                     # remove old test data
docker build -t "$APP_IMAGE" .     # build docker image

# Update keploy.yml config
sed -i 's/global: {}/global: {"header": {"Allow":[]}}/' "./keploy.yml"

# --- Helpers ---
container_kill() {
    pid=$(pgrep -n keploy || true)
    if [ -n "$pid" ]; then
        echo "Killing keploy (PID: $pid)"
        sudo kill "$pid" || true
    fi
}

send_request() {
    local container_name=$1
    echo "Waiting for $container_name to start..."
    for i in {1..10}; do
        if curl --silent http://localhost:6000/students >/dev/null; then
            echo "$container_name is up."
            break
        fi
        sleep 3
    done

    echo "Sending test traffic to $container_name..."
    curl -s -X POST -H "Content-Type: application/json" \
        -d '{"student_id": "12345", "name": "John Doe", "age": 20}' http://localhost:6000/students
    curl -s -X POST -H "Content-Type: application/json" \
        -d '{"student_id": "12346", "name": "Alice Green", "age": 22}' http://localhost:6000/students
    curl -s http://localhost:6000/students
    curl -s -X PUT -H "Content-Type: application/json" \
        -d '{"name": "Jane Smith", "age": 21}' http://localhost:6000/students/12345
    curl -s http://localhost:6000/students
    curl -s -X DELETE http://localhost:6000/students/12345

    sleep 5
    container_kill
}

check_logs() {
    local logfile=$1
    if grep -q "ERROR" "$logfile"; then
        echo "Error detected in $logfile"
        cat "$logfile"
        exit 1
    fi
    if grep -q "WARNING: DATA RACE" "$logfile"; then
        echo "Race condition detected in $logfile"
        cat "$logfile"
        exit 1
    fi
}

# --- Recording Phase ---
for i in $(seq 1 "$RECORD_ITERATIONS"); do
    container_name="flaskApp_${i}"
    logfile="${container_name}.txt"

    send_request "$container_name" &

    echo "Recording iteration $i..."
    sudo -E env PATH=$PATH $RECORD_BIN record \
        -c "docker run -p6000:6000 --net $NETWORK --rm --name $container_name $APP_IMAGE" \
        --container-name "$container_name" &> "$logfile"

    check_logs "$logfile"
    echo "Recorded test case and mocks for iteration $i"
done

# --- Shutdown Mongo ---
echo "Stopping MongoDB before replay..."
docker stop "$MONGO_CONTAINER" || true
docker rm "$MONGO_CONTAINER" || true

# --- Test Phase ---
test_container="flaskApp_test"
test_log="${test_container}.txt"

echo "Replaying tests..."
sudo -E env PATH=$PATH $REPLAY_BIN test \
    -c "docker run -p8080:8080 --net $NETWORK --name $test_container $APP_IMAGE" \
    --containerName "$test_container" \
    --apiTimeout 60 --delay 20 --generate-github-actions=false &> "$test_log"

check_logs "$test_log"

# --- Verify Reports ---
all_passed=true
for i in $(seq 0 $((RECORD_ITERATIONS-1))); do
    report_file="$REPORT_DIR/test-set-$i-report.yaml"
    if [ ! -f "$report_file" ]; then
        echo "Missing report: $report_file"
        all_passed=false
        break
    fi

    test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
    echo "Test-set-$i status: $test_status"

    if [ "$test_status" != "PASSED" ]; then
        echo "Test-set-$i did not pass"
        all_passed=false
        break
    fi
done

# --- Final Result ---
if [ "$all_passed" = true ]; then
    echo "All tests PASSED"
    exit 0
else
    echo "Some tests FAILED"
    cat "$test_log"
    exit 1
fi
