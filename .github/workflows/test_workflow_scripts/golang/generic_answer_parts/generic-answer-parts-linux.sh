#!/usr/bin/env bash
# E2E: keploy records a Generic dependency's answer whole when the server
# sends it in more than one chunk, and keeps the test cases that wait on it.
#
# The store (store/) speaks a TCP line protocol keploy has no parser for, so
# its calls are recorded as Generic mocks. The app (app/) reads every value on
# one pooled connection. Under keploy record, with the proven switch off and
# on, the app fetches:
#   p1     a 4 KiB value, its header and its body written apart;
#   big1   a 96 KiB value in one write: more than one 32 KiB relay read;
#   s1     64 bytes in one write;
#   pbig2  a 96 KiB value, its header and its body written apart: the pooled
#          connection's last exchange, which no request after it ends.
# Nor does the connection's end: keploy record stops the app before its stop
# drains what the agent holds, and the app's FIN does not end the connection,
# as the store keeps its side open, and keploy's relay is told to keep a
# half-closed connection open far longer than the stop waits
# (--half-close-grace). So only the recording's stop can end pbig2's
# exchange: a stop that did not decide it would wait on it until it gave up,
# and the recording would fail.
# Then it asserts:
#   1. keploy record exits 0, keeps every test case (5: /health and the four
#      fetches) and says of none that a captured exchange was not recorded as
#      a mock;
#   2. each fetch has one Generic mock whose answer, its chunks joined, is the
#      whole answer (its length and its value's "|END <key>" end), and the
#      big answers are recorded in more than one chunk (a 32 KiB relay read
#      cannot take one at once, however the writes are scheduled: whether
#      p1's header and body reach the relay apart is the scheduler's call,
#      so its chunks are only counted);
#   3. keploy test, with the store down, passes every test case;
#   4. with KEPLOY_EXACT_VIEW_VERIFY=1 (the workflow sets it), each keploy
#      run under the proven switch prints its agent's verify line
#      (verify_ran): the variable reached keploy through sudo (SUDO_ENV).
#
# Env: RECORD_BIN, REPLAY_BIN. Ports: 8080 (app), 9097 (store), 16789
# (keploy's proxy).
#
# HALF_CLOSE_GRACE is far over the stop's bound on a drain that makes no
# progress (30 looks, each after 3 s of quiet): the relay must not end
# pbig2's connection before the stop has decided it, or has given up.
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
# shellcheck source=../../sudo_env.sh
source "$(dirname "${BASH_SOURCE[0]}")/../../sudo_env.sh" || exit 1

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-$RECORD_BIN}"
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP=8080
STORE=9097
PROXY=16789
KEYS="p1 big1 s1 pbig2"
HALF_CLOSE_GRACE=600s
FAIL=0
fail() {
    echo "::error::$*"
    FAIL=$((FAIL + 1))
}

WORK="$(mktemp -d)"
echo "work dir: $WORK"
(cd "$SRC" && go build -o "$WORK/ga-app" ./app && go build -o "$WORK/ga-store" ./store) || { echo "::error::build failed"; exit 1; }
cd "$WORK" || exit 1

listening() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
# pid_running PID: whether PID still runs, by its state in /proc, which shows
# root's processes to this user too (not kill -0: keploy runs under sudo).
pid_running() {
    local st
    case $1 in "" | *[!0-9]*) return 1 ;; esac
    { read -r st <"/proc/$1/stat"; } 2>/dev/null || return 1
    st=${st##*) }
    [ "${st%% *}" != Z ]
}
wait_listen() {
    until listening "$1"; do
        pid_running "$2" || return 1
        sleep 0.2
    done
}
test_files() { find keploy/test-set-0/tests -maxdepth 1 -name '*.yaml' -size +0c 2>/dev/null | wc -l; }
own() { sudo chown -R "$(id -u):$(id -g)" . 2>/dev/null || true; }
stop_pid() {
    local sudo_pid="$1" child
    child="$(pgrep -P "$sudo_pid" 2>/dev/null | head -n1)"
    [ -n "$child" ] && sudo kill -INT "$child" 2>/dev/null
    wait "$sudo_pid" 2>/dev/null
}
# value_len KEY: the length of the store's value for KEY (store/main.go).
value_len() {
    case $1 in *big*) echo 98304 ;; p*) echo 4096 ;; *) echo 64 ;; esac
}
# switch_env SW: the environment of keploy's commands for the switch.
switch_env() { [ "$1" = on ] && echo KEPLOY_PROVEN=1 || echo KEPLOY_PROVEN=false; }

