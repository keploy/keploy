#!/usr/bin/env bash

set -Eeuo pipefail

source "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/test-iid.sh"
source "${GITHUB_WORKSPACE:-${PWD%/samples-*}}/.github/workflows/test_workflow_scripts/docker-build-retry.sh"

APP_CONTAINER_NAME="${APP_CONTAINER_NAME:-load-test-mysql-api}"
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
# Debounce: how many CONSECUTIVE over-threshold samples before we declare a
# violation and kill keploy. A single 0.5s sample crossing the line is almost
# always a transient GC/page-cache spike, not a leak; killing on it severs every
# in-flight connection and manufactures an HTTP-failure storm, which is the main
# source of this lane's flakiness. Requiring K back-to-back over-threshold ticks
# (~1.5s of sustained excess) keeps real runaway growth caught while ignoring
# blips. Set to 1 to restore the old single-sample behaviour.
MEMORY_VIOLATION_MIN_TICKS="${MEMORY_VIOLATION_MIN_TICKS:-3}"

# CI-tuned k6 thresholds — intentionally very relaxed because:
#   - Keploy proxy buffers request/response bodies for capture, adding latency
#   - GOMEMLIMIT at 90% of memory-limit causes aggressive GC under pressure
#   - under memory pressure the guard pauses CAPTURE (captureEnabled=false); on
#     this lane's default ASYNC record path the client connection is kept alive
#     (NO Connection: close — that is injected only in sync/sampling mode, see
#     pkg/agent/proxy/incoming/http.go), so the extra latency is GC + the capture
#     pause, not severed connections
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
# One memory-guard pause the record forces by ingress request count
# (KEPLOY_DEBUG_FORCE_PAUSE, "<from>:<to>": requests from..to-1 come during it),
# so that every run puts a pause in the middle of the k6 load, on any runner,
# and check_capture_accounted can hold the recording to what a pause may cost.
# Empty: no forced pause (real memory pressure can still pause the guard).
FORCE_PAUSE="${FORCE_PAUSE-1500:1530}"
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
    local rec_pid pattern
    # The record CLI's own command line starts with "$RECORD_BIN record" (sudo
    # and env exec it with the path as given). Match only that: a looser
    # pattern also matches a wrapper whose command line holds this script's
    # text (bash -c "..."), and the newest match is then the wrapper, so the
    # stop never reaches the CLI and the run waits on it forever.
    pattern="^$(printf '%s' "$RECORD_BIN" | sed 's/[][\.*^$+?(){}|]/\\&/g') record( |$)"
    rec_pid="$(pgrep -n -f "$pattern" || true)"
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
        echo "go-memory-load-mysql workflow failed (exit code=$rc)"
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
    if ! find ./keploy -path '*/tests/*.yaml' -print -quit 2>/dev/null | grep . >/dev/null; then
        echo "No recorded test cases were generated."
        return 1
    fi

    echo "Recorded test cases were generated successfully."
}

# recording_counter LINE NAME prints the counter NAME of the agent's
# "agent: recording complete" LINE, or nothing when the line does not carry it.
recording_counter() {
    printf '%s\n' "$1" | grep -oP "\"$2\": \K[0-9]+" | head -1 || true
}

