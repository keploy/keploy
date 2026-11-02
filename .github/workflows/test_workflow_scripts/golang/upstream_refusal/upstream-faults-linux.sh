#!/usr/bin/env bash
# E2E: an upstream's fault is recorded as itself and replayed as itself.
#
# faults/main.go's upstream answers /reset with a reset, /eof with a close,
# /hang with nothing until the client closes, /truncate with 10 of the 20
# body bytes its Content-Length names, /reason with "402 Pay Up Now", and /ok
# whole. The application calls it on a new connection each time, with a 2s
# client timeout, and answers what its own error handling saw, as JSON (which
# keploy test compares whole in every mode).
#
#   1. Without keploy: the fixture gives each fault (else this proves nothing).
#   2. keploy record: the application sees what it saw without keploy (not a
#      made-up 502/504), keploy writes the four faults as kind HttpFault and
#      says once that it records faults as themselves.
#   3. keploy test with the upstream gone: every test passes (the application
#      saw the same fault again, from the mock), and the summary names each
#      fault it served, as recorded.
#   4. With OLD_BIN (a released keploy that knows no faults): it skips each
#      HttpFault mock, naming the kind, and the four fault tests fail; none is
#      served the recorded head as a whole answer.
#   5. keploy record stopped while the application holds a call open (a long
#      poll the upstream never answers, its client's timeout far off): the
#      stop closes it, which is no fault, so none is recorded and no fault
#      note is said, while the /ok call made before it is recorded.
#   6-8. keploy mock record, the same held call, ended three ways: stopped by
#      SIGINT, stopped by --record-timer, and the command exiting on its own
#      (its /exit) with the call still open. Each is the run's end, not the
#      application giving up: no fault is recorded and no note is said, and
#      the /ok call is recorded.
#
# Env: RECORD_BIN, REPLAY_BIN (default RECORD_BIN), OLD_BIN (optional). Ports:
# 18090 (app), 18091 (upstream). Runs as a user with passwordless sudo.
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true

RECORD_BIN="${RECORD_BIN:?RECORD_BIN must name the keploy binary under test}"
REPLAY_BIN="${REPLAY_BIN:-$RECORD_BIN}"
OLD_BIN="${OLD_BIN:-}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP=127.0.0.1:18090
UP=127.0.0.1:18091
SHAPES="ok reset eof hang truncate reason"
FAULTS="reset eof hang truncate"
WORK="$(mktemp -d)"
FAIL=0
fail() {
    echo "::error::$*"
    FAIL=$((FAIL + 1))
}

