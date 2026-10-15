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
#
# Then once more with the capture lagging: KEPLOY_DEBUG_PARSER_LAG makes every
# recorded connection's parser run that far behind its traffic, as a starved
# agent's do, while the app is answered as fast as ever, and the recording is
# stopped the moment the last request is answered. The agent then still holds
# the upstream's mock when the stop comes, for longer than the stop's quiet
# bound, and every test case completes before its mock is parsed. The run
# passes only when the stop waits for the parsers (the agent's capture
# watermark, /agent/record/pending) and the agent holds each test case until
# its mocks are in: otherwise the /login test is saved without its mock, and
# its replay, with the upstream gone, fails.
#
# And once more lagging, recorded by a non-root CLI, which runs its agent with
# `sudo -n`, the stop delivered as `sudo pkill -INT -f keploy` delivers it: to
# keploy, its agent, and the sudo that runs the agent, which hands its own copy
# on, so the agent gets the stop twice. The agent holds the first until its
# client has drained it; the copy, taken for a second Ctrl+C, cut that short,
# and the recording lost what the client had not drained and failed.
#
# And twice more with --sync, which records each test's mapping to its mocks:
# once lagging, and once busy -- BUSY_REQS calls to /login at once, the stop
# the moment the last is answered, keploy and the app on two CPUs shared with
# two busy loops where the runner has four or more. Each must save every test
# case, its mock and its mapping, its drains ending on the agent's end markers
# (the agent's summary says ended_by "end"): the stop ends each stream once
# the agent has put on it all it had from before the stop, mappings included,
# not once it has been quiet for a while.

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
# The non-root run's agent runs under sudo, whose env_reset would drop the lag.
SUDOERS_LAG=/etc/sudoers.d/keploy-repeated-headers-lag
# The busy run's CPU hogs.
hog_pids=""
cleanup() {
    echo "cleanup..."
    [ -n "$hog_pids" ] && kill $hog_pids 2>/dev/null
    sudo pkill -9 keploy 2>/dev/null || true
    sudo rm -f "$SUDOERS_LAG"
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
    # STOP_AFTER: how long the recording runs on after the last answer. The
    # lag run stops it at once, with the agent's parsers still behind.
    sleep "${STOP_AFTER:-5}"
    sudo pkill -INT keploy 2>/dev/null || true
    if [ -n "${STOP_COPY:-}" ]; then
        # What `sudo pkill -INT -f keploy` also signals: the sudo that runs
        # the agent, which hands the agent its own copy of the stop.
        sudo pkill -INT -f "^sudo -n [^ ]*keploy agent" 2>/dev/null || true
    fi
    return $failed
}

# PARSER_LAG is how far behind the lag run's parsers run: well past the stop's
# quiet bound (3s), so a stop that goes by quiet alone ends before the agent
# has parsed the /login mock.
PARSER_LAG=8s

# ended_by_markers checks that a recording's drains ended on the agent's end
# markers: the agent's summary says its test-case stream ended so, and the
# CLI neither failed to ask for them nor gave a drain up on its stall bound.
ended_by_markers() {
    local format=$1
    if ! grep -q '"ended_by": "end"' "record_logs_${format}.txt"; then
        echo "::error::the agent did not end its streams with its markers: the drains ended some other way ($format)"
        grep -n 'recording complete' "record_logs_${format}.txt"
        return 1
    fi
    if grep -Eq 'could not ask the keploy agent to end its streams|teardown drain made no progress' "record_logs_${format}.txt"; then
        echo "::error::a drain did not end at the agent's markers: the CLI could not ask for them, or gave the drain up ($format)"
        grep -En 'could not ask the keploy agent to end its streams|teardown drain made no progress' "record_logs_${format}.txt"
        return 1
    fi
}

