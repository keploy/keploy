#!/bin/bash

set -euo pipefail

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

# Build Docker image(s)
docker compose build

# Remove any preexisting keploy tests and mocks.
sudo rm -rf keploy/

# Generate the keploy-config file.
$RECORD_BIN config --generate

container_kill() {
    REC_PID="$(pgrep -n -f 'keploy record' || true)"
    echo "$REC_PID Keploy PID"
    if [ -n "$REC_PID" ]; then
        echo "Killing keploy"
        sudo kill -INT "$REC_PID" 2>/dev/null || true
    fi
}

send_request() {
    sleep 15

    app_started=false
    for _ in {1..30}; do
        if curl -fsS http://localhost:8080/companies >/dev/null 2>&1; then
            app_started=true
            break
        fi
        sleep 2
    done

    if [ "$app_started" = false ]; then
        echo "Application did not become ready on :8080"
        container_kill
        return 1
    fi

    echo "App started"
    bash ./test.sh
    bash ./test_projects.sh

    # Wait for a few seconds for keploy to flush testcases and mocks.
    sleep 5
    container_kill
    wait
}

for i in {1..2}; do
    log_file_name="http-postgres-record-${i}"
    send_request &
    $RECORD_BIN record -c "docker compose up" --container-name "api" --generateGithubActions=false |& tee "${log_file_name}.txt"

    if grep "WARNING: DATA RACE" "${log_file_name}.txt"; then
        echo "Race condition detected in recording, stopping pipeline..."
        cat "${log_file_name}.txt"
        exit 1
    fi
    if grep "ERROR" "${log_file_name}.txt"; then
        echo "Error found in pipeline..."
        cat "${log_file_name}.txt"
        exit 1
    fi

    # Ensure clean state for the next recording pass.
    docker compose down -v
    sleep 5
    echo "Recorded test case and mocks for iteration ${i}"
done

# Start keploy in test mode.
test_container="api"
$REPLAY_BIN test -c 'docker compose up' --containerName "$test_container" --apiTimeout 60 --delay 20 --generate-github-actions=false &> "${test_container}.txt"

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
    report_file="./keploy/reports/test-run-0/test-set-$i-report.yaml"
    test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')

    echo "Test status for test-set-$i: $test_status"

    if [ "$test_status" != "PASSED" ]; then
        all_passed=false
        echo "Test-set-$i did not pass."
        break
    fi
done

if [ "$all_passed" = true ]; then
    echo "All tests passed"
    exit 0
else
    cat "${test_container}.txt"
    exit 1
fi