listening() { (exec 3<>"/dev/tcp/${1%:*}/${1##*:}") 2>/dev/null; }
wait_listen() {
    local _i
    for _i in $(seq 1 600); do
        listening "$1" && return 0
        kill -0 "$2" 2>/dev/null || return 1
        sleep 0.1
    done
    return 1
}
# stop_pid PID: SIGINT to the keploy that sudo PID runs, then wait for both.
stop_pid() {
    local child
    child="$(pgrep -P "$1" 2>/dev/null | head -n1)"
    [ -n "$child" ] && sudo kill -INT "$child" 2>/dev/null
    wait "$1" 2>/dev/null
}
UP_PID=""
APP_PID=""
REC_PID=""
cleanup() {
    [ -n "$REC_PID" ] && stop_pid "$REC_PID"
    [ -n "$APP_PID" ] && kill "$APP_PID" 2>/dev/null
    [ -n "$UP_PID" ] && kill "$UP_PID" 2>/dev/null
    cp "$WORK"/*.log "$HERE/" 2>/dev/null
    sudo rm -rf "$WORK"
}
trap cleanup EXIT

(cd "$HERE/faults" && CGO_ENABLED=0 go build -o "$WORK/faults" .) || { echo "::error::building the app failed"; exit 1; }
cd "$WORK" || exit 1
for a in "$APP" "$UP"; do
    listening "$a" && { echo "::error::$a is already in use"; exit 1; }
done
# call_all OUT: each shape's answer, one line each.
call_all() {
    local s
    for s in $SHAPES; do
        curl -s --max-time 30 "http://$APP/call/$s" || echo "{\"shape\":\"$s\",\"curl\":\"failed\"}"
    done > "$1"
}

echo "== 1. without keploy =="
./faults -upstream "$UP" > upstream.log 2>&1 &
UP_PID=$!
wait_listen "$UP" "$UP_PID" || { echo "::error::the upstream did not start"; exit 1; }
./faults -app "$APP" "$UP" > app.log 2>&1 &
APP_PID=$!
wait_listen "$APP" "$APP_PID" || { echo "::error::the app did not start"; exit 1; }
call_all baseline.txt
kill "$APP_PID"; wait "$APP_PID" 2>/dev/null; APP_PID=""
cat baseline.txt
for want in '{"shape":"ok","status":"200 OK","body":"ok","bytes":2,"length":2,"read":"whole"}' \
    '{"shape":"reset","saw":"reset"}' '{"shape":"eof","saw":"eof"}' '{"shape":"hang","saw":"timeout"}' \
    '{"shape":"truncate","status":"200 OK","body":"0123456789","bytes":10,"length":20,"read":"eof"}' \
    '{"shape":"reason","status":"402 Pay Up Now","body":"pay!","bytes":4,"length":4,"read":"whole"}'; do
    grep -qxF "$want" baseline.txt || fail "1: the fixture did not give '$want' without keploy"
done
[ "$FAIL" -eq 0 ] || exit 1

echo "== 2. keploy record =="
# The logs are the runner user's: sudo runs keploy, not the redirect.
# shellcheck disable=SC2024
sudo -E env PATH="$PATH" "$RECORD_BIN" record -c "./faults -app $APP $UP" \
    --generateGithubActions=false --disable-tele > record.log 2>&1 < /dev/null &
REC_PID=$!
wait_listen "$APP" "$REC_PID" || { echo "::error::the app did not start under keploy record"; tail -30 record.log; exit 1; }
call_all recorded.txt
n=0
for _ in $(seq 1 300); do
    n=$(find keploy/test-set-0/tests -maxdepth 1 -name '*.yaml' -size +0c 2>/dev/null | wc -l)
    [ "$n" -ge 6 ] && break
    sleep 0.1
done
stop_pid "$REC_PID"; REC_PID=""
sudo chown -R "$(id -u):$(id -g)" . 2>/dev/null
[ "$n" -eq 6 ] || fail "2: keploy record wrote $n test cases, want 6"
if ! diff -u baseline.txt recorded.txt; then
    fail "2: under keploy record the application saw other answers than without keploy (diff above)"
fi
got=$(grep -c '^kind: HttpFault$' keploy/test-set-0/mocks.yaml 2>/dev/null)
[ "$got" = 4 ] || fail "2: mocks.yaml holds ${got:-0} mocks of kind HttpFault, want 4 (reset, eof, hang, truncate)"
grep -q 'Pay Up Now' keploy/test-set-0/mocks.yaml || fail "2: mocks.yaml does not hold the reason phrase 'Pay Up Now'"
got=$(grep -c 'keploy now records a call that ended in a fault with the fault itself' record.log)
[ "$got" = 1 ] || fail "2: keploy record said its fault note $got times, want once"
if grep -q 'Keploy-Recorded-Upstream-Error' keploy/test-set-0/mocks.yaml; then
    fail "2: keploy record wrote a made-up upstream-error answer"
fi

kill "$UP_PID" 2>/dev/null; wait "$UP_PID" 2>/dev/null; UP_PID=""

# show LOG: what keploy said of its mocks and tests, for a failed step.
show() {
    grep -nE "cannot read mock kind|served a mock's fault|Testrun (passed|failed)|mock miss|no matching mock|ERROR" "$1" | head -40
    tail -n 20 "$1"
}

# replay BIN LOG: keploy test of the recording, the upstream gone; prints
# "SHAPE STATUS" per test from the run's report.
replay() {
    local runs report
    runs=$(find keploy/reports -maxdepth 1 -name 'test-run-*' 2>/dev/null | wc -l)
    # shellcheck disable=SC2024
    sudo -E env PATH="$PATH" "$1" test -c "./faults -app $APP $UP" --delay 3 \
        --generateGithubActions=false --disable-tele > "$2" 2>&1 < /dev/null
    sudo chown -R "$(id -u):$(id -g)" . 2>/dev/null
    # The run's own report: a run that wrote none must not be read as the
    # one before it.
    [ "$(find keploy/reports -maxdepth 1 -name 'test-run-*' 2>/dev/null | wc -l)" -gt "$runs" ] || { echo "NOREPORT"; return; }
    report="$(ls -1d keploy/reports/test-run-* | sort -V | tail -n1)/test-set-0-report.yaml"
    [ -f "$report" ] || { echo "NOREPORT"; return; }
    python3 - "$report" <<'PY'
import re, sys
# Each entry of the report's top-level tests: list, at whatever indent keploy
# writes it: its own status: key and the /call/SHAPE its request named.
lines = open(sys.argv[1]).read().split("\n")
start = next(i for i, l in enumerate(lines) if l == "tests:") + 1
entries, ind = [], None
for l in lines[start:]:
    if l and not l.startswith((" ", "-")):
        break
    m = re.match(r"^(\s*)- ", l)
    if m and (ind is None or len(m.group(1)) == ind):
        ind = len(m.group(1))
        entries.append([])
        l = " " * (ind + 2) + l[ind + 2:]
    if entries:
        entries[-1].append(l)
for e in entries:
    key = " " * (ind + 2)
    status = next((l.split(":", 1)[1].strip() for l in e if l.startswith(key + "status:")), "?")
    url = next((m.group(1) for l in e for m in [re.search(r"url: \S*/call/(\w+)", l)] if m), "?")
    print(url, status)
PY
}

