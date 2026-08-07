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

# The sample checks in its own keploy.yml carrying the globalNoise that ignores
# the wall-clock `duration` field these endpoints report (see samples-go
# proxy-stress-test/keploy.yml). `config --generate` above is a no-op when that
# file exists, so the noise applies here and to a bare `./test.sh` run alike —
# deliberately not patched in from CI, so the two cannot drift.
grep -q 'duration' ./keploy.yml || {
    echo "FAIL: the sample's keploy.yml is missing the duration noise; without it"
    echo "      /api/transfer, /api/batch-transfer and /api/post-transfer mismatch on every replay."
    exit 1
}

container_kill() {
    REC_PID="$(pgrep -n -f "$(basename "${RECORD_BIN:-keploy}") record" || true)"
    echo "Keploy record PID: $REC_PID"
    sudo kill -INT "$REC_PID" 2>/dev/null || true
}

send_request(){
    # The app's /health does a DB ping and the app only starts after postgres
    # is service_healthy; under CI contention + keploy interception this startup
    # occasionally exceeds the old 120s budget, so give it generous headroom.
    sleep 10
    max_attempts=80
    attempt=0
    while ! curl -sf --max-time 5 http://localhost:8080/health > /dev/null 2>&1; do
        attempt=$((attempt + 1))
        if [ "$attempt" -ge "$max_attempts" ]; then
            echo "App failed to start"
            # CRITICAL: stop keploy before bailing. send_request runs in the
            # background; the foreground `keploy record` only stops on this
            # SIGINT. Without it, a flaky slow app-start left keploy recording
            # forever — observed as a ~6h CI hang ending only at the job
            # timeout (3 SIGINTs delivered for 4 record iterations).
            container_kill
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
    # timeout -s INT: a hard ceiling so a never-stopped record can never hang
    # the job for hours, even if send_request dies before reaching container_kill.
    # SIGINT is the graceful keploy stop; a normal iteration is well under a
    # minute, so 600s is generous headroom over the 80x3s health budget.
    # shellcheck disable=SC2086
    timeout -s INT 600 $RECORD_BIN record $extra_flags -c "docker compose up" --container-name "$container_name" --generateGithubActions=false |& tee "$log"
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
source "${GITHUB_WORKSPACE:-${PWD%/samples-*}}/.github/workflows/test_workflow_scripts/json-pass-helpers.sh"

if json_pass_supported; then
    # State-bleed guard. The yaml iterations populated postgres (via
    # docker-compose volume) with rows whose IDs were assigned starting
    # from an empty DB. Recording the same traffic again with the
    # accumulated state captures bind values that reference rows only
    # the polluted DB had — postgres-v3's transactional matcher then
    # fails at replay-time with "no recorded invocation shares this SQL
    # hash". `docker compose down -v` clears the named volumes so the
    # json record pass starts from the same blank state.
    timeout 120 docker compose down -v || true
    sleep 2
    for i in {1..2}; do
        do_record_iteration "$i" "--storage-format json"
    done
fi

# Match any *.yaml under a tests/ dir so we are agnostic to the testcase
# naming scheme (legacy "test-N.yaml" vs descriptive slugs).
test_count=$(find ./keploy -name '*.yaml' -path '*/tests/*' 2>/dev/null | wc -l)
echo "Total recorded test cases: $test_count"
if [ "$test_count" -eq 0 ]; then echo "FAIL: No test cases recorded"; exit 1; fi

echo "Shutting down services before test mode..."
timeout 120 docker compose down || true
echo "Services stopped"

test_container="proxyStressApp"
# timeout -s INT: hard ceiling so a stuck replay can't hang the job (matches the
# record path); the "No reports — replay hung" guard below then fails it fast.
set +e
timeout -s INT 600 $REPLAY_BIN test -c 'docker compose up' --containerName "$test_container" --apiTimeout 60 --delay 15 --generate-github-actions=false |& tee "${test_container}.txt"
replay_rc=${PIPESTATUS[0]}
set -e

if grep "WARNING: DATA RACE" "${test_container}.txt"; then echo "FAIL: Data race during replay"; exit 1; fi
if grep -q "panic:" "${test_container}.txt"; then echo "FAIL: Panic during replay"; cat "${test_container}.txt"; exit 1; fi

report_count=$(find ./keploy/reports -name '*-report.yaml' 2>/dev/null | wc -l)
echo "Test reports generated: $report_count"
if [ "$report_count" -eq 0 ]; then echo "FAIL: No reports — replay hung"; cat "${test_container}.txt"; exit 1; fi
if grep -q "Error channel is full" "${test_container}.txt"; then echo "FAIL: Error channel overflow"; cat "${test_container}.txt"; exit 1; fi
if grep -q "incomplete or invalid response packet" "${test_container}.txt"; then echo "FAIL: PG decode failure"; cat "${test_container}.txt"; exit 1; fi

# Enforce the report status. This loop used to only `echo` it, so the lane was
# green through four consecutive FAILED test sets — 11 of 18 tests failing —
# and had been for weeks. Everything else in this script (data race, panic,
# zero test cases, zero reports) checks for the run not happening; nothing
# checked whether it passed.
all_passed=true
seen_reports=0
for report_file in ./keploy/reports/test-run-0/test-set-*-report.yaml; do
    [ -f "$report_file" ] || continue
    seen_reports=$((seen_reports + 1))
    status=$(grep 'status:' "$report_file" | head -1 | awk '{print $2}')
    echo "$(basename "$report_file"): $status"
    if [ "$status" != "PASSED" ]; then
        all_passed=false
    fi
done
# A glob that matched nothing leaves the loop body unexecuted, which would
# otherwise sail through as "all passed".
if [ "$seen_reports" -eq 0 ]; then
    echo "FAIL: no test-set reports under ./keploy/reports/test-run-0"
    cat "${test_container}.txt"
    exit 1
fi
# Every recorded test-set must have produced a report. Checking only the
# reports that exist means a test-set whose replay died before writing one is
# invisible: the survivors all say PASSED and the loop above is satisfied.
recorded_sets=$(find ./keploy -maxdepth 1 -type d -name 'test-set-*' | wc -l)
if [ "$seen_reports" -ne "$recorded_sets" ]; then
    echo "FAIL: $recorded_sets test-sets recorded but $seen_reports reported — they must match; a replay died before writing its report"
    cat "${test_container}.txt"
    exit 1
fi
if [ "$all_passed" != true ]; then
    echo "FAIL: one or more yaml test sets did not pass"
    cat "${test_container}.txt"
    exit 1
fi
# Asserted last so the per-report diagnostics above are printed first. Catches
# anything keploy fails at AFTER the final report is written — coverage,
# mock pruning, teardown — which leaves every report PASSED and would
# otherwise go green.
if [ "$replay_rc" -ne 0 ]; then
    echo "FAIL: every test set passed but keploy exited $replay_rc"
    cat "${test_container}.txt"
    exit 1
fi

if json_pass_supported; then
    # Reuse the compose service name (`proxyStressApp`) — there is no
    # `${container}_json` service in docker-compose. Format dispatch is
    # auto-detected per file by NewMockReaderAny / GetTestCases on the
    # replay side, so the --storage-format flag here only affects what
    # extension the json *report* gets written under.
    set +e
    timeout -s INT 600 $REPLAY_BIN test --storage-format json -c 'docker compose up' --containerName "$test_container" --apiTimeout 60 --delay 15 --generate-github-actions=false |& tee "${test_container}_json.txt"
    json_replay_rc=${PIPESTATUS[0]}
    set -e
    if grep "WARNING: DATA RACE" "${test_container}_json.txt"; then echo "FAIL: Data race during json replay"; exit 1; fi
    if grep -q "panic:" "${test_container}_json.txt"; then echo "FAIL: Panic during json replay"; cat "${test_container}_json.txt"; exit 1; fi
    # json_scan_reports already emits ::error:: annotations for FAILED sets and
    # returns non-zero. The `|| true` that used to be here threw that away, so
    # the lane printed four "##[error] ... status=FAILED" lines and then
    # declared itself passed on the very next line.
    if ! json_scan_reports; then
        echo "FAIL: one or more json test sets did not pass"
        cat "${test_container}_json.txt"
        exit 1
    fi
    # json_scan_reports only inspects the reports that EXIST — the same hole the
    # yaml pass has above, so it needs the same denominator. A test set whose
    # replay died before writing its report is otherwise invisible: the
    # survivors all say PASSED and the scan is satisfied.
    json_reports=$(find ./keploy/reports -type f -path '*/test-run-*/test-set-*-report.json' | wc -l)
    if [ "$json_reports" -ne "$recorded_sets" ]; then
        echo "FAIL: $recorded_sets test-sets recorded but only $json_reports json reports written — a replay died before writing its report"
        cat "${test_container}_json.txt"
        exit 1
    fi
    if [ "$json_replay_rc" -ne 0 ]; then
        echo "FAIL: every json test set passed but keploy exited $json_replay_rc"
        cat "${test_container}_json.txt"
        exit 1
    fi
    echo "Proxy stress test PASSED — yaml + json"
else
    echo "Proxy stress test PASSED — yaml only (json pass skipped for compat-matrix cell)"
fi
