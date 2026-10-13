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
# And once with a parser that stops behind its traffic (run_stop_lag), held
# back by a gate (KEPLOY_DEBUG_PARSER_GATE) until a healthy connection's test
# case was recorded: the span of what its connection could not record is on
# the capture's clock and ends with what that connection carried, so the
# healthy test case is kept. The run is ordered by events, not by time.

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
    # STOP_AFTER: how long the recording runs on after the last answer. The
    # lag run stops it at once, with the agent's parsers still behind.
    sleep "${STOP_AFTER:-5}"
    sudo pkill -INT keploy 2>/dev/null || true
    return $failed
}

# PARSER_LAG is how far behind the lag run's parsers run: well past the stop's
# quiet bound (3s), so a stop that goes by quiet alone ends before the agent
# has parsed the /login mock.
PARSER_LAG=8s

# STOPPED_SPAN_GAP: how long after /broken was answered /login is sent. The gap
# is the scenario, not a wait for anything to settle. The app answers /broken
# only after closing its upstream connection, so that connection's last chunk
# was stamped before the answer; the span of a stopped connection reaches no
# more than the agent's idle grace past its last chunk while it is open
# (syncMock.UnrecordedIdleGrace, 1s, on the capture's clock), so a test case
# inside the grace is left out by design. Past it by a second, /login's
# verdict does not depend on when that span closes. Load only widens the gap.
STOPPED_SPAN_GAP=2

# wait_for_agent_line LOG PATTERN WHAT: waits for the agent to write a line,
# the event the run moves on by. The bound only ends a broken run.
wait_for_agent_line() {
    local log=$1 pattern=$2 what=$3
    for _ in $(seq 1 240); do
        grep -q "$pattern" "$log" && return 0
        sleep 0.5
    done
    echo "::error::$what: the agent never said so"
    return 1
}

# The stopped-parser run's requests, ordered by events. Every recorded
# connection's parser waits behind its gate (KEPLOY_DEBUG_PARSER_GATE, $2):
# /broken's upstream connection is the first, /login's the second. /broken at
# W, whose upstream answers with bytes that are not HTTP; then /login, past the
# stopped connection's idle grace, on a connection of its own. Only then is
# /broken's parser let through: it stops on those bytes, behind the traffic,
# after /login was recorded, and the run waits until the agent says so ($1,
# the record log). Then every other parser, /login's included: its test case,
# held until its parser is through it, is checked after /broken's stop. Then
# the stop.
drive_stop_requests() {
    local record_log=$1 gate=$2
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
        touch "$gate/all"
        sudo pkill -INT keploy 2>/dev/null || true
        return 1
    fi
    local failed=0 out
    out=$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${APP_PORT}/broken")
    echo "driver: /broken -> $out"
    [ "$out" = 502 ] || { echo "::error::/broken answered $out, want 502"; failed=1; }
    # The gates are numbered in the order the agent records connections: the
    # one /broken made must be the first and only one so far, or gate 1 is
    # not its parser's.
    local gated
    wait_for_agent_line "$record_log" 'waits for its gate' "/broken's upstream connection should have been recorded" || failed=1
    # A gate directory the agent cannot watch opens no gate: say so now, not
    # when the wait for /broken's stop runs out.
    if grep -q 'cannot watch the gate directory' "$record_log"; then
        echo "::error::the agent cannot watch the gate directory: its parsers would never be let through"
        touch "$gate/all"
        sudo pkill -INT keploy 2>/dev/null || true
        return 1
    fi
    gated=$(grep -c 'waits for its gate' "$record_log")
    if [ "$gated" -ne 1 ]; then
        echo "::error::$gated recorded connections before /login, want /broken's alone: the gates would not be the ones this run means"
        failed=1
    elif ! grep 'waits for its gate' "$record_log" | grep -q "\"gate\": \"1\".*\"destination\": \"[^\"]*:${UPSTREAM_PORT}\""; then
        echo "::error::gate 1 is not the connection to /broken's upstream (:${UPSTREAM_PORT}):"
        grep 'waits for its gate' "$record_log"
        failed=1
    fi
    sleep "$STOPPED_SPAN_GAP"
    out=$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' "http://127.0.0.1:${APP_PORT}/login")
    echo "driver: /login -> $out"
    [ "$out" = 200 ] || { echo "::error::/login answered $out, want 200"; failed=1; }
    touch "$gate/1"
    wait_for_agent_line "$record_log" 'a captured exchange was not recorded as a mock' \
        "/broken's parser was let through and should have stopped on its exchange" || failed=1
    touch "$gate/all"
    sudo pkill -INT keploy 2>/dev/null || true
    return $failed
}