STORE_PID=""
REC_PID=""
cleanup() {
    [ -n "$REC_PID" ] && stop_pid "$REC_PID"
    [ -n "$STORE_PID" ] && kill "$STORE_PID" 2>/dev/null
}
trap cleanup EXIT

for p in "$APP" "$STORE" "$PROXY"; do
    if listening "$p"; then echo "::error::port $p is in use"; exit 1; fi
done

# record SW: a recording, in rec-SW, of /health and the four fetches.
record() {
    local sw="$1" dir="rec-$1" k out deadline rc
    mkdir -p "$dir" && cd "$dir" || exit 1
    ../ga-store >store.log 2>&1 &
    STORE_PID=$!
    wait_listen "$STORE" "$STORE_PID" || { echo "::error::the store did not start"; exit 1; }
    sudo env PATH="$PATH" "${SUDO_ENV[@]}" "$(switch_env "$sw")" "$RECORD_BIN" record -c "../ga-app" \
        --half-close-grace "$HALF_CLOSE_GRACE" \
        --generateGithubActions=false --disable-tele >record.log 2>&1 </dev/null &
    REC_PID=$!
    wait_listen "$APP" "$REC_PID" || { echo "::error::$dir: the app did not start under keploy record"; tail -30 record.log; exit 1; }
    curl -s --max-time 30 "http://127.0.0.1:$APP/health" >/dev/null
    for k in $KEYS; do
        out="$(curl -s --max-time 30 "http://127.0.0.1:$APP/fetch?key=$k")"
        case $out in
        *"\"len\":$(value_len "$k"),"*) ;;
        *) fail "$dir: /fetch?key=$k under keploy record answered: ${out:0:300}" ;;
        esac
    done
    # Every test case but the last is written as the next one is made; give
    # the last a while, then stop: the stop hands on pbig2's exchange.
    deadline=$((SECONDS + 30))
    until [ "$(test_files)" -ge 5 ] || [ "$SECONDS" -ge "$deadline" ]; do
        pid_running "$REC_PID" || break
        sleep 0.5
    done
    stop_pid "$REC_PID"
    rc=$?
    REC_PID=""
    if [ "$rc" -ne 0 ]; then
        fail "$dir: keploy record exited $rc, want 0"
        grep -E 'stopped waiting|still holds|captured before the stop' record.log | head -5 | cut -c1-600
    fi
    kill "$STORE_PID" 2>/dev/null
    wait "$STORE_PID" 2>/dev/null
    STORE_PID=""
    own
    verify_ran "$(proven_of "$(switch_env "$sw")")" record.log || FAIL=$((FAIL + 1))
    cd "$WORK" || exit 1
}

# mock_of DIR KEY: the Generic mocks of FETCH KEY in DIR's recording; mocks
# counts them, chunks counts the answer's chunks, and answer prints them
# joined (a "string" chunk as is, any other base64-decoded).
mock_of() { K="FETCH $2"$'\n' yq "select(.kind == \"Generic\" and .spec.genericrequests[0].message[0].data == strenv(K))" "$1/keploy/test-set-0/mocks.yaml"; }
mocks() { mock_of "$1" "$2" | yq -N '.kind' - 2>/dev/null | grep -c Generic; }
chunks() { mock_of "$1" "$2" | yq '.spec.genericresponses | length' - 2>/dev/null; }
answer() {
    mock_of "$1" "$2" | yq -o=json -I=0 '[.spec.genericresponses[].message[]]' - 2>/dev/null |
        python3 -c 'import base64, json, sys
out = b""
for m in json.load(sys.stdin):
    d = m["data"]
    out += d.encode() if m["type"] == "string" else base64.b64decode(d)
sys.stdout.buffer.write(out)'
}