# check_capture_accounted fails when the recording loses requests without
# saying so, or when a pause did not end for the app's ingress. Each k6
# request must be a recorded test or a loss the agent's summary counts:
# suppressed, not recorded under memory pressure, left out in flight, dropped
# at capacity, or held at the end. Requests k6 saw fail may have reached no
# one, so they are allowed for, with 1% of the requests (at least 10) on top.
#
# Every pause must also be temporary, the forced one (FORCE_PAUSE) and each one
# real memory pressure made. The agent counts the ingress requests that came
# during its pauses by the order of events, apart from what capture did with
# them (requests_during_pauses: the request count read at each pause's start
# and end). A pause costs those and the requests in flight at its two edges
# (each k6 VU has one in flight), and no more. So, whatever the number of
# pauses N:
# - the requests not recorded under pressure are at most
#   requests_during_pauses + N * 2 * VUs: more says a pause stayed on for a
#   connection after it ended, and the agent counted its later requests;
# - the recording holds at least
#   reqs - failed - requests_during_pauses - N * 2 * VUs - slack tests (a
#   suppressed test does not count towards that): fewer says that, after a
#   pause, the app's requests did not become tests again.
# The second check can judge only when a recording that captured nothing after
# its first pause would fall below that floor: when the floor is above the
# requests that came before the first pause (requests_before_first_pause).
# When it is not, too few requests came outside the pauses and their edges
# for the check to tell a pause that ended from one that did not, and the lane
# fails, saying it could not judge, rather than pass on a bound nothing can
# miss. One 0.5 s pause used to stop capture on every pooled MySQL connection
# for the rest of the run: 1,985 tests of 7,287 requests, 1,466 suppressed,
# 3,836 not counted. A build whose pause stayed on for each keep-alive
# connection, counting each later request as not recorded, recorded 3,421 tests
# of 7,051 requests through a forced pause of 30.
#
# These checks judge the ingress side: whether the app's requests became
# tests. A build that went on recording tests after a pause, but not their
# MySQL calls, would pass them: those mocks are counted only in
# mocks_dropped_by_pressure, which no bound here reads. Whether MySQL capture
# came back after a pause is judged by the replay (check_test_report): a test
# whose MySQL calls were not recorded finds no mock and fails, and the lane
# fails when more than MAX_FAIL_PERCENT of the tests do.
#
# Only a recorder built from this tree is checked: a released agent predates
# these counts, and does not know the forced pause.
check_capture_accounted() {
    section "Checking that the recording accounts for every request"
    if [ "${RECORD_SRC:-build}" != build ]; then
        echo "The recorder is ${RECORD_SRC}, not this build: not checked."
        return 0
    fi

    local reqs fail_pct failed tests line name value
    reqs="$(grep -oP 'http_reqs[.]*:\s+\K[0-9]+' "${PWD}/k6-output.log" | head -1 || true)"
    if [ -z "$reqs" ]; then
        echo "k6's http_reqs is not in k6-output.log: cannot account for the recording."
        return 1
    fi
    fail_pct="$(grep -oP 'http_req_failed[.]*:\s+\K[0-9]+(\.[0-9]+)?' "${PWD}/k6-output.log" | head -1 || true)"
    failed="$(awk -v p="${fail_pct:-0}" -v n="$reqs" 'BEGIN { printf "%d", p * n / 100 + 0.999 }')"
    tests="$(find ./keploy -path '*/tests/*.yaml' 2>/dev/null | wc -l)"

    line="$(grep 'agent: recording complete' record.txt | tail -1 || true)"
    if [ -z "$line" ]; then
        echo "record.txt has no 'agent: recording complete' summary: the recording does not say what it lost."
        return 1
    fi
    local rc=0
    local -A n=()
    for name in tcs_sent_to_cli tcs_suppressed_total tcs_not_recorded_under_pressure \
        tcs_left_out_in_flight tcs_dropped_capacity tcs_held_at_end pressure_ranges_total \
        requests_during_pauses requests_before_first_pause; do
        value="$(recording_counter "$line" "$name")"
        if [ -z "$value" ]; then
            echo "FAIL: the agent's summary does not say ${name}; counted as 0 below."
            rc=1
        fi
        n[$name]="${value:-0}"
    done

    local reported=$((n[tcs_suppressed_total] + n[tcs_not_recorded_under_pressure] + n[tcs_left_out_in_flight] + n[tcs_dropped_capacity] + n[tcs_held_at_end]))
    local unaccounted=$((reqs - tests - reported))
    local slack=$((reqs / 100 > 10 ? reqs / 100 : 10))
    echo "k6: ${reqs} requests, ${failed} failed (${fail_pct:-?}%). Recorded tests: ${tests} (the agent sent ${n[tcs_sent_to_cli]})."
    echo "The agent counts as lost: suppressed ${n[tcs_suppressed_total]}, not recorded under pressure ${n[tcs_not_recorded_under_pressure]}, left out in flight ${n[tcs_left_out_in_flight]}, dropped at capacity ${n[tcs_dropped_capacity]}, held at the end ${n[tcs_held_at_end]}; memory-guard pauses: ${n[pressure_ranges_total]}."
    echo "Requests neither recorded nor counted as lost: ${unaccounted} (allowed: ${failed} failed + ${slack})."
    if [ "$unaccounted" -gt $((failed + slack)) ]; then
        echo "FAIL: the recording lost ${unaccounted} requests without saying so."
        rc=1
    fi

    if [ -n "$FORCE_PAUSE" ]; then
        local to="${FORCE_PAUSE##*:}"
        if ! grep -q 'Pausing keploy-agent recording, forced by KEPLOY_DEBUG_FORCE_PAUSE' record.txt; then
            echo "FAIL: the agent did not force the pause FORCE_PAUSE=${FORCE_PAUSE} asks for."
            rc=1
        elif ! grep -q 'Cleared keploy-agent memory pressure, forced by KEPLOY_DEBUG_FORCE_PAUSE' record.txt; then
            echo "FAIL: the forced pause did not end: it ends at ingress request ${to}, and k6 sent ${reqs}."
            rc=1
        fi
    fi
    local max_vus=0 vus
    for vus in "$MIXED_API_START_VUS" ${MIXED_API_VU_STAGE_TARGETS//,/ }; do
        [ "$vus" -gt "$max_vus" ] && max_vus="$vus"
    done
    max_vus=$((max_vus + LARGE_PAYLOAD_MAX_VUS))
    local pauses="${n[pressure_ranges_total]}" during="${n[requests_during_pauses]}"
    local before="${n[requests_before_first_pause]}" not_recorded="${n[tcs_not_recorded_under_pressure]}"
    local edges=$((pauses * 2 * max_vus))
    echo "Memory-guard pauses: ${pauses}. The agent read ${during} requests during them and ${before} before the first; ${max_vus} VUs are in flight at each of their edges (${edges} for all)."
    if [ "$not_recorded" -gt $((during + edges)) ]; then
        echo "FAIL: the agent did not record ${not_recorded} requests under pressure, more than the ${during} that came during its pauses and the ${edges} in flight at their edges: a pause stayed on for some connection after it ended."
        rc=1
    fi
    local floor=$((reqs - failed - during - edges - slack))
    local judged=0
    if [ "$pauses" -eq 0 ]; then
        echo "No memory-guard pause in this recording: there is no resumption to judge."
    elif [ "$floor" -le "$before" ]; then
        echo "COULD NOT JUDGE: the recording would have to hold ${floor} tests, and the ${before} requests before the first pause alone reach that: too few requests came outside the pauses and their edges to tell a pause that ended from one that did not."
        rc=1
    else
        judged=1
        echo "The recording must hold at least ${floor} tests (more than the ${before} before the first pause)."
        if [ "$tests" -lt "$floor" ]; then
            echo "FAIL: the recording holds ${tests} tests of ${reqs} requests: the pauses cost more than the requests during them and at their edges, so after a pause the app's requests did not become tests again."
            rc=1
        fi
    fi
    if [ "$rc" -eq 0 ]; then
        if [ "$judged" -eq 1 ]; then
            echo "The recording accounts for every request, and after every pause the app's requests became tests again; the replay judges whether MySQL capture came back."
        else
            echo "The recording accounts for every request."
        fi
    fi
    return "$rc"
}

check_test_report() {
    local latest_report_dir
    local found_reports=false
    local report_file
    local agg_total=0
    local agg_failure=0

    # Maximum tolerated test-case failure percentage. These memory-load lanes
    # run under deliberate memory pressure on shared 2-vCPU CI runners, where a
    # small number of non-deterministic test divergences is expected and is not
    # a real regression. The lane fails only if MORE than this fraction of the
    # recorded test cases fail at replay. Tunable via MAX_FAIL_PERCENT.
    local max_fail_percent="${MAX_FAIL_PERCENT:-4}"

    echo "Checking test reports (failure tolerance: ${max_fail_percent}% of total test cases)..."

    if [ ! -d "./keploy/reports" ]; then
        echo "Test report directory not found."
        return 1
    fi

    latest_report_dir="$(ls -td ./keploy/reports/test-run-* 2>/dev/null | head -n 1 || true)"
    if [ -z "$latest_report_dir" ]; then
        echo "No test run directory found in ./keploy/reports/"
        return 1
    fi

    # Match both YAML (default) and JSON (--storage-format json) report files.
    # Aggregate the report-level total/failure counts across every test-set so
    # the tolerance is computed over the whole lane, not per-set.
    for report_file in "$latest_report_dir"/test-set-*-report.yaml "$latest_report_dir"/test-set-*-report.json; do
        [ -e "$report_file" ] || continue
        found_reports=true

        local test_set_name set_status set_total set_failure

        test_set_name="$(basename "$report_file")"
        test_set_name="${test_set_name%-report.*}"

        # grep -m 1 takes the report-LEVEL summary field (the first occurrence),
        # not the many per-test fields below it. gsub strips JSON quotes/commas
        # so both `total: 658` (YAML) and `"total": 658,` (JSON) parse cleanly.
        set_status="$(grep -m 1 -E '^[[:space:]]*"?status"?:' "$report_file" | awk '{gsub(/[",]/, ""); print $2}')"
        set_total="$(grep -m 1 -E '^[[:space:]]*"?total"?:' "$report_file" | awk '{gsub(/[",]/, ""); print $2}')"
        set_failure="$(grep -m 1 -E '^[[:space:]]*"?failure"?:' "$report_file" | awk '{gsub(/[",]/, ""); print $2}')"

        set_total="${set_total:-0}"
        set_failure="${set_failure:-0}"

        echo "  ${test_set_name}: status=${set_status:-unknown} total=${set_total} failure=${set_failure}"

        agg_total=$((agg_total + set_total))
        agg_failure=$((agg_failure + set_failure))
    done

    if [ "$found_reports" = false ]; then
        echo "No report files found in $latest_report_dir"
        return 1
    fi

    if [ "$agg_total" -le 0 ]; then
        echo "Report total test-case count is zero — treating as failure."
        return 1
    fi

    local fail_pct within
    fail_pct="$(awk -v f="$agg_failure" -v t="$agg_total" 'BEGIN { printf "%.4f", (f * 100.0) / t }')"
    within="$(awk -v p="$fail_pct" -v m="$max_fail_percent" 'BEGIN { print (p <= m) ? "yes" : "no" }')"

    echo "Aggregate: ${agg_failure}/${agg_total} test cases failed (${fail_pct}%). Tolerance: ${max_fail_percent}%."

    if [ "$within" = "yes" ]; then
        echo "Failure rate within tolerance — passing the lane."
        return 0
    fi

    echo "Failure rate ${fail_pct}% exceeds tolerance ${max_fail_percent}% — failing the lane."
    return 1
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

