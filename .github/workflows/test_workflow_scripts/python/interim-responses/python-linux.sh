#!/bin/bash
# Guard: an interim (1xx) response is forwarded to the client, and the
# exchange is recorded with the final response.
#
# net/http's ReadResponse returns an interim response as if it were the
# answer. The ingress forwarded only the app's 103 Early Hints (or 102
# Processing) and recorded it, with no body, as the test's response; the
# client never got the answer. A client that sends "Expect: 100-continue"
# waits for the app's 100 before it sends its body, and the ingress forwarded
# the 100 only once it had the body: curl waited, then got the 100 as the
# answer.
#
# app.py sends a 103, a 102 and a 100 before its final responses. curl drives
# them through keploy record, in the default mode and in --sync (each with
# its own loop in the ingress), and must get every interim response and then
# the answer, at once. The test cases must hold the final responses, and
# replay them PASSED. /deny sends its 100 and answers before it reads the
# body: curl, given the 100, sends the body anyway, and the test case must
# hold it whole (the ingress cut it off when the answer came, and recorded
# the exchange or not depending on which came first). /reject answers as Go's
# server does when its handler does not read the body, with no 100: curl
# sends the body once it stops waiting for the 100, and the test case must
# hold it too.
#
# It also guards that a run leaves none of keploy's CA trust files behind.
# Every native run, whatever the app's language, wrote the app's CA file
# (keploy-ca-*.crt, one more per run) and a Java truststore
# (keploy-java-truststore-*, built in Go, one per user, rewritten each run)
# to the temp dir, and never removed them: root's, here. keploy record and
# keploy test run with TMPDIR at a dir of this job's own, so what is left
# there is theirs and no other job's. A run's files go in its user's dir in
# the temp dir (keploy-0 for root), so the checks look at every depth. While
# each record runs, its CA file must be there (so the check below looks where
# keploy writes); once each has ended (record stopped by SIGINT, test
# finishing on its own) no keploy-ca-*.crt or keploy-java-* may be left.

set -uo pipefail
# Explicit `set +e`: exit codes are captured explicitly and cleanup always runs.
set +e

source "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/test-iid.sh"

REPLAY_BIN="${REPLAY_BIN:-$RECORD_BIN}"
APP_PORT=8000

# keploy is matched by process name (pkill default): `pkill -f` would match the
# wrapping bash/sudo argv.
cleanup() {
    echo "cleanup..."
    sudo pkill -9 keploy 2>/dev/null || true
    sleep 1
    [ -n "${KEPLOY_TMPDIR:-}" ] && sudo rm -rf -- "$KEPLOY_TMPDIR"
}
trap cleanup EXIT

# keploy's temp dir: os.TempDir() follows TMPDIR. No sudo passes TMPDIR (glibc
# drops it for every setuid program, so neither -E nor env_keep keeps it), so
# each keploy command gets it after sudo, from env.
KEPLOY_TMPDIR=$(mktemp -d "${RUNNER_TEMP:-/tmp}/interim-responses-tmp.XXXXXX") || exit 1

# no_temp_left WHAT: once WHAT has ended, none of keploy's CA trust files may
# be left in its temp dir. Whatever else is there is only listed.
no_temp_left() {
    local what=$1 left other
    left=$(sudo find "$KEPLOY_TMPDIR" -mindepth 1 \( -name 'keploy-ca-*.crt' \
        -o -name 'keploy-java-*' \) -printf '%P ') || {
        echo "::error::cannot list keploy's temp dir ${KEPLOY_TMPDIR} after ${what}, so nothing was checked"
        return 1
    }
    if [ -n "$left" ]; then
        echo "::error::${what} left its temp files behind (each run must remove its own when it ends): ${left}"
        sudo ls -laR "$KEPLOY_TMPDIR"
        return 1
    fi
    other=$(sudo find "$KEPLOY_TMPDIR" -mindepth 1 -printf '%P ')
    [ -z "$other" ] || echo "note: ${what} left in its temp dir (not checked here): ${other}"
    return 0
}

cd "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/python/interim-responses" || exit 1

