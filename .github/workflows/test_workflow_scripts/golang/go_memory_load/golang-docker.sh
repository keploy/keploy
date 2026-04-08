#!/usr/bin/env bash

set -Eeuo pipefail

source "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/test-iid.sh"

APP_CONTAINER_NAME="${APP_CONTAINER_NAME:-load-test-api}"
APP_HEALTH_URL="${APP_HEALTH_URL:-http://127.0.0.1:8080/healthz}"
RECORD_MEMORY_LIMIT_MB="${RECORD_MEMORY_LIMIT_MB:-200}"
KEPLOY_CONTAINER_MEMORY_LIMIT="${KEPLOY_CONTAINER_MEMORY_LIMIT:-300m}"

section() {
    printf '\n==== %s ====\n' "$*"
}

run_with_keploy_privileges() {
    if command -v sudo >/dev/null 2>&1; then
        sudo -E env PATH="$PATH" "$@"
    else
        env PATH="$PATH" "$@"
    fi
}

dump_logs() {
    section "Docker PS"
    docker ps -a || true

    section "Record Log"
    if [ -f record.txt ]; then
        cat record.txt
    else
        echo "Record log not found."
    fi

    section "Replay Log"
    if [ -f test.txt ]; then
        cat test.txt
    else
        echo "Replay log not found."
    fi

    section "Compose State"
    docker compose ps || true
    docker compose logs || true
}

stop_keploy_record() {
    local rec_pid
    rec_pid="$(pgrep -n -f 'keploy[^ ]* record' || true)"
    echo "Keploy record PID: ${rec_pid:-not-found}"
    if [ -n "${rec_pid:-}" ]; then
        sudo kill -INT "$rec_pid" 2>/dev/null || true
    fi
}

cleanup_compose() {
    docker compose down -v --remove-orphans >/dev/null 2>&1 || true
}

final_cleanup() {
    local rc=$?
    if [ "$rc" -ne 0 ]; then
        echo "go-memory-load workflow failed (exit code=$rc)"
        dump_logs
    fi
    stop_keploy_record
    cleanup_compose
}

trap final_cleanup EXIT

check_for_errors() {
    local logfile="$1"

    echo "Checking $logfile for critical Keploy errors..."

    if [ ! -f "$logfile" ]; then
        echo "Log file not found: $logfile"
        return 1
    fi

    if grep "ERROR" "$logfile" | grep "Keploy:" | grep -v "failed to read symbols, skipping coverage calculation"; then
        echo "Critical Keploy errors found in $logfile"
        return 1
    fi

    if grep -q "WARNING: DATA RACE" "$logfile"; then
        echo "Data race detected in $logfile"
        return 1
    fi

    if grep -qE 'panic:|fatal error:' "$logfile"; then
        echo "Fatal error detected in $logfile"
        return 1
    fi

    echo "No critical errors found in $logfile."
}

check_recorded_tests() {
    if ! find ./keploy -path '*/tests/test-*.yaml' -print -quit 2>/dev/null | grep -q .; then
        echo "No recorded test cases were generated."
        return 1
    fi

    echo "Recorded test cases were generated successfully."
}

check_test_report() {
    local latest_report_dir
    local all_passed=true
    local report_file

    echo "Checking test reports..."

    if [ ! -d "./keploy/reports" ]; then
        echo "Test report directory not found."
        return 1
    fi

    latest_report_dir="$(ls -td ./keploy/reports/test-run-* 2>/dev/null | head -n 1 || true)"
    if [ -z "$latest_report_dir" ]; then
        echo "No test run directory found in ./keploy/reports/"
        return 1
    fi

    for report_file in "$latest_report_dir"/test-set-*-report.yaml; do
        local test_set_name
        local test_status

        [ -e "$report_file" ] || {
            echo "No report files found in $latest_report_dir"
            return 1
        }

        test_set_name="$(basename "$report_file" -report.yaml)"
        test_status="$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')"

        echo "Status for ${test_set_name}: $test_status"
        if [ "$test_status" != "PASSED" ]; then
            all_passed=false
            echo "${test_set_name} did not pass."
        fi
    done

    if [ "$all_passed" = false ]; then
        return 1
    fi

    echo "All test reports passed."
}

wait_for_keploy_container() {
    local timeout_s="${1:-120}"
    local keploy_container=""
    local i

    section "Waiting for Keploy container" >&2
    for ((i = 1; i <= timeout_s; i++)); do
        keploy_container="$(docker ps --format '{{.Names}}' | grep '^keploy-v3-' | head -n 1 || true)"
        if [ -n "$keploy_container" ]; then
            echo "$keploy_container"
            return 0
        fi
        sleep 1
    done

    echo "Keploy container did not become available in time."
    docker ps -a || true
    return 1
}

apply_keploy_memory_limit() {
    local keploy_container="$1"

    section "Applying Docker memory limit to ${keploy_container}"
    if ! docker update --memory "$KEPLOY_CONTAINER_MEMORY_LIMIT" --memory-swap "$KEPLOY_CONTAINER_MEMORY_LIMIT" "$keploy_container"; then
        echo "docker update with --memory-swap failed, retrying with --memory only"
        docker update --memory "$KEPLOY_CONTAINER_MEMORY_LIMIT" "$keploy_container"
    fi

    docker inspect --format 'Memory={{.HostConfig.Memory}} MemorySwap={{.HostConfig.MemorySwap}}' "$keploy_container" || true
}

wait_for_http() {
    local url="${1:-$APP_HEALTH_URL}"
    local timeout_s="${2:-180}"
    local i

    section "Waiting for application on ${url}"
    for ((i = 1; i <= timeout_s; i++)); do
        if curl -fsS --max-time 2 "$url" -o /dev/null; then
            echo "Application is ready on ${url}"
            return 0
        fi
        sleep 1
    done

    echo "Application did not become available on ${url} in time."
    docker compose ps || true
    docker compose logs api db || true
    return 1
}

run_loadtest() {
    section "Running k6 load test"
    docker compose --profile loadtest run --rm --no-deps k6 run /scripts/scenario.js
}

section "Building sample application images"
docker compose build

section "Cleaning previous artifacts"
sudo rm -rf keploy/
rm -f record.txt test.txt docker-compose-tmp.yaml
cleanup_compose

section "Generating Keploy config"
"$RECORD_BIN" config --generate

section "Recording load-test traffic"
run_with_keploy_privileges "$RECORD_BIN" record -c "docker compose up" --container-name "$APP_CONTAINER_NAME" --memory-limit "$RECORD_MEMORY_LIMIT_MB" --generate-github-actions=false 2>&1 | tee record.txt &
record_pid=$!
echo "Started Keploy record process with PID: $record_pid"

keploy_container="$(wait_for_keploy_container 120)"
echo "Detected Keploy container: $keploy_container"
apply_keploy_memory_limit "$keploy_container"

wait_for_http "$APP_HEALTH_URL" 180
run_loadtest

sleep 10
stop_keploy_record
wait "$record_pid" || true

check_for_errors record.txt
check_recorded_tests

section "Preparing Replay"
cleanup_compose

section "Replaying recorded test cases"
run_with_keploy_privileges "$REPLAY_BIN" test -c "docker compose up" --container-name "$APP_CONTAINER_NAME" --api-timeout 120 --delay 20 --generate-github-actions=false 2>&1 | tee test.txt || true

check_for_errors test.txt
check_test_report

echo "go-memory-load workflow completed successfully."
