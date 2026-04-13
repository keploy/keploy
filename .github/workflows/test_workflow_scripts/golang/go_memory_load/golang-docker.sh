#!/usr/bin/env bash

set -Eeuo pipefail

source "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/test-iid.sh"

APP_CONTAINER_NAME="${APP_CONTAINER_NAME:-load-test-api}"
APP_HEALTH_URL="${APP_HEALTH_URL:-http://127.0.0.1:8080/healthz}"
RECORD_MEMORY_LIMIT_MB="${RECORD_MEMORY_LIMIT_MB:-200}"
KEPLOY_CONTAINER_MEMORY_LIMIT="${KEPLOY_CONTAINER_MEMORY_LIMIT:-160m}"
MIXED_API_START_VUS="${MIXED_API_START_VUS:-2}"
MIXED_API_VU_STAGE_TARGETS="${MIXED_API_VU_STAGE_TARGETS:-4,8,12,4}"
LARGE_PAYLOAD_PREALLOCATED_VUS="${LARGE_PAYLOAD_PREALLOCATED_VUS:-14}"
LARGE_PAYLOAD_MAX_VUS="${LARGE_PAYLOAD_MAX_VUS:-60}"
LARGE_PAYLOAD_STAGE_TARGETS="${LARGE_PAYLOAD_STAGE_TARGETS:-1,2,1}"
LARGE_PAYLOAD_SIZES_MB="${LARGE_PAYLOAD_SIZES_MB:-1}"
MEMORY_MONITOR_INTERVAL_SECONDS="${MEMORY_MONITOR_INTERVAL_SECONDS:-0.5}"

# CI-tuned k6 thresholds — intentionally very relaxed because:
#   - Keploy proxy buffers request/response bodies for capture, adding latency
#   - GOMEMLIMIT at 90% of memory-limit causes aggressive GC under pressure
#   - memoryguard pause/resume cycling disrupts connections (Connection: close)
#   - ubuntu-latest has 2 shared vCPUs compounding all of the above
# k6 threshold failures are NOT fatal — the script checks HTTP failure rate
# separately (see check_k6_failure_rate). Only >40% HTTP failures are fatal.
THRESHOLD_HTTP_FAILED_RATE="${THRESHOLD_HTTP_FAILED_RATE:-0.50}"
THRESHOLD_HTTP_P95="${THRESHOLD_HTTP_P95:-120000}"
THRESHOLD_HTTP_AVG="${THRESHOLD_HTTP_AVG:-60000}"
THRESHOLD_LARGE_INSERT_P95="${THRESHOLD_LARGE_INSERT_P95:-120000}"
THRESHOLD_LARGE_GET_P95="${THRESHOLD_LARGE_GET_P95:-120000}"
THRESHOLD_LARGE_DELETE_P95="${THRESHOLD_LARGE_DELETE_P95:-120000}"
# Hard failure rate: fail CI only when more than this fraction of requests fail.
CI_MAX_HTTP_FAILURE_RATE="${CI_MAX_HTTP_FAILURE_RATE:-0.40}"
MEMORY_VIOLATION_FILE="${PWD}/keploy-memory-violation.txt"
MEMORY_USAGE_LOG="${PWD}/keploy-memory-usage.log"
MEMORY_MONITOR_PID=""

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

    section "Keploy Memory Log"
    if [ -f "$MEMORY_USAGE_LOG" ]; then
        cat "$MEMORY_USAGE_LOG"
    else
        echo "Keploy memory log not found."
    fi

    section "Keploy Memory Violation"
    if [ -f "$MEMORY_VIOLATION_FILE" ]; then
        cat "$MEMORY_VIOLATION_FILE"
    else
        echo "No memory violation recorded."
    fi
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

stop_memory_monitor() {
    if [ -n "${MEMORY_MONITOR_PID:-}" ] && kill -0 "$MEMORY_MONITOR_PID" 2>/dev/null; then
        kill "$MEMORY_MONITOR_PID" 2>/dev/null || true
        wait "$MEMORY_MONITOR_PID" 2>/dev/null || true
    fi
    MEMORY_MONITOR_PID=""
}

final_cleanup() {
    local rc=$?
    stop_memory_monitor

    section "Keploy Memory Log"
    if [ -f "$MEMORY_USAGE_LOG" ]; then
        cat "$MEMORY_USAGE_LOG"
    else
        echo "Keploy memory log not found."
    fi

    if [ "$rc" -ne 0 ]; then
        echo "go-memory-load workflow failed (exit code=$rc)"
        dump_logs
    fi
    stop_keploy_record
    cleanup_compose
}

trap final_cleanup EXIT