# The recording's requests. Each must come back with its interim responses,
# then its final one, within --max-time: --expect100-timeout keeps curl waiting
# for the 100 longer than that, so a 100 that is not forwarded until the body
# has gone fails here instead of being papered over by curl's 1 s default.
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
    # keploy writes the app's CA file before it starts the app, so it is in
    # keploy's temp dir by now: else keploy is not writing there (TMPDIR lost
    # on the way, a renamed file) and the leak check would pass unseen.
    if [ -z "$(sudo find "$KEPLOY_TMPDIR" -mindepth 1 -name 'keploy-ca-*.crt' -print -quit)" ]; then
        echo "::error::keploy record's CA file (keploy-ca-*.crt) is not in its temp dir ${KEPLOY_TMPDIR} while it runs, so the temp-file check would see nothing"
        sudo ls -laR "$KEPLOY_TMPDIR"
        sudo pkill -INT keploy 2>/dev/null || true
        return 1
    fi
    local failed=0
    # expect <final code> <interim status line, or - for none> <body fragment> <curl args...>
    expect() {
        local want=$1 interim=$2 fragment=$3
        shift 3
        local heads body code
        heads=$(mktemp)
        body=$(curl -sS --max-time 10 --expect100-timeout 30 -D "$heads" -w '\n%{http_code}' "$@")
        code=${body##*$'\n'}
        body=${body%$'\n'*}
        echo "driver: $* -> $code $body"
        sed 's/^/driver:   | /' "$heads"
        if [ "$code" != "$want" ]; then
            echo "::error::live request $* answered $code, want $want"
            failed=1
        fi
        if [ "$interim" = - ]; then
            if grep -q '^HTTP/1.1 1' "$heads"; then
                echo "::error::live request $* got an interim response the app did not send"
                failed=1
            fi
        elif ! grep -q "^${interim}" "$heads"; then
            echo "::error::live request $* did not get the app's '${interim}' before its answer"
            failed=1
        fi
        if [[ "$body" != *"$fragment"* ]]; then
            echo "::error::live request $* answered '$body', want the app's final body ('$fragment')"
            failed=1
        fi
        rm -f "$heads"
    }
    expect 200 "HTTP/1.1 103 Early Hints" '"hinted"' "http://127.0.0.1:${APP_PORT}/hints"
    expect 200 "HTTP/1.1 102 Processing" '"processed"' "http://127.0.0.1:${APP_PORT}/processing"
    expect 201 "HTTP/1.1 100 Continue" '"received": "abc"' \
        -H 'Expect: 100-continue' --data-binary 'abc' "http://127.0.0.1:${APP_PORT}/upload"
    # A PUT is idempotent: the normal loop buffered its body, for a re-send,
    # before it forwarded anything.
    expect 201 "HTTP/1.1 100 Continue" '"received": "xyz"' \
        -X PUT -H 'Expect: 100-continue' --data-binary 'xyz' "http://127.0.0.1:${APP_PORT}/upload"
    expect 401 "HTTP/1.1 100 Continue" '"unauthorized"' \
        -H 'Expect: 100-continue' --data-binary 'denied-body' "http://127.0.0.1:${APP_PORT}/deny"
    # curl's own wait for a 100 (one second; the last --expect100-timeout
    # counts), as a client that gets none sends the body after it.
    expect 401 - '"rejected"' \
        -H 'Expect: 100-continue' --expect100-timeout 1 --data-binary 'rejected-body' "http://127.0.0.1:${APP_PORT}/reject"
    sleep 5
    sudo pkill -INT keploy 2>/dev/null || true
    return $failed
}