echo "== 3. keploy test, the upstream gone =="
before=$FAIL
replay "$REPLAY_BIN" test.log > statuses.txt
cat statuses.txt
for s in $SHAPES; do
    grep -qx "$s PASSED" statuses.txt || fail "3: test of /call/$s did not pass under keploy test (the application saw another answer than at record)"
done
for want in 'upstream reset after 0 bytes, as recorded' 'upstream closed with no answer, as recorded' \
    'no answer until the application closed, as recorded' 'upstream sent the head and 10 body bytes, then closed, as recorded'; do
    grep -q "served a mock's fault.*$want" test.log || fail "3: keploy test's summary does not name '$want'"
done
[ "$FAIL" -eq "$before" ] || show test.log

if [ -n "$OLD_BIN" ]; then
    echo "== 4. a keploy that knows no faults ($("$OLD_BIN" --version 2>/dev/null | tail -n1)) =="
    before=$FAIL
    replay "$OLD_BIN" old.log > old_statuses.txt
    cat old_statuses.txt
    got=$(grep -c 'cannot read mock kind' old.log)
    [ "$got" -ge 4 ] || fail "4: the old keploy said it cannot read a mock's kind $got times, want one per HttpFault mock (4)"
    for s in $FAULTS; do
        grep -qx "$s FAILED" old_statuses.txt || fail "4: the old keploy's test of /call/$s did not fail: a fault mock was served as a whole answer"
    done
    grep -qx "ok PASSED" old_statuses.txt || fail "4: the old keploy's test of /call/ok did not pass, so its misses prove nothing"
    [ "$FAIL" -eq "$before" ] || show old.log
fi

echo "== 5. keploy record stopped with a call in flight =="
before=$FAIL
mkdir -p "$WORK/stop" && cd "$WORK/stop" || exit 1
"$WORK/faults" -upstream "$UP" > held_upstream.log 2>&1 &
UP_PID=$!
wait_listen "$UP" "$UP_PID" || { echo "::error::the upstream did not start"; exit 1; }
# shellcheck disable=SC2024
sudo -E env PATH="$PATH" HELD_CALL=1 "$RECORD_BIN" record -c "$WORK/faults -app $APP $UP" \
    --generateGithubActions=false --disable-tele > held_record.log 2>&1 < /dev/null &
REC_PID=$!
wait_listen "$APP" "$REC_PID" || { echo "::error::the app did not start under keploy record"; tail -30 held_record.log; exit 1; }
curl -s --max-time 30 "http://$APP/call/ok" > /dev/null
held=0
for _ in $(seq 1 300); do
    grep -q 'upstream: holding /held' held_upstream.log && held=1 && break
    sleep 0.1
done
[ "$held" = 1 ] || fail "5: the application's held call did not reach the upstream"
# The stop, while the call is held: the upstream answers it never.
stop_pid "$REC_PID"; REC_PID=""
kill "$UP_PID" 2>/dev/null; wait "$UP_PID" 2>/dev/null; UP_PID=""
sudo chown -R "$(id -u):$(id -g)" . 2>/dev/null
if grep -q 'held call: timeout' held_record.log; then
    fail "5: the application's own client gave up on the held call, so this proves nothing"
