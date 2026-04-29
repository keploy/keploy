#!/bin/bash

# Proxy Stress Test — e2e validation for all 4 proxy performance fixes:
# 1. TLS cert caching: 20 concurrent HTTPS through CONNECT tunnel
# 2. Error channel drain: OTel enabled with no collector
# 3. PG DataRow reassembly: queries returning 100KB+ rows
# 4. HTTP MatchType: POST through forward proxy

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

docker compose build
sudo rm -rf keploy/
$RECORD_BIN config --generate

container_kill() {
    REC_PID="$(pgrep -n -f "$(basename "${RECORD_BIN:-keploy}") record" || true)"
    echo "Keploy record PID: $REC_PID"
    sudo kill -INT "$REC_PID" 2>/dev/null || true
}

send_request(){
    sleep 10
    max_attempts=40
    attempt=0
    while ! curl -sf --max-time 5 http://localhost:8080/health > /dev/null 2>&1; do
        attempt=$((attempt + 1))
        if [ "$attempt" -ge "$max_attempts" ]; then
            echo "App failed to start"
            exit 1
        fi
        sleep 3
    done
    echo "App started"
    curl -sf --max-time 120 http://localhost:8080/api/transfer
    curl -sf --max-time 120 http://localhost:8080/api/batch-transfer
    curl -sf --max-time 30 http://localhost:8080/api/post-transfer
    curl -sf --max-time 5 http://localhost:8080/health
    sleep 5
    container_kill
    wait
}

container_name="proxyStressApp"

do_record_iteration() {
    local i="$1"
    local extra_flags="${2:-}"
    local label="${extra_flags:+_json}"
    local log="${container_name}${label}.txt"
    send_request &
    # shellcheck disable=SC2086
    $RECORD_BIN record $extra_flags -c "docker compose up" --container-name "$container_name" --generateGithubActions=false |& tee "$log"
    if grep "WARNING: DATA RACE" "$log"; then
        echo "FAIL: Data race during recording${label:+ (json)}"; exit 1
    fi
    if grep -q "panic:" "$log"; then
        echo "FAIL: Panic during recording${label:+ (json)}"; cat "$log"; exit 1
    fi
    sleep 5
    echo "Recorded test case and mocks for iteration ${i}${label:+ (json)}"
}

for i in {1..2}; do
    do_record_iteration "$i"
done

# shellcheck disable=SC1091
source "${GITHUB_WORKSPACE}/.github/workflows/test_workflow_scripts/json-pass-helpers.sh"

if json_pass_supported; then
    for i in {1..2}; do
        do_record_iteration "$i" "--storage-format json"
    done
fi

test_count=$(find ./keploy -name 'test-*.yaml' -path '*/tests/*' 2>/dev/null | wc -l)
echo "Total recorded test cases: $test_count"
if [ "$test_count" -eq 0 ]; then echo "FAIL: No test cases recorded"; exit 1; fi

echo "Shutting down services before test mode..."
docker compose down
echo "Services stopped"

test_container="proxyStressApp"
$REPLAY_BIN test -c 'docker compose up' --containerName "$test_container" --apiTimeout 60 --delay 15 --generate-github-actions=false |& tee "${test_container}.txt" || true

if grep "WARNING: DATA RACE" "${test_container}.txt"; then echo "FAIL: Data race during replay"; exit 1; fi
if grep -q "panic:" "${test_container}.txt"; then echo "FAIL: Panic during replay"; cat "${test_container}.txt"; exit 1; fi

report_count=$(find ./keploy/reports -name '*-report.yaml' 2>/dev/null | wc -l)
echo "Test reports generated: $report_count"
if [ "$report_count" -eq 0 ]; then echo "FAIL: No reports — replay hung"; cat "${test_container}.txt"; exit 1; fi
if grep -q "Error channel is full" "${test_container}.txt"; then echo "FAIL: Error channel overflow"; cat "${test_container}.txt"; exit 1; fi
if grep -q "incomplete or invalid response packet" "${test_container}.txt"; then echo "FAIL: PG decode failure"; cat "${test_container}.txt"; exit 1; fi

for report_file in ./keploy/reports/test-run-0/test-set-*-report.yaml; do
    [ -f "$report_file" ] && echo "$(basename "$report_file"): $(grep 'status:' "$report_file" | head -1 | awk '{print $2}')"
done

if json_pass_supported; then
    $REPLAY_BIN test --storage-format json -c 'docker compose up' --containerName "${test_container}_json" --apiTimeout 60 --delay 15 --generate-github-actions=false |& tee "${test_container}_json.txt" || true
    if grep "WARNING: DATA RACE" "${test_container}_json.txt"; then echo "FAIL: Data race during json replay"; exit 1; fi
    if grep -q "panic:" "${test_container}_json.txt"; then echo "FAIL: Panic during json replay"; cat "${test_container}_json.txt"; exit 1; fi
    if ! json_scan_reports; then
        cat "${test_container}_json.txt"
        exit 1
    fi
    echo "Proxy stress test PASSED — yaml + json"
else
    echo "Proxy stress test PASSED — yaml only (json pass skipped for compat-matrix cell)"
fi
