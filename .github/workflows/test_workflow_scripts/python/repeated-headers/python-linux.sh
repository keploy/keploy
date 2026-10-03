#!/bin/bash
# Guard: replay sends a recorded header on the lines it arrived on.
#
# keploy stores a header as one value per name, the wire lines joined by ",",
# and used to send that join back as ONE line. `X_tenant: acme` twice replayed
# as `X_tenant: acme,acme`, which the app's tenant validation rejects, as a
# production service's did for every such recorded test. The mock side had the
# same fold: two Set-Cookie lines from a dependency were served as one cookie.
#
# app.py rejects a folded tenant and returns the cookies its upstream sent, so
# the replay passes only when both the test's request and the mock's response
# go out on their recorded lines. A single line that carries commas
# (`X_tenant: acme, globex`, like `Accept: a, b`) must stay one line: split, it
# would turn a recorded 400 into a 200.
#
# Runs the whole record -> replay in each storage format (yaml, then json when
# both binaries support it), since the line lengths must survive both.

set -uo pipefail
# Explicit `set +e`: exit codes are captured explicitly and cleanup always runs.
set +e

source "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/test-iid.sh"
source "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/json-pass-helpers.sh"

REPLAY_BIN="${REPLAY_BIN:-$RECORD_BIN}"
APP_PORT=8000
UPSTREAM_PORT=9092

# keploy is matched by process name (pkill default): `pkill -f` would match the
# wrapping bash/sudo argv. The upstream is killed by PID, since "python3"
# matches every Python process on the runner.
upstream_pid=""
stop_upstream() {
    if [ -n "$upstream_pid" ]; then
        kill -9 "$upstream_pid" 2>/dev/null || true
        wait "$upstream_pid" 2>/dev/null || true
        upstream_pid=""
    fi
}
cleanup() {
    echo "cleanup..."
    sudo pkill -9 keploy 2>/dev/null || true
    stop_upstream
    sleep 1
}
trap cleanup EXIT

cd "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/python/repeated-headers" || exit 1

wait_for_port() {
    local port=$1 what=$2
    for i in $(seq 1 30); do
        if ss -tln | awk '{print $4}' | grep -q ":${port}$"; then
            echo "$what listening after ${i}s"
            return 0
        fi
        sleep 1
    done
    echo "::error::$what did not start listening on :$port within 30s"
    return 1
}

