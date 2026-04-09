#!/bin/bash

# Proxy Stress Test — e2e regression test for:
# 1. TLS cert caching (concurrent HTTPS through CONNECT tunnel)
# 2. Large PostgreSQL DataRow responses (wire protocol reassembly)
# 3. HTTP POST through forward proxy (MatchType validation)
# 4. Error channel behavior under background noise (OTel, bg connections)
#
# The app container runs with CPU limit (0.5 CPU via docker-compose deploy)
# and the Keploy container is also throttled after startup. This simulates
# a resource-constrained K8s pod where cert storm and error channel bugs
# become visible.

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

docker compose build

sudo rm -rf keploy/

$RECORD_BIN config --generate

# Apply CPU limit to the Keploy container after it starts.
# Simulates the resource-constrained K8s pod where the proxy runs.
apply_keploy_cpu_limit() {
    local timeout_s=60
    local keploy_container=""
    for ((i = 1; i <= timeout_s; i++)); do
        keploy_container="$(docker ps --format '{{.Names}}' | grep '^keploy-v3' | head -n 1 || true)"
        if [ -n "$keploy_container" ]; then
            echo "Applying CPU limit (0.5) to $keploy_container"
            docker update --cpus 0.5 "$keploy_container" 2>/dev/null || true
            return 0
        fi
        sleep 1
    done
    echo "Warning: Keploy container not found for CPU throttling"
}

container_kill() {
    REC_PID="$(pgrep -n -f 'keploy record' || true)"
    echo "Keploy record PID: $REC_PID"
    sudo kill -INT "$REC_PID" 2>/dev/null || true
}

send_request(){
    sleep 15
    app_started=false
    max_attempts=30
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

    # 42 concurrent HTTPS through CONNECT tunnel + large PG rows
    curl -sf --max-time 120 http://localhost:8080/api/transfer
    echo ""

    # Batch 42 concurrent HTTPS (tests cert caching under concurrency)
    curl -sf --max-time 120 http://localhost:8080/api/batch-transfer
    echo ""

    # POST through proxy (tests HTTP MatchType validation)
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
    apply_keploy_cpu_limit &
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

# Replay — previously hung for 130s+ due to error channel saturation
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

docker compose down

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