run_mode() {
    local mode=$1 mode_flag=()
    [ "$mode" = sync ] && mode_flag=(--sync)
    echo "=================== record ${mode} ==================="
    rm -rf keploy/ keploy.yml
    # http.server stamps each response with the current Date.
    cat > keploy.yml <<'KEPLOY_CFG'
test:
    globalNoise:
        global: {"header": {"Date":[]}}
KEPLOY_CFG

    drive_requests > "driver_logs_${mode}.txt" 2>&1 &
    local driver_pid=$!
    sudo -E env PATH="$PATH" TMPDIR="$KEPLOY_TMPDIR" "$RECORD_BIN" record "${mode_flag[@]}" \
        -c "python3 app.py ${APP_PORT}" \
        --generateGithubActions=false > "record_logs_${mode}.txt" 2>&1
    echo "record exit status: $?"
    local driver_ok=1
    wait "$driver_pid" || driver_ok=0
    cat "driver_logs_${mode}.txt"
    if [ "$driver_ok" -ne 1 ]; then
        echo "::error::the client did not get the app's interim and final responses through keploy record (${mode})"
        tail -100 "record_logs_${mode}.txt"
        return 1
    fi
    no_temp_left "keploy record (${mode})" || return 1
    if grep -q 'WARNING: DATA RACE' "record_logs_${mode}.txt"; then
        echo "::error::data race detected during record (${mode})"
        tail -200 "record_logs_${mode}.txt"
        return 1
    fi
    if grep -q 'ERROR' "record_logs_${mode}.txt"; then
        echo "::error::keploy record logged an error (${mode})"
        grep -n 'ERROR' "record_logs_${mode}.txt" | head -20
        return 1
    fi
    if grep -q 'Not recording this request' "record_logs_${mode}.txt"; then
        echo "::error::keploy record left an exchange unrecorded (${mode})"
        grep -n 'Not recording this request' "record_logs_${mode}.txt"
        return 1
    fi

    local tests_dir=keploy/test-set-0/tests ntests
    ntests=$(find "$tests_dir" -type f -name '*.yaml' 2>/dev/null | wc -l)
    if [ "$ntests" -lt 7 ]; then
        echo "::error::recorded $ntests test cases, want at least 7 (health, hints, processing, two uploads, deny, reject) (${mode})"
        ls -la "$tests_dir" 2>/dev/null
        tail -100 "record_logs_${mode}.txt"
        return 1
    fi
    # The final responses, never an interim one.
    if grep -rnE '^\s*status_code: 1[0-9][0-9]\s*$' "$tests_dir"; then
        echo "::error::a test case recorded an interim response as its answer (${mode})"
        return 1
    fi
    local fragment
    # denied-body, rejected-body: the /deny and /reject test cases hold the
    # body the client sent, though the app had answered before it came.
    for fragment in '"hinted"' '"processed"' '"received": "abc"' '"received": "xyz"' '"unauthorized"' 'denied-body' '"rejected"' 'rejected-body'; do
        if ! grep -rqsF -- "$fragment" "$tests_dir"; then
            echo "::error::no test case recorded the final response with ${fragment} (${mode})"
            return 1
        fi
    done

    echo "=================== replay ${mode} ==================="
    sudo -E env PATH="$PATH" TMPDIR="$KEPLOY_TMPDIR" "$REPLAY_BIN" test \
        -c "python3 app.py ${APP_PORT}" --delay 5 \
        --generateGithubActions=false > "test_logs_${mode}.txt" 2>&1
    echo "test exit status: $?"
    if grep -q 'WARNING: DATA RACE' "test_logs_${mode}.txt"; then
        echo "::error::data race detected during replay (${mode})"
        tail -200 "test_logs_${mode}.txt"
        return 1
    fi
    if grep -q 'ERROR' "test_logs_${mode}.txt"; then
        echo "::error::keploy test logged an error (${mode})"
        grep -n 'ERROR' "test_logs_${mode}.txt" | head -20
        return 1
    fi
    local report status
    report=$(ls keploy/reports/test-run-0/test-set-0-report.yaml 2>/dev/null)
    if [ -z "$report" ]; then
        echo "::error::no replay report (${mode})"
        tail -100 "test_logs_${mode}.txt"
        return 1
    fi
    status=$(grep -m1 '^status:' "$report" | awk '{print $2}')
    echo "replay status (${mode}): $status"
    if [ "$status" != "PASSED" ]; then
        echo "::error::replay did not pass (${mode})"
        tail -60 "test_logs_${mode}.txt"
        return 1
    fi
    no_temp_left "keploy test (${mode})" || return 1
    return 0
}

run_mode default || exit 1
run_mode sync || exit 1
echo "PASS: interim responses are forwarded and the final ones recorded, and no run left its CA trust files behind"
exit 0