# The recording's requests. Each curl prints the status and body it got, so
# the record log shows what the app answered live.
drive_requests() {
    local up=0
    for _ in $(seq 1 60); do
        if curl -fsS --max-time 5 "http://127.0.0.1:${APP_PORT}/health" >/dev/null 2>&1; then
            up=1
            break
        fi
        sleep 1
    done
    if [ "$up" -ne 1 ]; then
        echo "::error::app /health did not become ready within 60s"
        sudo pkill -INT keploy 2>/dev/null || true
        return 1
    fi
    local failed=0
    expect() {
        local want=$1
        shift
        local out got
        out=$(curl -sS --max-time 10 -w '\n%{http_code}' "$@")
        got=${out##*$'\n'}
        echo "driver: $* -> $got ${out%$'\n'*}"
        if [ "$got" != "$want" ]; then
            echo "::error::live request $* answered $got, want $want"
            failed=1
        fi
    }
    # The header on two lines: valid tenants both, so 200.
    expect 200 -H 'X_tenant: acme' -H 'X_tenant: acme' "http://127.0.0.1:${APP_PORT}/tenant"
    # One line: 200.
    expect 200 -H 'X_tenant: globex' "http://127.0.0.1:${APP_PORT}/tenant"
    # One line with a comma in it: 400, and it must replay as 400.
    expect 400 -H 'X_tenant: acme, globex' "http://127.0.0.1:${APP_PORT}/tenant"
    # Two Set-Cookie lines from the upstream.
    expect 200 "http://127.0.0.1:${APP_PORT}/login"
    sleep 5
    sudo pkill -INT keploy 2>/dev/null || true
    return $failed
}

run_format() {
    local format=$1 fmt_flag=()
    [ "$format" = json ] && fmt_flag=(--storage-format json)
    echo "=================== $format ==================="
    rm -rf keploy/ keploy.yml
    # The app's http.server stamps each response with the current Date.
    cat > keploy.yml <<'KEPLOY_CFG'
test:
    globalNoise:
        global: {"header": {"Date":[]}}
KEPLOY_CFG

    python3 upstream.py "$UPSTREAM_PORT" > "upstream_logs_${format}.txt" 2>&1 &
    upstream_pid=$!
    wait_for_port "$UPSTREAM_PORT" upstream || return 1

    drive_requests > "driver_logs_${format}.txt" 2>&1 &
    local driver_pid=$!
    sudo -E env PATH="$PATH" "$RECORD_BIN" record "${fmt_flag[@]}" \
        -c "python3 app.py ${APP_PORT} ${UPSTREAM_PORT}" \
        --generateGithubActions=false > "record_logs_${format}.txt" 2>&1
    echo "record exit status: $?"
    if ! wait "$driver_pid"; then
        echo "::error::the request driver failed while recording ($format)"
        cat "driver_logs_${format}.txt"
        tail -100 "record_logs_${format}.txt"
        return 1
    fi
    cat "driver_logs_${format}.txt"
    if grep -q 'WARNING: DATA RACE' "record_logs_${format}.txt"; then
        echo "::error::data race detected during record ($format)"
        tail -200 "record_logs_${format}.txt"
        return 1
    fi

    # The upstream goes away: at replay only the mock can answer /login.
    stop_upstream

    local tests_dir=keploy/test-set-0/tests
    local ntests
    ntests=$(find "$tests_dir" -type f \( -name '*.yaml' -o -name '*.json' \) 2>/dev/null | wc -l)
    if [ "$ntests" -lt 5 ]; then
        echo "::error::recorded $ntests test cases, want 5 (health + 3 tenant + login)"
        ls -la "$tests_dir" 2>/dev/null
        tail -100 "record_logs_${format}.txt"
        return 1
    fi
    # What reached disk: line lengths for the two-line tenant and for the
    # mock's Set-Cookie, and nothing new anywhere else.
    if ! grep -rqs 'header_line_lengths' "$tests_dir"; then
        echo "::error::no test case recorded the lines of its repeated X_tenant"
        return 1
    fi
    local with_lengths
    with_lengths=$(grep -rls 'header_line_lengths' "$tests_dir" | wc -l)
    if [ "$with_lengths" -ne 1 ]; then
        echo "::error::$with_lengths test cases store header_line_lengths, want exactly the one with a repeated header"
        grep -rl 'header_line_lengths' "$tests_dir"
        return 1
    fi
    if ! grep -qs 'header_line_lengths' keploy/test-set-0/mocks.*; then
        echo "::error::the upstream's mock did not record the lines of its two Set-Cookie headers"
        return 1
    fi

    sudo -E env PATH="$PATH" "$REPLAY_BIN" test "${fmt_flag[@]}" \
        -c "python3 app.py ${APP_PORT} ${UPSTREAM_PORT}" --delay 5 \
        --generateGithubActions=false > "test_logs_${format}.txt" 2>&1
    echo "test exit status: $?"
    if grep -q 'WARNING: DATA RACE' "test_logs_${format}.txt"; then
        echo "::error::data race detected during replay ($format)"
        tail -200 "test_logs_${format}.txt"
        return 1
    fi

    local report status
    report=$(ls keploy/reports/test-run-0/test-set-0-report.* 2>/dev/null | head -n1)
    if [ -z "$report" ]; then
        echo "::error::no replay report ($format)"
        tail -100 "test_logs_${format}.txt"
        return 1
    fi
    if [ "$format" = json ]; then
        status=$(jq -r '.status' "$report")
    else
        status=$(grep -m1 '^status:' "$report" | awk '{print $2}')
    fi
    echo "replay status ($format): $status"
    if [ "$status" != "PASSED" ]; then
        echo "::error::replay did not pass ($format): a header was not sent on its recorded lines"
        grep -E 'Tenant :|not valid tenant|Set-Cookie|cookies' "$report" | head -20
        tail -60 "test_logs_${format}.txt"
        return 1
    fi
    return 0
}

run_format yaml || exit 1
if json_pass_supported; then
    run_format json || exit 1
else
    echo "json pass skipped: a binary does not support --storage-format"
fi
echo "PASS: repeated headers replay on their recorded lines"
exit 0