# run_stop_lag: the parser of /broken's upstream connection stops on what it
# gets to, behind its traffic: it is let through only after /login was
# recorded (drive_stop_requests). A test case is held until the parsers of the
# connections it used are through it, so /login's, on a healthy connection,
# is checked after that stop. Before, the stop was dated when the parser got
# there (the agent's clock) and /broken's exchange was spanned to it, which
# left /login out too. Now the exchange is spanned to the last byte the parser
# read of it and the stop is dated on the capture's clock, and /login is kept.
# This run checks that outcome end to end; which rule keeps /login when only
# one of them regresses is pinned by the proxy's unit tests
# (TestLeftOutSpan_AStopBehindTheTrafficDoesNotLeaveOutAHealthyConnectionsTestCase).
# /broken's own test case is left out: its mock went with the parser.
run_stop_lag() {
    local format=yaml_stoplag
    echo "=================== $format ==================="
    rm -rf keploy/ keploy.yml
    cat > keploy.yml <<'KEPLOY_CFG'
test:
    globalNoise:
        global: {"header": {"Date":[]}}
KEPLOY_CFG

    python3 upstream.py "$UPSTREAM_PORT" > "upstream_logs_${format}.txt" 2>&1 &
    upstream_pid=$!
    wait_for_port "$UPSTREAM_PORT" upstream || return 1

    local gate
    gate=$(mktemp -d)
    drive_stop_requests "record_logs_${format}.txt" "$gate" > "driver_logs_${format}.txt" 2>&1 &
    local driver_pid=$!
    sudo -E env PATH="$PATH" KEPLOY_DEBUG_PARSER_GATE="$gate" "$RECORD_BIN" record \
        -c "python3 app.py ${APP_PORT} ${UPSTREAM_PORT}" \
        --generateGithubActions=false > "record_logs_${format}.txt" 2>&1
    local record_status=$?
    echo "record exit status: $record_status"
    local driver_ok=0
    wait "$driver_pid" || driver_ok=1
    rm -rf "$gate"
    if [ "$driver_ok" -ne 0 ]; then
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
    if ! grep -q 'KEPLOY_DEBUG_PARSER_GATE is set' "record_logs_${format}.txt" ||
        grep -q 'cannot watch the gate directory' "record_logs_${format}.txt"; then
        echo "::error::the agent did not hold its parsers behind their gates ($format)"
        tail -100 "record_logs_${format}.txt"
        return 1
    fi
    # The scenario happened: the parser stopped on /broken's exchange.
    if ! grep -q 'a captured exchange was not recorded as a mock' "record_logs_${format}.txt"; then
        echo "::error::the broken upstream did not stop its connection's parser ($format)"
        tail -100 "record_logs_${format}.txt"
        return 1
    fi
    if grep -qE 'stopped waiting for the keploy agent|the recording (is|may be) incomplete|the keploy agent ended its (mapping )?stream' "record_logs_${format}.txt"; then
        echo "::error::the stop did not save all the agent held from before it ($format)"
        grep -E 'stopped waiting for the keploy agent|the recording (is|may be) incomplete|the keploy agent ended its (mapping )?stream' "record_logs_${format}.txt"
        return 1
    fi
    if [ "$record_status" -ne 0 ]; then
        echo "::error::record exited $record_status ($format)"
        tail -100 "record_logs_${format}.txt"
        return 1
    fi

    stop_upstream

    local tests_dir=keploy/test-set-0/tests
    if ! grep -rqs 'url: http://127.0.0.1:'"${APP_PORT}"'/login' "$tests_dir"; then
        echo "::error::/login's test case, on a healthy connection, was left out by the span of another connection's stopped parser ($format)"
        ls -la "$tests_dir" 2>/dev/null
        grep -E 'orphan|left out|suppressed' "record_logs_${format}.txt" | tail -20
        return 1
    fi
    if grep -rqs 'url: http://127.0.0.1:'"${APP_PORT}"'/broken' "$tests_dir"; then
        echo "::error::/broken's test case was saved, though its mock went with its connection's parser ($format)"
        return 1
    fi

    sudo -E env PATH="$PATH" "$REPLAY_BIN" test \
        -c "python3 app.py ${APP_PORT} ${UPSTREAM_PORT}" --delay 5 \
        --generateGithubActions=false > "test_logs_${format}.txt" 2>&1
    echo "test exit status: $?"
    local report status
    report=$(ls keploy/reports/test-run-0/test-set-0-report.* 2>/dev/null | head -n1)
    if [ -z "$report" ]; then
        echo "::error::no replay report ($format)"
        tail -100 "test_logs_${format}.txt"
        return 1
    fi
    status=$(grep -m1 '^status:' "$report" | awk '{print $2}')
    echo "replay status ($format): $status"
    if [ "$status" != "PASSED" ]; then
        echo "::error::replay did not pass ($format)"
        tail -60 "test_logs_${format}.txt"
        return 1
    fi
    return 0
}

run_format() {
    # mode: "" for a normal run, "lag" for the run with the capture lagging.
    local format=$1 mode=${2:-} fmt_flag=() lag_env=() stop_after=5
    [ "$format" = json ] && fmt_flag=(--storage-format json)
    if [ "$mode" = lag ]; then
        lag_env=(KEPLOY_DEBUG_PARSER_LAG="$PARSER_LAG")
        stop_after=0
        format="${format}_lag"
    fi
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

    STOP_AFTER=$stop_after drive_requests > "driver_logs_${format}.txt" 2>&1 &
    local driver_pid=$!
    sudo -E env PATH="$PATH" "${lag_env[@]}" "$RECORD_BIN" record "${fmt_flag[@]}" \
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
    # So no stop may say that what it was still saving is not saved: it said
    # so at the end of every recording, which had lost nothing.
    if grep -q 'what Keploy was still saving is not saved' "record_logs_${format}.txt"; then
        echo "::error::a clean stop said that what Keploy was still saving is not saved ($format)"
        grep -n 'Signal received\|agent stop\|has not exited' "record_logs_${format}.txt"
        return 1
    fi
    if [ "$mode" = lag ]; then
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
run_stop_lag || exit 1
echo "PASS: a parser that stops behind its traffic leaves out only what its own connection carried"
exit 0
