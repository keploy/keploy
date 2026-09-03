#!/usr/bin/env bash
#
# MySQL replay-vs-record wall-clock ratio harness (REPORT-ONLY).
#
# Goal we are tracking: "replay >= 10x faster than record". This script
# stands up the go-memory-load-mysql sample under Keploy, drives a fixed,
# deterministic HTTP workload while RECORDING (real MySQL), then REPLAYS the
# recorded test cases against mocked MySQL and compares the two execution
# wall-clocks. It also captures a CPU profile of the REPLAY agent so the hot
# path can be inspected when the ratio is short of target.
#
# It is deliberately non-fatal on the ratio: this is a report-only gate. It
# only fails on genuine harness breakage (recording produced nothing, replay
# crashed, etc.) so the number it prints can be trusted. Flip PERF_GATE_BLOCKING
# to "true" (env, wired from the workflow) once the replayer is fast enough to
# hold the line, and the ratio shortfall becomes a hard failure.
#
# Modelled on the sibling golang-docker.sh harnesses (test_workflow_scripts/
# golang/go_memory_load_mysql/golang-docker.sh): same sourcing, same
# run_with_keploy_privileges + compose lifecycle, same GITHUB_WORKSPACE layout.
# It is expected to be `source`d from samples-go/go-memory-load-mysql.

set -Eeuo pipefail

source "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/test-iid.sh"
source "${GITHUB_WORKSPACE:-${PWD%/samples-*}}/.github/workflows/test_workflow_scripts/docker-build-retry.sh"

# -------------------------------------------------------------------------
# Configuration (all env-overridable so the workflow / a local run can tune).
# -------------------------------------------------------------------------
# One binary drives both phases so record and replay share an implementation.
# The workflow points KEPLOY_BIN at the download-binary output; RECORD_BIN /
# REPLAY_BIN are accepted as a fallback for parity with the sibling harnesses.
KEPLOY_BIN="${KEPLOY_BIN:-${RECORD_BIN:-keploy}}"
REPLAY_BIN="${REPLAY_BIN:-$KEPLOY_BIN}"

APP_CONTAINER_NAME="${APP_CONTAINER_NAME:-load-test-mysql-api}"
APP_HEALTH_URL="${APP_HEALTH_URL:-http://127.0.0.1:8080/healthz}"
APP_BASE_URL="${APP_BASE_URL:-http://127.0.0.1:8080}"

# Fixed, deterministic workload size. Each iteration issues 8 requests (a mix
# of MySQL writes + reads), so PERF_ITERATIONS=40 records ~320 test cases —
# enough that the sequential replay runs for several seconds on a shared
# 2-vCPU runner, keeping the wall-clock well above timing granularity.
PERF_ITERATIONS="${PERF_ITERATIONS:-40}"

# keploy test --delay: startup grace for docker compose up + agent readiness.
# This is EXCLUDED from the measured replay window (see parse_replay_seconds).
REPLAY_DELAY="${REPLAY_DELAY:-20}"
API_TIMEOUT="${API_TIMEOUT:-120}"

# Post-workload settle so in-flight mocks flush to disk before record stops.
# Outside the measured record window.
RECORD_SETTLE_SECONDS="${RECORD_SETTLE_SECONDS:-10}"

PERF_TARGET_RATIO="${PERF_TARGET_RATIO:-10}"
# Report-only by default. "true" makes a ratio shortfall a hard failure.
PERF_GATE_BLOCKING="${PERF_GATE_BLOCKING:-false}"

# Minimum share of workload requests that must return 2xx for the recording to
# be considered trustworthy. Below this the recorded mocks are meaningless, so
# the ratio would be garbage — that is harness breakage (fatal), NOT a ratio
# verdict, and is independent of the report-only gate.
PERF_MIN_SUCCESS_PCT="${PERF_MIN_SUCCESS_PCT:-80}"

# Outputs. PERF_OUT_DIR is what the workflow uploads as an artifact.
PERF_OUT_DIR="${PERF_OUT_DIR:-$PWD/perf-results}"
# Host-side CPU_PROFILE path. In docker-compose mode keploy does NOT write this
# file itself — it injects CPU_PROFILE into the keploy-agent container and
# mounts $PWD to /tmp/pprof_output, so the agent writes "docker-<basename>"
# into $PWD (see pkg/platform/docker/docker.go). We collect that docker-* file.
REPLAY_CPU_PROFILE="${REPLAY_CPU_PROFILE:-$PWD/replay-cpu.prof}"

