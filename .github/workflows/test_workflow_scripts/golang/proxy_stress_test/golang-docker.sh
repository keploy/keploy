#!/bin/bash

# Proxy Stress Test — e2e regression test for proxy performance fixes.
# Tests concurrent HTTPS through CONNECT tunnel, large PG DataRow
# responses, and HTTP MatchType validation.

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

docker compose build

sudo rm -rf keploy/

$RECORD_BIN config --generate

container_kill() {
    REC_PID="$(pgrep -n -f 'keploy record' || true)"
    echo "Keploy record PID: $REC_PID"
    sudo kill -INT "$REC_PID" 2>/dev/null || true
}

send_request(){
    sleep 10
    app_started=false
    max_attempts=40
    attempt=0
    while [ "$app_started" = false ]; do
        if curl -sf --max-time 5 http://localhost:8080/health > /dev/null 2>&1; then
            app_started=true
        fi
        attempt=$((attempt + 1))
        if [ "$attempt" -ge "$max_attempts" ]; then
            echo "App failed to start after $max_attempts attempts"
            exit 1
        fi
        sleep 3
    done
    echo "App started"

    curl -sf --max-time 120 http://localhost:8080/api/transfer
    echo ""
    curl -sf --max-time 120 http://localhost:8080/api/batch-transfer
    echo ""
    curl -sf --max-time 30 http://localhost:8080/api/post-transfer
    echo ""
    curl -sf --max-time 5 http://localhost:8080/health

    sleep 5
    container_kill
    wait
}

container_name="proxyStressApp"

for i in {1..2}; do
    send_request &
    $RECORD_BIN record -c "docker compose up" --container-name "$container_name" --generateGithubActions=false |& tee "${container_name}.txt"

    if grep "WARNING: DATA RACE" "${container_name}.txt"; then
        echo "Race condition detected in recording, stopping pipeline..."
        cat "${container_name}.txt"
        exit 1
    fi
    sleep 5

    echo "Recorded test case and mocks for iteration ${i}"
done

# Shutdown services before test mode
echo "Shutting down docker compose services before test mode..."
docker compose down
echo "Services stopped"

# Start keploy in test mode
test_container="proxyStressApp"
$REPLAY_BIN test -c 'docker compose up' --containerName "$test_container" --apiTimeout 60 --delay 15 --generate-github-actions=false &> "${test_container}.txt"

if grep "WARNING: DATA RACE" "${test_container}.txt"; then
    echo "Race condition detected in test, stopping pipeline..."
    cat "${test_container}.txt"
    exit 1
fi

all_passed=true

for i in {0..1}
do
    report_file="./keploy/reports/test-run-0/test-set-$i-report.yaml"

    if [ ! -f "$report_file" ]; then
        echo "Report file not found: $report_file"
        echo "=== Replay output ==="
        cat "${test_container}.txt"
        all_passed=false
        continue
    fi

    test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
    echo "Test status for test-set-$i: $test_status"

    if [ "$test_status" != "PASSED" ]; then
        all_passed=false
    fi
done

if [ "$all_passed" = false ]; then
    echo "Some proxy stress test sets FAILED"
    cat "${test_container}.txt"
    exit 1
fi

echo "All proxy stress tests PASSED"