check_recording() {
    local sw="$1" dir="rec-$1" k n want got
    n="$(cd "$dir" && test_files)"
    [ "$n" -eq 5 ] || fail "$dir: keploy record kept $n test cases, want 5: a test case was left out"
    if grep -q 'a captured exchange was not recorded as a mock' "$dir/record.log"; then
        fail "$dir: keploy record left out a captured exchange"
        grep 'a captured exchange was not recorded as a mock' "$dir/record.log" | head -5 | cut -c1-600
    fi
    for k in $KEYS; do
        n="$(mocks "$dir" "$k")"
        [ "$n" = 1 ] || { fail "$dir: $n Generic mocks of FETCH $k, want 1"; continue; }
        got="$(answer "$dir" "$k" | wc -c)"
        want=$(($(printf 'VAL %d\n' "$(value_len "$k")" | wc -c) + $(value_len "$k")))
        [ "$got" -eq "$want" ] || fail "$dir: FETCH $k's mock answers $got bytes, want $want (the whole answer)"
        [ "$(answer "$dir" "$k" | tail -c "$((${#k} + 5))")" = "|END $k" ] ||
            fail "$dir: FETCH $k's mock answer does not end with the value's end"
        case $k in
        *big*)
            [ "$(chunks "$dir" "$k")" -gt 1 ] ||
                fail "$dir: FETCH $k's answer is recorded in $(chunks "$dir" "$k") chunk, but no relay read takes $want bytes at once"
            ;;
        esac
        echo "$dir: FETCH $k: 1 mock, $got bytes in $(chunks "$dir" "$k") chunks"
    done
}

# replay SW: keploy test of rec-SW, the store down: every test case passes.
replay() {
    local sw="$1" dir="rec-$1" rc report
    cd "$dir" || exit 1
    sudo env PATH="$PATH" "${SUDO_ENV[@]}" "$(switch_env "$sw")" "$REPLAY_BIN" test -c "../ga-app" \
        --delay 3 --generateGithubActions=false --disable-tele >replay.log 2>&1 </dev/null
    rc=$?
    own
    verify_ran "$(proven_of "$(switch_env "$sw")")" replay.log || FAIL=$((FAIL + 1))
    report="$(ls -1d keploy/reports/test-run-*/test-set-0-report.yaml 2>/dev/null | sort -V | tail -n1)"
    if [ -z "$report" ]; then
        fail "$dir: keploy test wrote no report (exit $rc)"
        tail -30 replay.log
    elif [ "$rc" -ne 0 ] || [ "$(yq -r '.status' "$report")" != PASSED ] ||
        [ "$(yq -r '[.tests[] | select(.status == "PASSED")] | length' "$report")" != 5 ]; then
        fail "$dir: keploy test must pass all 5 test cases (exit $rc, status $(yq -r '.status' "$report"))"
        yq -r '.tests[] | .test_case_id + " " + .status' "$report"
        grep -nE 'fetch |502|no matching|mismatch' replay.log | head -20 | cut -c1-400
    fi
    cd "$WORK" || exit 1
}

for sw in off on; do
    echo "== the proven switch $sw: record =="
    record "$sw"
    check_recording "$sw"
    echo "== the proven switch $sw: replay, the store down =="
    replay "$sw"
done

if [ "$FAIL" -ne 0 ]; then
    echo "::error::$FAIL check(s) failed"
    exit 1
fi
echo "every check passed"