RECORD_LOG="$PWD/record.txt"
REPLAY_LOG="$PWD/test.txt"
WORKLOAD_LOG="$PWD/workload.txt"
SUMMARY_ENV="$PERF_OUT_DIR/summary.env"
SUMMARY_JSON="$PERF_OUT_DIR/summary.json"

REQUESTS_SENT=0
REQUESTS_OK=0

section() {
    printf '\n==== %s ====\n' "$*"
}

# sudo -E so keploy keeps the env (CPU_PROFILE etc.) when it re-execs for
# privileged eBPF/proxy setup. Mirrors the sibling harnesses.
run_with_keploy_privileges() {
    if command -v sudo >/dev/null 2>&1; then
        sudo -E env PATH="$PATH" "$@"
    else
        env PATH="$PATH" "$@"
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

dump_logs() {
    section "Docker PS"
    docker ps -a || true
    section "Record Log (tail)"
    [ -f "$RECORD_LOG" ] && tail -n 120 "$RECORD_LOG" || echo "Record log not found."
    section "Replay Log (tail)"
    [ -f "$REPLAY_LOG" ] && tail -n 120 "$REPLAY_LOG" || echo "Replay log not found."
    section "Compose State"
    docker compose ps || true
}

final_cleanup() {
    local rc=$?
    if [ "$rc" -ne 0 ]; then
        echo "mysql_replay_perf harness failed (exit code=$rc)"
        dump_logs
    fi
    stop_keploy_record
    cleanup_compose
}
trap final_cleanup EXIT

wait_for_http() {
    local url="${1:-$APP_HEALTH_URL}"
    local timeout_s="${2:-180}"
    local i
    section "Waiting for application on ${url}"
    for ((i = 1; i <= timeout_s; i++)); do
        if curl -fsS --max-time 2 "$url" -o /dev/null; then
            echo "Application is ready on ${url} after ${i}s"
            return 0
        fi
        sleep 1
    done
    echo "Application did not become available on ${url} in time."
    docker compose ps || true
    docker compose logs api db || true
    return 1
}

# Pull the first top-level JSON "id" from a response body. Order responses nest
# customer.id, but Go marshals struct fields in declaration order and Order.ID
# is the first field, so the first match is always the top-level id.
#
# Trailing `|| true` is load-bearing: under `set -Eeuo pipefail` a body with no
# "id" (a non-2xx / {"error":...} / empty-on-timeout response) makes grep exit
# non-zero, and `head` closing the pipe early can hand grep a SIGPIPE — either
# would abort the whole harness at the `cust=$(... json_id)` assignment and red
# the PR on a transient hiccup, which the report-only contract forbids. We want
# an empty string + success, and the caller already guards on emptiness.
json_id() {
    grep -oE '"id":"[^"]+"' | head -n 1 | sed -E 's/.*:"//; s/"$//' || true
}

# One deterministic workload request. Counts sends/successes for the summary.
# Non-2xx does not abort the loop (a report-only perf gate must not die on a
# single divergent request); the aggregate success rate is checked afterwards.
req() {
    local method="$1" path="$2" body="${3:-}"
    local out http
    REQUESTS_SENT=$((REQUESTS_SENT + 1))
    if [ -n "$body" ]; then
        out="$(curl -sS --max-time 15 -w '\n%{http_code}' -X "$method" \
            -H 'Content-Type: application/json' -d "$body" "$APP_BASE_URL$path" 2>>"$WORKLOAD_LOG" || true)"
    else
        out="$(curl -sS --max-time 15 -w '\n%{http_code}' -X "$method" \
            "$APP_BASE_URL$path" 2>>"$WORKLOAD_LOG" || true)"
    fi
    http="${out##*$'\n'}"
    REQ_BODY="${out%$'\n'*}"
    if [[ "$http" =~ ^2 ]]; then
        REQUESTS_OK=$((REQUESTS_OK + 1))
    fi
    echo "$method $path -> $http" >>"$WORKLOAD_LOG"
}

# Deterministic, sequential traffic. Each iteration exercises MySQL writes
# (customer/product/order INSERT + a multi-statement order transaction) and
# reads (point lookup, search, per-customer aggregate, analytics rollup, ping)
# so the recorded mocks cover the MySQL replayer's real query surface.
drive_workload() {
    local segments=(startup enterprise retail partner)
    local statuses=(pending paid shipped cancelled)
    local i seg st cust prod order
    for ((i = 1; i <= PERF_ITERATIONS; i++)); do
        seg="${segments[$((i % 4))]}"
        st="${statuses[$((i % 4))]}"

        req POST /customers \
            "{\"email\":\"perf-${i}@example.com\",\"full_name\":\"Perf User ${i}\",\"segment\":\"${seg}\"}"
        cust="$(printf '%s' "$REQ_BODY" | json_id)"

        req POST /products \
            "{\"sku\":\"SKU-${i}\",\"name\":\"Widget ${i}\",\"category\":\"cat-${seg}\",\"price_cents\":$((500 + i)),\"inventory_count\":1000}"
        prod="$(printf '%s' "$REQ_BODY" | json_id)"

        if [ -n "$cust" ] && [ -n "$prod" ]; then
            req POST /orders \
                "{\"customer_id\":\"${cust}\",\"status\":\"${st}\",\"items\":[{\"product_id\":\"${prod}\",\"quantity\":1}]}"
            order="$(printf '%s' "$REQ_BODY" | json_id)"
            [ -n "$order" ] && req GET "/orders/${order}"
            req GET "/customers/${cust}/summary"
        fi

        req GET "/orders?status=${st}&limit=10"
        req GET "/analytics/top-products?days=30&limit=10"
        req GET /healthz
    done
    echo "Workload complete: ${REQUESTS_OK}/${REQUESTS_SENT} requests returned 2xx."
}

# Guard the trustworthiness of the recording: if most of the deterministic
# workload failed (sample stopped serving a route, DB unhealthy, agent not
# capturing), the recorded mocks — and therefore the ratio — are meaningless.
# This is fatal even under report-only, because report-only only ever suppresses
# the RATIO verdict, never genuine harness breakage.
check_workload_success() {
    local pct
    if [ "${REQUESTS_SENT:-0}" -le 0 ]; then
        echo "No workload requests were sent."
        return 1
    fi
    pct="$(awk -v ok="$REQUESTS_OK" -v n="$REQUESTS_SENT" 'BEGIN { printf "%.1f", (ok * 100.0) / n }')"
    echo "Workload success rate: ${pct}% (${REQUESTS_OK}/${REQUESTS_SENT}, min ${PERF_MIN_SUCCESS_PCT}%)."
    if awk -v p="$pct" -v m="$PERF_MIN_SUCCESS_PCT" 'BEGIN { exit !(p >= m) }'; then
        return 0
    fi
    echo "Workload success rate below ${PERF_MIN_SUCCESS_PCT}% — recording is not trustworthy."
    return 1
}

check_recorded_tests() {
    local n
    n="$(find ./keploy -path '*/tests/*.yaml' 2>/dev/null | wc -l | tr -d ' ')"
    RECORDED_TESTS="$n"
    if [ "${n:-0}" -lt 1 ]; then
        echo "No recorded test cases were generated — cannot measure replay."
        return 1
    fi
    echo "Recorded ${n} test case(s)."
}

check_for_fatal() {
    local logfile="$1"
    [ -f "$logfile" ] || { echo "Log file not found: $logfile"; return 1; }
    if grep -qE 'panic:|fatal error:|WARNING: DATA RACE' "$logfile"; then
        echo "Fatal error / data race detected in $logfile"
        return 1
    fi
    echo "No fatal errors in $logfile."
}

# Replay execution wall-clock (seconds), EXCLUDING docker startup + --delay.
# Every per-test "result" line carries a clean, uncoloured
# "Keploy: <RFC3339Nano>" prefix (utils/log/time.go). Those lines are emitted
# only while test cases actually run — after the app is ready — so the span
# from the first to the last brackets the pure execution phase. Falls back to
# the report's per-test started/completed epochs (whole-second granularity) if
# no result lines parse.
parse_replay_seconds() {
    local log="$1"
    local first last s e
    first="$(grep -a 'testcase id' "$log" 2>/dev/null | head -n 1 | sed -E 's/.*Keploy: ([^ ]+).*/\1/')"
    last="$(grep -a 'testcase id' "$log" 2>/dev/null | tail -n 1 | sed -E 's/.*Keploy: ([^ ]+).*/\1/')"
    if [ -n "$first" ] && [ -n "$last" ]; then
        s="$(date -d "$first" +%s.%N 2>/dev/null || true)"
        e="$(date -d "$last" +%s.%N 2>/dev/null || true)"
        if [ -n "$s" ] && [ -n "$e" ]; then
            awk -v a="$s" -v b="$e" 'BEGIN { d = b - a; if (d < 0) d = 0; printf "%.3f", d }'
            return 0
        fi
    fi
    # Fallback: min(started) .. max(completed) across report test results.
    local report
    report="$(ls -t ./keploy/reports/test-run-*/test-set-*-report.yaml 2>/dev/null | head -n 1 || true)"
    if [ -n "$report" ]; then
        awk '
            /^[[:space:]]*started:/    { v=$2+0; if (mins=="" || v<mins) mins=v }
            /^[[:space:]]*completed:/  { v=$2+0; if (v>maxc) maxc=v }
            END { d = maxc - mins; if (d < 0) d = 0; printf "%.3f", d }
        ' "$report"
        return 0
    fi
    echo "0"
}

collect_replay_profile() {
    mkdir -p "$PERF_OUT_DIR"
    local base prof dest
    base="docker-$(basename "$REPLAY_CPU_PROFILE")"
    dest="$PERF_OUT_DIR/replay-agent-cpu.prof"
    # Agent-written profile (carries the mysql replayer frames).
    prof="$(ls -t "$PWD/$base" 2>/dev/null | head -n 1 || true)"
    if [ -z "$prof" ]; then
        prof="$(ls -t "$PWD"/docker-*.prof 2>/dev/null | head -n 1 || true)"
    fi
    if [ -n "$prof" ] && [ -f "$prof" ]; then
        cp "$prof" "$dest"
        REPLAY_PROFILE_PATH="$dest"
        echo "Collected replay-agent CPU profile: $prof ($(du -h "$prof" | cut -f1)) -> $dest"
    else
        REPLAY_PROFILE_PATH=""
        echo "WARNING: no agent CPU profile (docker-*.prof) found in $PWD."
        ls -la "$PWD"/*.prof 2>/dev/null || true
    fi
    # Best-effort: host-side profile too, if the host process wrote one.
    [ -f "$REPLAY_CPU_PROFILE" ] && cp "$REPLAY_CPU_PROFILE" "$PERF_OUT_DIR/replay-host-cpu.prof" 2>/dev/null || true
}

# =========================================================================
# Main
# =========================================================================
section "Environment"
echo "KEPLOY_BIN=$KEPLOY_BIN"
echo "REPLAY_BIN=$REPLAY_BIN"
"$KEPLOY_BIN" --version || echo "version check failed (non-fatal)"
mkdir -p "$PERF_OUT_DIR"

section "Building sample application image"
docker_build_retry docker compose build

section "Cleaning previous artifacts"
# sudo for keploy/ and keploy-logs.txt: a prior privileged run leaves them
# root-owned, and a stale root-owned keploy-logs.txt makes the (unprivileged)
# `config --generate` below fail on its chmod. Harmless on a fresh runner.
sudo rm -rf keploy/ keploy-logs.txt
rm -f "$RECORD_LOG" "$REPLAY_LOG" "$WORKLOAD_LOG" "$PWD"/docker-*.prof
cleanup_compose

section "Generating Keploy config"
"$KEPLOY_BIN" config --generate

# ---------------------------- RECORD -------------------------------------
section "RECORD: starting keploy record"
run_with_keploy_privileges "$KEPLOY_BIN" record -c "docker compose up" \
    --container-name "$APP_CONTAINER_NAME" \
    --generate-github-actions=false 2>&1 | tee "$RECORD_LOG" &
record_pid=$!
echo "keploy record PID: $record_pid"

# Startup (compose up + agent + MySQL) is NOT part of the measured window.
wait_for_http "$APP_HEALTH_URL" 180

section "RECORD: driving deterministic workload (${PERF_ITERATIONS} iterations)"
rec_start="$(date +%s.%N)"
drive_workload
rec_end="$(date +%s.%N)"
RECORD_SECONDS="$(awk -v a="$rec_start" -v b="$rec_end" 'BEGIN { printf "%.3f", b - a }')"
echo "Record workload wall-clock: ${RECORD_SECONDS}s"

# Fail fast on a broken recording (harness breakage, not a ratio verdict).
check_workload_success

sleep "$RECORD_SETTLE_SECONDS"
stop_keploy_record
wait "$record_pid" || true

check_for_fatal "$RECORD_LOG"
check_recorded_tests

# ---------------------------- REPLAY -------------------------------------
section "REPLAY: preparing"
cleanup_compose

section "REPLAY: starting keploy test with CPU profiling"
# CPU_PROFILE is injected into the keploy-agent container; the agent writes
# docker-<basename> into $PWD (mounted to /tmp/pprof_output).
CPU_PROFILE="$REPLAY_CPU_PROFILE" run_with_keploy_privileges \
    env CPU_PROFILE="$REPLAY_CPU_PROFILE" "$REPLAY_BIN" test -c "docker compose up" \
    --container-name "$APP_CONTAINER_NAME" \
    --api-timeout "$API_TIMEOUT" \
    --delay "$REPLAY_DELAY" \
    --generate-github-actions=false 2>&1 | tee "$REPLAY_LOG" &
replay_pid=$!
echo "keploy test PID: $replay_pid"
wait "$replay_pid" || true

check_for_fatal "$REPLAY_LOG"
REPLAY_SECONDS="$(parse_replay_seconds "$REPLAY_LOG")"
echo "Replay execution wall-clock: ${REPLAY_SECONDS}s"

collect_replay_profile

# ---------------------------- REPORT -------------------------------------
section "RESULT"
RATIO="$(awk -v r="$RECORD_SECONDS" -v p="$REPLAY_SECONDS" \
    'BEGIN { if (p <= 0) { print "0.00"; } else { printf "%.2f", r / p } }')"
MEETS_TARGET="$(awk -v x="$RATIO" -v t="$PERF_TARGET_RATIO" 'BEGIN { print (x >= t) ? "yes" : "no" }')"
if [ "$MEETS_TARGET" = "yes" ]; then
    VERDICT="PASS"
else
    VERDICT="WOULD-FAIL-AT-${PERF_TARGET_RATIO}x"
fi

echo "RECORD=${RECORD_SECONDS} REPLAY=${REPLAY_SECONDS} RATIO=${RATIO} TARGET=${PERF_TARGET_RATIO} (report-only)"

# Machine-readable summaries for the workflow to upload + render.
{
    echo "RECORD_SECONDS=${RECORD_SECONDS}"
    echo "REPLAY_SECONDS=${REPLAY_SECONDS}"
    echo "RATIO=${RATIO}"
    echo "TARGET_RATIO=${PERF_TARGET_RATIO}"
    echo "MEETS_TARGET=${MEETS_TARGET}"
    echo "VERDICT=${VERDICT}"
    echo "RECORDED_TESTS=${RECORDED_TESTS:-0}"
    echo "REQUESTS_SENT=${REQUESTS_SENT}"
    echo "REQUESTS_OK=${REQUESTS_OK}"
    echo "ITERATIONS=${PERF_ITERATIONS}"
    echo "REPLAY_DELAY=${REPLAY_DELAY}"
    echo "REPLAY_PROFILE=${REPLAY_PROFILE_PATH:-}"
    echo "GATE_BLOCKING=${PERF_GATE_BLOCKING}"
} | tee "$SUMMARY_ENV"

cat > "$SUMMARY_JSON" <<JSON
{
  "record_seconds": ${RECORD_SECONDS},
  "replay_seconds": ${REPLAY_SECONDS},
  "ratio": ${RATIO},
  "target_ratio": ${PERF_TARGET_RATIO},
  "meets_target": $( [ "$MEETS_TARGET" = "yes" ] && echo true || echo false ),
  "verdict": "${VERDICT}",
  "recorded_tests": ${RECORDED_TESTS:-0},
  "requests_sent": ${REQUESTS_SENT},
  "requests_ok": ${REQUESTS_OK},
  "iterations": ${PERF_ITERATIONS},
  "replay_delay_seconds": ${REPLAY_DELAY},
  "gate_blocking": "${PERF_GATE_BLOCKING}"
}
JSON
echo "Wrote $SUMMARY_ENV and $SUMMARY_JSON"

# Step summary table (GitHub renders this on the job page).
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    {
        echo "### MySQL replay-vs-record perf (report-only)"
        echo ""
        echo "| Record s | Replay s | Ratio | Target | Verdict |"
        echo "|---------:|---------:|------:|-------:|:--------|"
        echo "| ${RECORD_SECONDS} | ${REPLAY_SECONDS} | ${RATIO}x | ${PERF_TARGET_RATIO}x | ${VERDICT} |"
        echo ""
        echo "_Recorded ${RECORDED_TESTS:-0} test cases from ${REQUESTS_OK}/${REQUESTS_SENT} OK requests. Replay CPU profile uploaded as an artifact._"
    } >> "$GITHUB_STEP_SUMMARY"
fi

# Report-only gate: never fail on the ratio unless explicitly made blocking.
if [ "$MEETS_TARGET" = "no" ]; then
    echo "NOTE: replay is ${RATIO}x vs record; target is ${PERF_TARGET_RATIO}x. This WOULD fail once PERF_GATE_BLOCKING=true."
    if [ "$PERF_GATE_BLOCKING" = "true" ]; then
        echo "PERF_GATE_BLOCKING=true — failing the build on ratio shortfall."
        exit 1
    fi
fi

echo "mysql_replay_perf harness completed successfully (report-only)."