fi
grep -q '/ok' keploy/test-set-0/mocks.yaml 2>/dev/null || fail "5: keploy record did not record the /ok call, so a recording with no fault proves nothing"
got=$(grep -c '^kind: HttpFault$' keploy/test-set-0/mocks.yaml 2>/dev/null)
[ "${got:-0}" = 0 ] || fail "5: keploy record wrote $got fault mocks for a call its own stop cut, want none"
if grep -q 'keploy now records a call that ended in a fault' held_record.log; then
    fail "5: keploy record said its fault note for a call its own stop cut"
fi
[ "$FAIL" -eq "$before" ] || { grep -n 'fault' keploy/test-set-0/mocks.yaml | head -20; tail -40 held_record.log; }
cp held_upstream.log held_record.log "$WORK/" 2>/dev/null
cd "$WORK" || exit 1

# mock_record_held LEG HOW: keploy mock record of the application with a call
# held open, ended HOW (sigint, timer or exit), and what it wrote checked.
mock_record_held() {
    local leg="$1" how="$2" extra=() held=0 mocks
    echo "== $leg. keploy mock record ended by $how with a call in flight =="
    local before=$FAIL
    mkdir -p "$WORK/mock-$how" && cd "$WORK/mock-$how" || exit 1
    "$WORK/faults" -upstream "$UP" > upstream.log 2>&1 &
    UP_PID=$!
    wait_listen "$UP" "$UP_PID" || { echo "::error::the upstream did not start"; exit 1; }
    [ "$how" = timer ] && extra=(--record-timer 30s)
    # shellcheck disable=SC2024
    sudo -E env PATH="$PATH" HELD_CALL=1 "$RECORD_BIN" mock record -c "$WORK/faults -app $APP $UP" --name held \
        --disable-tele "${extra[@]}" > record.log 2>&1 < /dev/null &
    REC_PID=$!
    wait_listen "$APP" "$REC_PID" || { echo "::error::the app did not start under keploy mock record"; tail -30 record.log; exit 1; }
    curl -s --max-time 30 "http://$APP/call/ok" > /dev/null
    for _ in $(seq 1 300); do
        grep -q 'upstream: holding /held' upstream.log && held=1 && break
        sleep 0.1
    done
    [ "$held" = 1 ] || fail "$leg: the application's held call did not reach the upstream"
    case "$how" in
    sigint) stop_pid "$REC_PID" ;;
    timer) wait "$REC_PID" 2>/dev/null ;;
    exit)
        curl -s --max-time 30 "http://$APP/exit" > /dev/null
        wait "$REC_PID" 2>/dev/null
        ;;
    esac
    REC_PID=""
    kill "$UP_PID" 2>/dev/null; wait "$UP_PID" 2>/dev/null; UP_PID=""
    sudo chown -R "$(id -u):$(id -g)" . 2>/dev/null
    if grep -q 'held call: timeout' record.log; then
        fail "$leg: the application's own client gave up on the held call, so this proves nothing"
    fi
    if [ "$how" = exit ] && ! grep -q 'app: exiting on its own' record.log; then
        fail "$leg: the application did not exit on its own"
    fi
    mocks=keploy/held/mocks.yaml
    grep -q '/ok' "$mocks" 2>/dev/null || fail "$leg: keploy mock record did not record the /ok call, so a recording with no fault proves nothing"
    got=$(grep -c '^kind: HttpFault$' "$mocks" 2>/dev/null)
    [ "${got:-0}" = 0 ] || fail "$leg: keploy mock record wrote $got fault mocks for a call the run's end cut, want none"
    if grep -q 'keploy now records a call that ended in a fault' record.log; then
        fail "$leg: keploy mock record said its fault note for a call the run's end cut"
    fi
    [ "$FAIL" -eq "$before" ] || { grep -n 'fault' "$mocks" | head -20; tail -40 record.log; }
    cp upstream.log "$WORK/mock-$how-upstream.log" 2>/dev/null
    cp record.log "$WORK/mock-$how-record.log" 2>/dev/null
    cd "$WORK" || exit 1
}
mock_record_held 6 sigint
mock_record_held 7 timer
mock_record_held 8 exit

if [ "$FAIL" -ne 0 ]; then
    echo "::error::$FAIL check(s) failed"
    exit 1
fi
echo "all checks passed"