# read_working_set_bytes echoes the container's working-set memory in bytes:
#
#     working_set = memory.current - inactive_file
#
# This matches what kubectl top, the Kubernetes eviction manager, and the
# keploy in-process memoryguard use. Page cache (specifically inactive_file)
# is reclaimable under memory pressure and not a real OOM risk, so excluding
# it gives an apples-to-apples comparison with the keploy --memory-limit
# pressure trigger (which also runs against working-set).
#
# docker stats --format '{{.MemUsage}}' reports total RSS + page cache and
# routinely overshoots working-set by 100–200 MiB on a write-heavy recording
# workload (mysql/mongo mocks.yaml file growth). The script previously killed
# the container on that inflated number, producing false-positive failures
# even when keploy's own pressure machinery was healthy and dropping zero mocks.
#
# Reads first the container's own cgroup v2 files as this host sees them: the
# cgroup /proc/<its pid>/cgroup names, under this host's /sys/fs/cgroup. That
# is the file keploy's memory guard reads inside the container. The container's
# own /sys/fs/cgroup is not: the agent bind-mounts the Docker daemon's whole
# /sys/fs/cgroup in a cgroup namespace of its own, so there memory.current is
# the daemon's root cgroup. On a GitHub runner the root has no memory.current
# (the reads below fell through to docker stats), and under docker-in-docker
# it is the whole outer container: MySQL, the app and k6 too.
# Then falls back to cgroup v2 and v1 inside the container, and finally to
# docker stats MemUsage if every cgroup read fails.
read_working_set_bytes() {
    local container="$1"
    local current=""
    local inactive=""
    local pid cg

    pid="$(docker inspect --format '{{.State.Pid}}' "$container" 2>/dev/null || true)"
    if [ -n "$pid" ] && [ "$pid" != "0" ]; then
        cg="$(awk -F: '$1 == "0" { print $3; exit }' "/proc/$pid/cgroup" 2>/dev/null || true)"
        if [ -n "$cg" ] && [ "$cg" != "/" ] && current="$(cat "/sys/fs/cgroup${cg}/memory.current" 2>/dev/null)" && [ -n "$current" ]; then
            inactive="$(awk '/^inactive_file /{print $2; exit}' "/sys/fs/cgroup${cg}/memory.stat" 2>/dev/null || true)"
            if [ -n "$inactive" ]; then
                local ws=$((current - inactive))
                [ "$ws" -lt 0 ] && ws=0
                echo "$ws"
                return 0
            fi
        fi
    fi

    # cgroup v2: /sys/fs/cgroup/memory.current + /sys/fs/cgroup/memory.stat
    if current="$(docker exec "$container" cat /sys/fs/cgroup/memory.current 2>/dev/null)" && [ -n "$current" ]; then
        inactive="$(docker exec "$container" sh -c "awk '/^inactive_file /{print \$2; exit}' /sys/fs/cgroup/memory.stat" 2>/dev/null || true)"
        if [ -n "$inactive" ]; then
            local ws=$((current - inactive))
            [ "$ws" -lt 0 ] && ws=0
            echo "$ws"
            return 0
        fi
    fi

    # cgroup v1: /sys/fs/cgroup/memory/{memory.usage_in_bytes,memory.stat}
    if current="$(docker exec "$container" cat /sys/fs/cgroup/memory/memory.usage_in_bytes 2>/dev/null)" && [ -n "$current" ]; then
        inactive="$(docker exec "$container" sh -c "awk '/^total_inactive_file /{print \$2; exit}' /sys/fs/cgroup/memory/memory.stat" 2>/dev/null || true)"
        if [ -n "$inactive" ]; then
            local ws=$((current - inactive))
            [ "$ws" -lt 0 ] && ws=0
            echo "$ws"
            return 0
        fi
    fi

    # Last resort: docker stats raw MemUsage (includes page cache; will
    # overshoot working-set on write-heavy workloads).
    local mem_raw mem_val
    mem_raw="$(docker stats --no-stream --format '{{.MemUsage}}' "$container" 2>/dev/null | head -n 1 || true)"
    mem_val="${mem_raw%% / *}"
    bytes_from_human "$mem_val"
}