# run_sync_busy records BUSY_REQS calls to /login at once with --sync, stops
# the moment the last is answered, and checks that every test case, its mock
# and its mapping were saved. Where the runner has four CPUs or more, keploy
# and the app share two with two busy loops.
run_sync_busy() {
    local format=yaml_sync_busy n=${BUSY_REQS:-200} pin=()
    echo "=================== $format ==================="
    sudo rm -rf keploy/ keploy.yml
    cat > keploy.yml <<'KEPLOY_CFG'
test:
    globalNoise:
        global: {"header": {"Date":[]}}
KEPLOY_CFG
    python3 upstream.py "$UPSTREAM_PORT" > "upstream_logs_${format}.txt" 2>&1 &
    upstream_pid=$!
    wait_for_port "$UPSTREAM_PORT" upstream || return 1
    if [ "$(nproc)" -ge 4 ]; then
        pin=(taskset -c 0-1)
        for _ in 1 2; do
            taskset -c 0-1 sh -c 'while :; do :; done' &
            hog_pids="$hog_pids $!"
        done
    fi
    (
        for _ in $(seq 1 60); do
            curl -fsS --max-time 5 -o /dev/null "http://127.0.0.1:${APP_PORT}/health" && break
            sleep 1
        done
        seq 1 "$n" | xargs -P 16 -I{} curl -sS --max-time 30 -o /dev/null "http://127.0.0.1:${APP_PORT}/login"
        sudo pkill -INT keploy 2>/dev/null || true
    ) > "driver_logs_${format}.txt" 2>&1 &
    local driver_pid=$!
    sudo -E "${pin[@]}" env PATH="$PATH" "$RECORD_BIN" record --sync \
        -c "python3 app.py ${APP_PORT} ${UPSTREAM_PORT}" \
        --generateGithubActions=false > "record_logs_${format}.txt" 2>&1
    local record_status=$?
    [ -n "$hog_pids" ] && kill $hog_pids 2>/dev/null
    hog_pids=""
    wait "$driver_pid"
    stop_upstream
    echo "record exit status: $record_status"
    if [ "$record_status" -ne 0 ] || grep -qE 'stopped waiting for the keploy agent|the recording (is|may be) incomplete|the keploy agent ended its (mapping )?stream|stopped saving what the agent captured' "record_logs_${format}.txt"; then
        echo "::error::the busy --sync recording did not save all it captured ($format)"
        tail -100 "record_logs_${format}.txt"
        return 1
    fi
    local logins mocks mapped
    logins=$(grep -l "url: http://127.0.0.1:${APP_PORT}/login" keploy/test-set-0/tests/*.yaml 2>/dev/null | wc -l)
    mocks=$(grep -c '^kind:' keploy/test-set-0/mocks.yaml 2>/dev/null)
    mapped=$(grep -cE '^[[:space:]]*- id: test-' keploy/test-set-0/mappings.yaml 2>/dev/null)
    echo "busy --sync recording: $logins /login test cases, $mocks mocks, $mapped tests mapped, of $n calls"
    if [ "$logins" -ne "$n" ] || [ "$mocks" -ne "$n" ] || [ "$mapped" -ne "$n" ]; then
        echo "::error::a busy --sync recording lost test cases, mocks or mappings: $logins test cases, $mocks mocks, $mapped mapped, of $n calls ($format)"
        tail -60 "record_logs_${format}.txt"
        return 1
    fi
    ended_by_markers "$format" || return 1
}

run_format() {
    # mode: "" for a normal run, "lag" for the run with the capture lagging,
    # "sudo_lag" for that run recorded by a non-root CLI (its agent under
    # sudo), stopped as `sudo pkill -INT -f keploy` stops it, "sync_lag" for
    # the lagging run recorded with --sync (each test's mapping).
    local format=$1 mode=${2:-} fmt_flag=() lag_env=() stop_after=5 as_root=(sudo -E) stop_copy="" sync_flag=()
    [ "$format" = json ] && fmt_flag=(--storage-format json)
    [ "$mode" = sync_lag ] && sync_flag=(--sync)
    if [ "$mode" = lag ] || [ "$mode" = sudo_lag ] || [ "$mode" = sync_lag ]; then
        lag_env=(KEPLOY_DEBUG_PARSER_LAG="$PARSER_LAG")
        stop_after=0
        format="${format}_${mode}"
    fi
    if [ "$mode" = sudo_lag ]; then
        as_root=()
        stop_copy=1
        echo 'Defaults env_keep += "KEPLOY_DEBUG_PARSER_LAG"' | sudo tee "$SUDOERS_LAG" >/dev/null
        sudo chmod 0440 "$SUDOERS_LAG"
        if ! sudo -n true; then
            echo "::error::the non-root run needs passwordless sudo, as the agent's sudo -n does"
            return 1
        fi
    fi
    echo "=================== $format ==================="
    # sudo: an earlier run's replay ran as root, and wrote its reports so.
    sudo rm -rf keploy/ keploy.yml
    # The app's http.server stamps each response with the current Date.
    cat > keploy.yml <<'KEPLOY_CFG'
test:
    globalNoise:
        global: {"header": {"Date":[]}}
KEPLOY_CFG

    python3 upstream.py "$UPSTREAM_PORT" > "upstream_logs_${format}.txt" 2>&1 &
    upstream_pid=$!
    wait_for_port "$UPSTREAM_PORT" upstream || return 1

    STOP_AFTER=$stop_after STOP_COPY=$stop_copy drive_requests > "driver_logs_${format}.txt" 2>&1 &
    local driver_pid=$!
    "${as_root[@]}" env PATH="$PATH" "${lag_env[@]}" "$RECORD_BIN" record "${fmt_flag[@]}" "${sync_flag[@]}" \
        -c "python3 app.py ${APP_PORT} ${UPSTREAM_PORT}" \
        --generateGithubActions=false > "record_logs_${format}.txt" 2>&1
    local record_status=$?
    echo "record exit status: $record_status"
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
    # The stop signals the agent too (pkill matches it), and it holds that
    # signal until the recording's client has taken all it captured. The
    # client then stops it: an agent already on its way out is waited for,
    # not signalled again, unless it is still running past a grace (a busy
    # runner's slow teardown can be), and then it has nothing left unsaved.
    # So no stop may give up on what it was still saving: neither the
    # client's drain, nor the agent's hold (as one before this lane cut it
    # short at a copy of the stop).
    if grep -qE 'what Keploy was still saving is not saved|stopped saving what the agent captured before the stop' "record_logs_${format}.txt"; then
        echo "::error::a stop gave up on what Keploy was still saving ($format)"
        grep -nE 'Signal received|not saved|stopped saving|agent stop|has not exited' "record_logs_${format}.txt"
        return 1
    fi
    if [ "$mode" = sudo_lag ]; then
        # The run tested a copy of the stop only if the agent held for its
        # client, and the copy came while it did.
        if ! grep -q "a recording's client is still saving what this agent captured before the stop" "record_logs_${format}.txt"; then
            echo "::error::the agent did not hold its stop for its client: the run tested nothing of a copy of the stop ($format)"
            grep -n 'Signal received' "record_logs_${format}.txt"
            return 1
        fi
        if ! grep -q 'Keploy stops once its client has saved what it still holds for it' "record_logs_${format}.txt"; then
            echo "::error::the agent did not get a copy of the stop while it held it: the run tested nothing of one ($format)"
            grep -n 'Signal received' "record_logs_${format}.txt"
            return 1
        fi
    fi
    if [ -n "$mode" ]; then
        # The lag was really induced: the agent says so at WARN when it is.
        if ! grep -q 'KEPLOY_DEBUG_PARSER_LAG is set' "record_logs_${format}.txt"; then
            echo "::error::the agent did not run its parsers behind the traffic: KEPLOY_DEBUG_PARSER_LAG had no effect ($format)"
            tail -100 "record_logs_${format}.txt"
            return 1
        fi
        # A stop that gave up on what the agent held, or lost it to an agent
        # that went away, reports it, and fails.
        if grep -qE 'stopped waiting for the keploy agent|the recording (is|may be) incomplete|the keploy agent ended its (mapping )?stream' "record_logs_${format}.txt"; then
            echo "::error::the stop did not save all the agent held from before it ($format)"
            grep -E 'stopped waiting for the keploy agent|the recording (is|may be) incomplete|the keploy agent ended its (mapping )?stream' "record_logs_${format}.txt"
            return 1
        fi
        if [ "$record_status" -ne 0 ]; then
            echo "::error::record exited $record_status with its capture lagging ($format)"
            tail -100 "record_logs_${format}.txt"
            return 1
        fi
    fi
    if [ "$mode" = sync_lag ]; then
        local login
        login=$(grep -l '/login' keploy/test-set-0/tests/*.yaml 2>/dev/null | head -1 | xargs -r basename | sed 's/\.yaml$//')
        if [ -z "$login" ] || ! grep -qE "^[[:space:]]*- id: ${login}\$" keploy/test-set-0/mappings.yaml 2>/dev/null; then
            echo "::error::the /login test ($login) has no mapping to its mock ($format)"
            cat keploy/test-set-0/mappings.yaml 2>/dev/null
            return 1
        fi
        ended_by_markers "$format" || return 1
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
run_format yaml lag || exit 1
echo "PASS: a recording whose capture lags its traffic keeps every test case and its mocks"
run_format yaml sudo_lag || exit 1
echo "PASS: a non-root CLI's recording keeps every test case and its mocks when its agent gets the stop twice"
run_format yaml sync_lag || exit 1
echo "PASS: a --sync recording whose capture lags keeps each test's mapping, its drains ending on the agent's markers"
run_sync_busy || exit 1
echo "PASS: a busy --sync recording keeps every test case, mock and mapping, its drains ending on the agent's markers"
exit 0