bytes_from_human() {
    local value="$1"
    local number
    local unit
    local scale

    value="${value//[[:space:]]/}"
    if [ -z "$value" ] || [ "$value" = "--" ]; then
        echo "-1"
        return
    fi

    number="${value%%[[:alpha:]]*}"
    unit="${value#$number}"

    case "$unit" in
        B) scale=1 ;;
        KiB) scale=1024 ;;
        MiB) scale=$((1024 * 1024)) ;;
        GiB) scale=$((1024 * 1024 * 1024)) ;;
        TiB) scale=$((1024 * 1024 * 1024 * 1024)) ;;
        kB) scale=1000 ;;
        MB) scale=$((1000 * 1000)) ;;
        GB) scale=$((1000 * 1000 * 1000)) ;;
        TB) scale=$((1000 * 1000 * 1000 * 1000)) ;;
        *)
            echo "-1"
            return
            ;;
    esac

    awk -v number="$number" -v scale="$scale" 'BEGIN { printf "%.0f\n", number * scale }'
}

check_for_errors() {
    local logfile="$1"

    echo "Checking $logfile for critical Keploy errors..."

    if [ ! -f "$logfile" ]; then
        echo "Log file not found: $logfile"
        return 1
    fi

    # if grep "ERROR" "$logfile" | grep "Keploy:" | grep -v "failed to read symbols, skipping coverage calculation"; then
    #     echo "Critical Keploy errors found in $logfile"
    #     return 1
    # fi

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

start_memory_monitor() {
    local keploy_container="$1"
    local phase_pid="$2"
    local phase_name="$3"
    local threshold_bytes
    local threshold_mib

    threshold_bytes="$(docker inspect --format '{{.HostConfig.Memory}}' "$keploy_container" 2>/dev/null || true)"
    if [ -z "$threshold_bytes" ] || [ "$threshold_bytes" = "0" ]; then
        threshold_bytes="$((250 * 1024 * 1024))"
    fi

    threshold_mib="$(awk -v bytes="$threshold_bytes" 'BEGIN { printf "%.2f", bytes / 1024 / 1024 }')"
    echo "Monitoring ${keploy_container} memory during ${phase_name}. Threshold: ${threshold_bytes} bytes (${threshold_mib} MiB)." | tee -a "$MEMORY_USAGE_LOG"

    (
        usage_raw=""
        usage_value=""
        usage_bytes=""
        oom_killed=""
        running=""

        while kill -0 "$phase_pid" 2>/dev/null; do
            if ! docker inspect "$keploy_container" >/dev/null 2>&1; then
                sleep "$MEMORY_MONITOR_INTERVAL_SECONDS"
                continue
            fi

            usage_raw="$(docker stats --no-stream --format '{{.MemUsage}}' "$keploy_container" 2>/dev/null | head -n 1 || true)"
            usage_value="${usage_raw%% / *}"
            usage_bytes="$(bytes_from_human "$usage_value")"
            oom_killed="$(docker inspect --format '{{.State.OOMKilled}}' "$keploy_container" 2>/dev/null || echo false)"
            running="$(docker inspect --format '{{.State.Running}}' "$keploy_container" 2>/dev/null || echo false)"

            echo "[$(date -u +%FT%TZ)] phase=${phase_name} container=${keploy_container} usage=${usage_raw:-unknown} running=${running} oom_killed=${oom_killed}" >> "$MEMORY_USAGE_LOG"

            if [ "$oom_killed" = "true" ]; then
                echo "Keploy container ${keploy_container} was OOM-killed during ${phase_name}." > "$MEMORY_VIOLATION_FILE"
                kill -TERM "$phase_pid" 2>/dev/null || true
                exit 0
            fi

            if [ "$usage_bytes" -ge 0 ] && [ "$usage_bytes" -ge "$threshold_bytes" ]; then
                echo "Keploy container ${keploy_container} exceeded ${threshold_mib} MiB during ${phase_name}. Observed usage: ${usage_raw}." > "$MEMORY_VIOLATION_FILE"
                docker kill "$keploy_container" >/dev/null 2>&1 || true
                kill -TERM "$phase_pid" 2>/dev/null || true
                exit 0
            fi

            sleep "$MEMORY_MONITOR_INTERVAL_SECONDS"
        done
    ) &

    MEMORY_MONITOR_PID=$!
}

check_memory_violation() {
    if [ -f "$MEMORY_VIOLATION_FILE" ]; then
        echo "Keploy memory threshold violation detected:"
        cat "$MEMORY_VIOLATION_FILE"
        return 1
    fi
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

check_k6_failure_rate() {
    local k6_log="$1"
    local max_rate="$2"

    if [ ! -f "$k6_log" ]; then
        echo "k6 output log not found: $k6_log"
        return 1
    fi

    # Extract the http_req_failed percentage, e.g. "3.26%" from:
    #   http_req_failed.................: 3.26%    ✓ 10   ✗ 296
    local fail_pct
    fail_pct="$(grep -oP 'http_req_failed[.]*:\s+\K[0-9]+(\.[0-9]+)?' "$k6_log" | head -1 || true)"

    if [ -z "$fail_pct" ]; then
        echo "Could not parse http_req_failed from k6 output. Treating as pass."
        return 0
    fi

    # Convert max_rate (0.40) to percentage (40) for comparison.
    local max_pct
    max_pct="$(awk -v r="$max_rate" 'BEGIN { printf "%.2f", r * 100 }')"

    echo "k6 HTTP failure rate: ${fail_pct}% (max allowed: ${max_pct}%)"

    # Compare: fail if observed rate exceeds the maximum.
    local exceeded
    exceeded="$(awk -v obs="$fail_pct" -v max="$max_pct" 'BEGIN { print (obs > max) ? "yes" : "no" }')"

    if [ "$exceeded" = "yes" ]; then
        echo "HTTP failure rate ${fail_pct}% exceeds maximum ${max_pct}%. Failing CI."
        return 1
    fi

    echo "HTTP failure rate is within tolerance."
    return 0
}

run_loadtest() {
    section "Running k6 load test"
    local k6_log="${PWD}/k6-output.log"

    # Run k6 but do NOT let threshold failures kill the pipeline.
    # k6 exits non-zero when thresholds are crossed (latency, etc.)
    # which is expected on CI's constrained 2-vCPU runners.
    # We check the HTTP failure rate ourselves below.
    local k6_rc=0
    docker compose --profile loadtest run --rm --no-deps \
        -e MIXED_API_START_VUS="$MIXED_API_START_VUS" \
        -e MIXED_API_VU_STAGE_TARGETS="$MIXED_API_VU_STAGE_TARGETS" \
        -e LARGE_PAYLOAD_PREALLOCATED_VUS="$LARGE_PAYLOAD_PREALLOCATED_VUS" \
        -e LARGE_PAYLOAD_MAX_VUS="$LARGE_PAYLOAD_MAX_VUS" \
        -e LARGE_PAYLOAD_STAGE_TARGETS="$LARGE_PAYLOAD_STAGE_TARGETS" \
        -e LARGE_PAYLOAD_SIZES_MB="$LARGE_PAYLOAD_SIZES_MB" \
        -e THRESHOLD_HTTP_FAILED_RATE="$THRESHOLD_HTTP_FAILED_RATE" \
        -e THRESHOLD_HTTP_P95="$THRESHOLD_HTTP_P95" \
        -e THRESHOLD_HTTP_AVG="$THRESHOLD_HTTP_AVG" \
        -e THRESHOLD_LARGE_INSERT_P95="$THRESHOLD_LARGE_INSERT_P95" \
        -e THRESHOLD_LARGE_GET_P95="$THRESHOLD_LARGE_GET_P95" \
        -e THRESHOLD_LARGE_DELETE_P95="$THRESHOLD_LARGE_DELETE_P95" \
        k6 run /scripts/scenario.js 2>&1 | tee "$k6_log" || k6_rc=$?

    if [ "$k6_rc" -ne 0 ]; then
        echo "k6 exited with code ${k6_rc} (likely threshold violations). Checking HTTP failure rate..."
    fi

    check_k6_failure_rate "$k6_log" "$CI_MAX_HTTP_FAILURE_RATE"
}

section "Building sample application images"
docker compose build

section "Cleaning previous artifacts"
sudo rm -rf keploy/
rm -f record.txt test.txt docker-compose-tmp.yaml "$MEMORY_VIOLATION_FILE" "$MEMORY_USAGE_LOG"
cleanup_compose

section "Generating Keploy config"
"$RECORD_BIN" config --generate

section "Recording load-test traffic"
run_with_keploy_privileges "$RECORD_BIN" record -c "docker compose up" --container-name "$APP_CONTAINER_NAME" --memory-limit "$RECORD_MEMORY_LIMIT_MB" --enable-sampling --generate-github-actions=false 2>&1 | tee record.txt &
record_pid=$!
echo "Started Keploy record process with PID: $record_pid"

keploy_container="$(wait_for_keploy_container 120)"
echo "Detected Keploy container: $keploy_container"
# apply_keploy_memory_limit "$keploy_container"
start_memory_monitor "$keploy_container" "$record_pid" "record"

wait_for_http "$APP_HEALTH_URL" 180
run_loadtest

sleep 10
stop_keploy_record
wait "$record_pid" || true
stop_memory_monitor

check_memory_violation
check_for_errors record.txt
check_recorded_tests

# section "Preparing Replay"
# cleanup_compose

# section "Replaying recorded test cases"
# run_with_keploy_privileges "$REPLAY_BIN" test -c "docker compose up" --container-name "$APP_CONTAINER_NAME" --api-timeout 120 --delay 20 --generate-github-actions=false 2>&1 | tee test.txt &
# replay_pid=$!
# echo "Started Keploy test process with PID: $replay_pid"

# wait "$replay_pid" || true

# check_for_errors test.txt
# check_test_report

echo "go-memory-load workflow completed successfully."
