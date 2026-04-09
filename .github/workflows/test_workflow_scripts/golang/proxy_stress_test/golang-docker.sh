#!/bin/bash

# Proxy Stress Test — e2e regression test for:
# 1. TLS cert caching (concurrent HTTPS through CONNECT tunnel)
# 2. Large PostgreSQL DataRow responses (wire protocol reassembly)
# 3. HTTP POST through forward proxy (MatchType validation)
# 4. Error channel behavior under background noise
#
# This test verifies that Keploy recording and replay work correctly under
# proxy stress conditions that previously caused 322s p99 latency and hangs.

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
    sleep 15
    app_started=false
    while [ "$app_started" = false ]; do
        if curl -sf http://localhost:8080/health > /dev/null 2>&1; then
            app_started=true
        fi
        sleep 3
    done
    echo "App started"

    # Concurrent HTTPS through CONNECT tunnel + large PG rows
    curl -sf http://localhost:8080/api/transfer
    echo ""

    # Batch concurrent HTTPS (tests cert caching under concurrency)
    curl -sf http://localhost:8080/api/batch-transfer
    echo ""

    # POST through proxy (tests HTTP MatchType validation)
    curl -sf http://localhost:8080/api/post-transfer
    echo ""

    curl -sf http://localhost:8080/health

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
    if grep -q "panic:" "${container_name}.txt"; then
        echo "Panic detected during recording..."
        cat "${container_name}.txt"
        exit 1
    fi
    sleep 5

    echo "Recorded test case and mocks for iteration ${i}"
done

# Shutdown before replay
docker compose down

# Replay
test_container="proxyStressApp"
$REPLAY_BIN test -c 'docker compose up' --containerName "$test_container" --apiTimeout 60 --delay 20 --generate-github-actions=false &> "${test_container}.txt"

if grep "WARNING: DATA RACE" "${test_container}.txt"; then
    echo "Race condition detected in test, stopping pipeline..."
    cat "${test_container}.txt"
    exit 1
fi

if grep -q "panic:" "${test_container}.txt"; then
    echo "Panic detected during replay..."
    cat "${test_container}.txt"
    exit 1
fi

all_passed=true

for i in {0..1}; do
    report_file="./keploy/reports/test-run-0/test-set-$i-report.yaml"

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