start_memory_monitor() {
    local keploy_container="$1"
    local phase_pid="$2"
    local phase_name="$3"
    local threshold_bytes
    local threshold_mib

    threshold_bytes="$(docker inspect --format '{{.HostConfig.Memory}}' "$keploy_container" 2>/dev/null || true)"
    if [ -z "$threshold_bytes" ] || [ "$threshold_bytes" = "0" ]; then
        # Working-set threshold. Lower than the old MemUsage-based 350 MiB
        # because we no longer count reclaimable page cache. Picked to give
        # keploy headroom above its internal pressure trigger (160 MiB at
        # --memory-limit 200) for Go-runtime overhead and goroutine stacks
        # while still catching real leaks.
        threshold_bytes="$((400 * 1024 * 1024))"
    fi

    threshold_mib="$(awk -v bytes="$threshold_bytes" 'BEGIN { printf "%.2f", bytes / 1024 / 1024 }')"
    echo "Monitoring ${keploy_container} working-set memory during ${phase_name}. Threshold: ${threshold_bytes} bytes (${threshold_mib} MiB)." | tee -a "$MEMORY_USAGE_LOG"

    (
        usage_bytes=""
        oom_killed=""
        running=""
        consecutive_over=0

        while kill -0 "$phase_pid" 2>/dev/null; do
            if ! docker inspect "$keploy_container" >/dev/null 2>&1; then
                sleep "$MEMORY_MONITOR_INTERVAL_SECONDS"
                continue
            fi

            usage_bytes="$(read_working_set_bytes "$keploy_container" 2>/dev/null || echo 0)"
            oom_killed="$(docker inspect --format '{{.State.OOMKilled}}' "$keploy_container" 2>/dev/null || echo false)"
            running="$(docker inspect --format '{{.State.Running}}' "$keploy_container" 2>/dev/null || echo false)"

            # Human-readable usage in MiB for the log so operators don't
            # have to divide by 1024^2.
            usage_mib="$(awk -v bytes="$usage_bytes" 'BEGIN { printf "%.1f", bytes / 1024 / 1024 }')"
            echo "[$(date -u +%FT%TZ)] phase=${phase_name} container=${keploy_container} working_set=${usage_mib}MiB (${usage_bytes}B) running=${running} oom_killed=${oom_killed}" >> "$MEMORY_USAGE_LOG"

            if [ "$oom_killed" = "true" ]; then
                echo "Keploy container ${keploy_container} was OOM-killed during ${phase_name}." > "$MEMORY_VIOLATION_FILE"
                kill -TERM "$phase_pid" 2>/dev/null || true
                exit 0
            fi

            if [ "$usage_bytes" -ge "$threshold_bytes" ]; then
                consecutive_over=$((consecutive_over + 1))
                if [ "$consecutive_over" -ge "$MEMORY_VIOLATION_MIN_TICKS" ]; then
                    sustained_s="$(awk -v n="$consecutive_over" -v i="$MEMORY_MONITOR_INTERVAL_SECONDS" 'BEGIN { printf "%.1f", n * i }')"
                    echo "Keploy container ${keploy_container} exceeded ${threshold_mib} MiB working-set during ${phase_name} for ${consecutive_over} consecutive samples (~${sustained_s}s sustained). Observed: ${usage_mib} MiB." > "$MEMORY_VIOLATION_FILE"
                    docker kill "$keploy_container" >/dev/null 2>&1 || true
                    kill -TERM "$phase_pid" 2>/dev/null || true
                    exit 0
                fi
            else
                # Dropped back under the line — the excess was a transient spike,
                # not sustained growth. Reset so only BACK-TO-BACK breaches count.
                consecutive_over=0
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
docker_build_retry docker compose build

section "Cleaning previous artifacts"
sudo rm -rf keploy/
rm -f record.txt test.txt docker-compose-tmp.yaml "$MEMORY_VIOLATION_FILE" "$MEMORY_USAGE_LOG"
cleanup_compose

section "Generating Keploy config"
# Only when the sample does not ship one. `config --generate` used to do
# nothing at all over an existing keploy.yml -- it asked, stdin answered EOF,
# and it skipped and exited 0 -- so the shipped config is what these runs have
# always used, noise rules and all. It refuses out loud now rather than
# pretending, which is right, and this says what the run actually wants.
[ -f keploy.yml ] || "$RECORD_BIN" config --generate

section "Recording load-test traffic"
run_with_keploy_privileges env KEPLOY_DEBUG_FORCE_PAUSE="$FORCE_PAUSE" "$RECORD_BIN" record -c "docker compose up" --container-name "$APP_CONTAINER_NAME" --memory-limit "$RECORD_MEMORY_LIMIT_MB" --generate-github-actions=false 2>&1 | tee record.txt &
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
check_capture_accounted

section "Preparing Replay"
cleanup_compose

section "Replaying recorded test cases"
run_with_keploy_privileges "$REPLAY_BIN" test -c "docker compose up" --container-name "$APP_CONTAINER_NAME" --api-timeout 120 --delay 20 --generate-github-actions=false 2>&1 | tee test.txt &
replay_pid=$!
echo "Started Keploy test process with PID: $replay_pid"

wait "$replay_pid" || true

check_for_errors test.txt
check_test_report

echo "go-memory-load-mysql workflow completed successfully."
