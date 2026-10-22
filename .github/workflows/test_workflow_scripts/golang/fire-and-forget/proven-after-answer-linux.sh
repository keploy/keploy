#!/usr/bin/env bash
# E2E: a call the app makes after it has answered a test (a fire-and-forget
# publish its handler starts once the answer has left) is judged, under the
# proven switch (P101): as the call of the test open when it is made, so a
# changed call fails its set and keploy test exits non-zero.
#
# The app's /d-after answers "accepted <id>" and only then publishes
# "after <id>" to a broker, from a goroutine of its own, never waiting for the
# reply: /e-wait for the same id releases the publish and answers once it is
# over. Each recording is /c-ping, /d-after?id=1, /e-wait?id=1 (keploy
# replays a set's tests in name order): the publish is made after d-after-1's
# answer, inside e-wait-1, in the recording and in every replay, whatever the
# scheduler does.
#
#   1. TCP, the recorded message: every test passes.
#   2. TCP, a changed message: the set fails, keploy test exits non-zero;
#      e-wait-1, which the call was made in, fails, and d-after-1 passes.
#   3. HTTP, the recorded message: every test passes.
#   4. HTTP, a changed message: as 2.
#   5. TCP, the app no longer publishes: the set fails, keploy test exits
#      non-zero; e-wait-1, which recorded the call, fails for it, and
#      d-after-1 passes. The publish is a connection of its own, with one
#      exchange, opened and closed inside e-wait-1: the test's whole, its
#      first exchange included.
#
# A call made after the set's LAST test is judged by a read of the set's
# misses at its end, which keploy makes once it has stopped the app it started
# for the set and the app has exited (P101-b): a call the app makes only as it
# is stopped is read with them. With SHUTDOWN_PUBLISH set the app publishes
# "shutdown" as it is stopped, after its last answer; its recording is /c-ping:
#
#   6. HTTP, the recorded shutdown publish: every test passes.
#   7. HTTP, a changed shutdown publish: the set fails, keploy test exits
#      non-zero, the report names the call among those made after the set's
#      last test, and c-ping, whose answer it followed, passes.
#
#   8. An app that ignores SIGINT: keploy's stop at the set's end waits for
#      its exit, and kills it once it has made no progress on its way out for
#      the stall bound of keploy's runnable time (a minute), and the set is
#      not judged (what the app would have done as it stopped is not known):
#      keploy test exits non-zero, and the report names the kill and the
#      stall.
#
#   9. The app of 6 and 7, started in the background by its shell
#      (`app & wait`), its shutdown taking 5 s before it publishes, and a
#      second signal ending it once it is stopping (ONE_SIGNAL, as Go's
#      signal.NotifyContext documents): the shell dies on keploy's interrupt
#      at once, and keploy waits for the app, a member of the shell's process
#      group that acts on the interrupt, without signalling it again. The
#      recorded shutdown publish passes, as in 6; a changed one fails the
#      set, as in 7.
#  10. That app, started so, never ending once it is stopping (HANG_ON_STOP):
#      keploy kills it once it has made no progress on its way out, and the
#      set is not judged: keploy test exits non-zero, and the report names
#      the kill.
#
#  11. A recording whose stop had to kill the app (its shutdown never ends
#      once it is stopping, HANG_ON_STOP, so keploy record kills it on the
#      stall rule, and its shutdown publish is not recorded): keploy record
#      says so in the test-set's config (recordStopKilled).
#      A replay whose app publishes on its way out at once makes a call the
#      recording lacks: the set is not judged, keploy test exits non-zero,
#      and the report names the recording's kill. It is not failed for a
#      call that record's grace cut (P101-b-k).
#
# A call made before the set's FIRST test, as the app starts, is no test's
# either (F2): the first test's read hands it over as the set's, and the set's
# end fails the set with it. With STARTUP_PUBLISH set the app publishes
# "startup" before it listens; its recording is /c-ping:
#
#  12. HTTP, the recorded startup publish: every test passes.
#  13. HTTP, a changed startup publish: the set fails, keploy test exits
#      non-zero, the report names the call among those made before the set's
#      first test, and c-ping passes. It was dropped with a warning, and the
#      set passed.
#
# A delayed publish the app starts on its own after its last answer, and
# that lands before it is stopped, is the same read's; whether it lands
# before the stop is the app's clock, and no case here asserts on it.
#
# Env: RECORD_BIN, REPLAY_BIN. Ports: 8080 (app), 9099 (broker, HTTP), 9098
# (broker, TCP), 16789 (keploy's proxy).
set -uo pipefail
# The proven switch, stated for every keploy this lane runs: the steps that
# say KEPLOY_PROVEN=1 run under it, and every other one with it off
# (KEPLOY_PROVEN=0, the open-source build's default, and keploy enterprise's
# on Windows and macOS), as this lane always ran them. Unset, keploy
# enterprise runs the proven model on Linux.
export KEPLOY_PROVEN=0

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-$RECORD_BIN}"
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP=8080
PROXY=16789
FAIL=0
fail() {
    echo "::error::$*"
    FAIL=$((FAIL + 1))
}

WORK="$(mktemp -d)"
echo "work dir: $WORK"
(cd "$SRC" && go build -o "$WORK/ff-app" ./app && go build -o "$WORK/ff-broker" ./broker) || { echo "::error::build failed"; exit 1; }
cd "$WORK" || exit 1

listening() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
wait_listen() {
    until listening "$1"; do
        kill -0 "$2" 2>/dev/null || return 1
        sleep 0.2
    done
}
test_files() { find keploy/test-set-0/tests -maxdepth 1 -name '*.yaml' -size +0c 2>/dev/null | wc -l; }
wait_tests() {
    until [ "$(test_files)" -ge "$1" ]; do
        kill -0 "$2" 2>/dev/null || return 1
        sleep 0.2
    done
}
own() { sudo chown -R "$(id -u):$(id -g)" . 2>/dev/null || true; }
stop_pid() {
    local sudo_pid="$1" child
    child="$(pgrep -P "$sudo_pid" 2>/dev/null | head -n1)"
    [ -n "$child" ] && sudo kill -INT "$child" 2>/dev/null
    wait "$sudo_pid" 2>/dev/null
}
# The replay the checks read: replay sets it to the recording it replayed.
REPLAY_DIR=""
newest_run() { ls -1d "$REPLAY_DIR"/keploy/reports/test-run-* 2>/dev/null | sort -V | tail -n1; }
REPORT() { echo "$(newest_run)/test-set-0-report.yaml"; }
set_status() { grep -m1 '^status:' "$(REPORT)" 2>/dev/null | awk '{print $2}'; }
# test_status <id>: the status the report gives test case id.
test_status() { ID="$1" yq -r '.tests[] | select(.test_case_id == strenv(ID)) | .status' "$(REPORT)" 2>/dev/null; }

# APP_ENV is more of the app's environment, for every run of it.
APP_ENV=""
BROKER_PID=""
REC_PID=""
cleanup() {
    [ -n "$REC_PID" ] && stop_pid "$REC_PID"
    [ -n "$BROKER_PID" ] && kill "$BROKER_PID" 2>/dev/null
}
trap cleanup EXIT

for p in "$APP" 9098 9099 "$PROXY"; do
    if listening "$p"; then echo "::error::port $p is in use"; exit 1; fi
done

broker_of() { case "$1" in tcp | tcpkeep) echo 127.0.0.1:9098 ;; *) echo 127.0.0.1:9099 ;; esac; }

# record <dir> <proto>
record() {
    local dir="$1" proto="$2" broker
    broker="$(broker_of "$proto")"
    mkdir -p "$dir" && cd "$dir" || exit 1
    ../ff-broker >broker.log 2>&1 &
    BROKER_PID=$!
    wait_listen 9099 "$BROKER_PID" || { echo "::error::the broker did not start"; exit 1; }
    sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$RECORD_BIN" record -c "env PROTO=$proto BROKER=$broker $APP_ENV ../ff-app" \
        --generateGithubActions=false --disable-tele >record.log 2>&1 </dev/null &
    REC_PID=$!
    wait_listen "$APP" "$REC_PID" || { echo "::error::$dir: the app did not start under keploy record"; tail -30 record.log; exit 1; }
    curl -s "http://127.0.0.1:$APP/c-ping" >/dev/null
    curl -s "http://127.0.0.1:$APP/d-after?id=1" >/dev/null
    curl -s "http://127.0.0.1:$APP/e-wait?id=1" >/dev/null
    # The publish is recorded once the broker answered it; the app logs it.
    until grep -q "after 1: broker said" record.log; do kill -0 "$REC_PID" 2>/dev/null || break; sleep 0.2; done
    wait_tests 3 "$REC_PID" || fail "$dir: keploy record wrote $(test_files) test cases, want 3"
    stop_pid "$REC_PID"
    REC_PID=""
    kill "$BROKER_PID" 2>/dev/null
    wait "$BROKER_PID" 2>/dev/null
    BROKER_PID=""
    own
    grep -q "after 1" keploy/test-set-0/mocks.yaml 2>/dev/null || fail "$dir: the publish was not recorded"
    cd "$WORK" || exit 1
}

# record_shutdown <dir>: a recording of /c-ping by the app that publishes
# "shutdown" as it is stopped (SHUTDOWN_PUBLISH), over HTTP.
record_shutdown() {
    local dir="$1"
    mkdir -p "$dir" && cd "$dir" || exit 1
    ../ff-broker >broker.log 2>&1 &
    BROKER_PID=$!
    wait_listen 9099 "$BROKER_PID" || { echo "::error::the broker did not start"; exit 1; }
    sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$RECORD_BIN" record -c "env PROTO=http BROKER=127.0.0.1:9099 $APP_ENV ../ff-app" \
        --generateGithubActions=false --disable-tele >record.log 2>&1 </dev/null &
    REC_PID=$!
    wait_listen "$APP" "$REC_PID" || { echo "::error::$dir: the app did not start under keploy record"; tail -30 record.log; exit 1; }
    curl -s "http://127.0.0.1:$APP/c-ping" >/dev/null
    wait_tests 1 "$REC_PID" || fail "$dir: keploy record wrote $(test_files) test cases, want 1"
    stop_pid "$REC_PID"
    REC_PID=""
    kill "$BROKER_PID" 2>/dev/null
    wait "$BROKER_PID" 2>/dev/null
    BROKER_PID=""
    own
    grep -q "shutdown published" record.log || fail "$dir: the app did not publish as keploy record stopped it"
    grep -q "shutdown" keploy/test-set-0/mocks.yaml 2>/dev/null || fail "$dir: the shutdown publish was not recorded"
    cd "$WORK" || exit 1
}

# APP_WRAP, when set, is how the app is started: "bg" runs it in the
# background of its shell, which waits for it (`app & wait`).
APP_WRAP=""
app_cmd() {
    if [ "$APP_WRAP" = bg ]; then
        echo "$1 & wait"
    else
        echo "$1"
    fi
}

# record_startup <dir>: a recording of /c-ping by the app that publishes
# "startup" as it starts, before it listens (STARTUP_PUBLISH), over HTTP.
record_startup() {
    local dir="$1"
    mkdir -p "$dir" && cd "$dir" || exit 1
    ../ff-broker >broker.log 2>&1 &
    BROKER_PID=$!
    wait_listen 9099 "$BROKER_PID" || { echo "::error::the broker did not start"; exit 1; }
    sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$RECORD_BIN" record -c "env PROTO=http BROKER=127.0.0.1:9099 $APP_ENV ../ff-app" \
        --generateGithubActions=false --disable-tele >record.log 2>&1 </dev/null &
    REC_PID=$!
    wait_listen "$APP" "$REC_PID" || { echo "::error::$dir: the app did not start under keploy record"; tail -30 record.log; exit 1; }
    curl -s "http://127.0.0.1:$APP/c-ping" >/dev/null
    wait_tests 1 "$REC_PID" || fail "$dir: keploy record wrote $(test_files) test cases, want 1"
    stop_pid "$REC_PID"
    REC_PID=""
    kill "$BROKER_PID" 2>/dev/null
    wait "$BROKER_PID" 2>/dev/null
    BROKER_PID=""
    own
    grep -q "startup published" record.log || fail "$dir: the app did not publish as it started under keploy record"
    grep -q "startup" keploy/test-set-0/mocks.yaml 2>/dev/null || fail "$dir: the startup publish was not recorded"
    cd "$WORK" || exit 1
}

# replay <dir> <name> <proto> [suffix] [nopublish]: keploy test of dir's
# recording, the broker down; RC is its exit code.
replay() {
    local dir="$1" name="$2" proto="$3" suffix="${4:-}" nopublish="${5:-}" broker before
    broker="$(broker_of "$proto")"
    REPLAY_DIR="$WORK/$dir"
    cd "$REPLAY_DIR" || exit 1
    before="$(newest_run)"
    sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$REPLAY_BIN" test -c "$(app_cmd "env PROTO=$proto BROKER=$broker SUFFIX=$suffix NOPUBLISH=$nopublish $APP_ENV ../ff-app")" \
        --delay 2 --generateGithubActions=false --disable-tele >"$name.log" 2>&1 </dev/null
    RC=$?
    own
    if [ "$(newest_run)" = "$before" ] || [ ! -f "$(REPORT)" ]; then
        fail "$dir, $name: keploy test wrote no report (exit $RC)"
        tail -30 "$name.log"
    fi
    cd "$WORK" || exit 1
}
expect_pass() {
    if [ "$RC" -ne 0 ] || [ "$(set_status)" != PASSED ]; then
        fail "$1: the set must pass (exit $RC, status $(set_status))"
        grep -nE 'test_case_id|status:|UNMATCHED|DEPENDENCY|unconsumed' "$(REPORT)" | head -30
    fi
}
# expect_after_fails <step>: the set fails, keploy test exits non-zero, the
# test the call was made in fails, and the test whose answer it followed
# passes.
expect_after_fails() {
    if [ "$RC" -eq 0 ] || [ "$(set_status)" != FAILED ]; then
        fail "$1: the set must fail, keploy test non-zero (exit $RC, status $(set_status))"
    fi
    [ "$(test_status get-e-wait-1)" = FAILED ] ||
        fail "$1: the test the changed call was made in did not fail ($(test_status get-e-wait-1))"
    [ "$(test_status get-d-after-1)" = PASSED ] ||
        fail "$1: the test whose answer the changed call followed did not pass ($(test_status get-d-after-1))"
}

echo "== recordings =="
record tcp tcp
record http http

echo "== 1. TCP, the recorded message =="
replay tcp as-recorded tcp
expect_pass "1 (TCP, as recorded)"
echo "== 2. TCP, a changed message =="
replay tcp changed tcp "-changed"
expect_after_fails "2 (TCP, changed)"
echo "== 3. HTTP, the recorded message =="
replay http as-recorded http
expect_pass "3 (HTTP, as recorded)"
echo "== 4. HTTP, a changed message =="
replay http changed http "-changed"
expect_after_fails "4 (HTTP, changed)"
echo "== 5. TCP, the app no longer publishes =="
replay tcp unmade tcp "" 1
expect_after_fails "5 (TCP, no publish)"

APP_ENV="SHUTDOWN_PUBLISH=1"
record_shutdown shutdown
echo "== 6. HTTP, the recorded shutdown publish =="
replay shutdown as-recorded http
expect_pass "6 (HTTP, shutdown publish as recorded)"
echo "== 7. HTTP, a changed shutdown publish =="
replay shutdown changed http "-changed"
if [ "$RC" -eq 0 ] || [ "$(set_status)" != FAILED ]; then
    fail "7 (HTTP, changed shutdown publish): the set must fail, keploy test non-zero (exit $RC, status $(set_status))"
fi
yq -r '.unclaimed_calls[]? | .actual_summary // ""' "$(REPORT)" 2>/dev/null | grep -q '/publish' ||
    fail "7 (HTTP, changed shutdown publish): the report does not name the publish among the calls made after the set's last test: $(grep -n -A3 'unclaimed' "$(REPORT)" | head -12)"
[ "$(test_status get-c-ping-1)" = PASSED ] ||
    fail "7 (HTTP, changed shutdown publish): c-ping, whose answer the call followed, did not pass ($(test_status get-c-ping-1))"

echo "== 9. the app of 7 in the background of its shell =="
APP_ENV="SHUTDOWN_PUBLISH=1 SHUTDOWN_DELAY=5s ONE_SIGNAL=1"
APP_WRAP=bg
replay shutdown bg-as-recorded http
expect_pass "9 (in the background, the recorded shutdown publish)"
replay shutdown bg-changed http "-changed"
if [ "$RC" -eq 0 ] || [ "$(set_status)" != FAILED ]; then
    fail "9 (in the background, a changed shutdown publish): the set must fail, keploy test non-zero (exit $RC, status $(set_status))"
fi
yq -r '.unclaimed_calls[]? | .actual_summary // ""' "$(REPORT)" 2>/dev/null | grep -q '/publish' ||
    fail "9 (in the background, a changed shutdown publish): the report does not name the publish"
echo "== 10. that app never ending once it is stopping =="
APP_ENV="SHUTDOWN_PUBLISH=1 HANG_ON_STOP=1"
replay shutdown bg-hangs http
if [ "$RC" -eq 0 ] || [ "$(set_status)" != INTERNAL_ERR ]; then
    fail "10 (in the background, never ending): the set must not be judged, keploy test non-zero (exit $RC, status $(set_status))"
fi
grep -m1 '^failure_reason:' "$(REPORT)" | grep -q 'made no progress on its way out' ||
    fail "10 (in the background, never ending): the report does not name the kill: $(grep -m1 '^failure_reason:' "$(REPORT)")"
APP_WRAP=""

APP_ENV="IGNORE_INT=1"
record_ping() {
    local dir="$1"
    mkdir -p "$dir" && cd "$dir" || exit 1
    sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$RECORD_BIN" record -c "env PROTO=http BROKER=127.0.0.1:9099 $APP_ENV ../ff-app" \
        --generateGithubActions=false --disable-tele >record.log 2>&1 </dev/null &
    REC_PID=$!
    wait_listen "$APP" "$REC_PID" || { echo "::error::$dir: the app did not start under keploy record"; tail -30 record.log; exit 1; }
    curl -s "http://127.0.0.1:$APP/c-ping" >/dev/null
    wait_tests 1 "$REC_PID" || fail "$dir: keploy record wrote $(test_files) test cases, want 1"
    stop_pid "$REC_PID"
    REC_PID=""
    own
    cd "$WORK" || exit 1
}
record_ping ignores
echo "== 8. an app that ignores SIGINT =="
replay ignores as-recorded http
if [ "$RC" -eq 0 ] || [ "$(set_status)" != INTERNAL_ERR ]; then
    fail "8 (an app that ignores SIGINT): the set must not be judged, keploy test non-zero (exit $RC, status $(set_status))"
fi
# (The report's YAML doubles an apostrophe: keploy''s.)
grep -m1 '^failure_reason:' "$(REPORT)" | grep -q "made no progress on its way out for .* runnable time after keploy interrupted it, so keploy killed it" ||
    fail "8 (an app that ignores SIGINT): the report does not name the kill and the stall: $(grep -m1 '^failure_reason:' "$(REPORT)")"

APP_ENV="SHUTDOWN_PUBLISH=1 HANG_ON_STOP=1"
# record_cut <dir>: a recording of /c-ping by that app, whose stop keploy
# record has to cut.
record_cut() {
    local dir="$1"
    mkdir -p "$dir" && cd "$dir" || exit 1
    ../ff-broker >broker.log 2>&1 &
    BROKER_PID=$!
    wait_listen 9099 "$BROKER_PID" || { echo "::error::the broker did not start"; exit 1; }
    sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$RECORD_BIN" record -c "env PROTO=http BROKER=127.0.0.1:9099 $APP_ENV ../ff-app" \
        --generateGithubActions=false --disable-tele >record.log 2>&1 </dev/null &
    REC_PID=$!
    wait_listen "$APP" "$REC_PID" || { echo "::error::$dir: the app did not start under keploy record"; tail -30 record.log; exit 1; }
    curl -s "http://127.0.0.1:$APP/c-ping" >/dev/null
    wait_tests 1 "$REC_PID" || fail "$dir: keploy record wrote $(test_files) test cases, want 1"
    stop_pid "$REC_PID"
    REC_PID=""
    kill "$BROKER_PID" 2>/dev/null
    wait "$BROKER_PID" 2>/dev/null
    BROKER_PID=""
    own
    grep -q "shutdown published" record.log && fail "$dir: the app published as keploy record stopped it; its stop was to be cut"
    grep -q "recordStopKilled" keploy/test-set-0/config.yaml 2>/dev/null ||
        fail "$dir: the test-set's config does not say its recording's stop killed the app: $(cat keploy/test-set-0/config.yaml 2>/dev/null)"
    cd "$WORK" || exit 1
}
record_cut cut
echo "== 11. a recording whose stop killed the app, replayed by an app that finishes its shutdown =="
APP_ENV="SHUTDOWN_PUBLISH=1"
replay cut finishes http
if [ "$RC" -eq 0 ] || [ "$(set_status)" != INTERNAL_ERR ]; then
    fail "11 (a recording cut by its stop): the set must not be judged, keploy test non-zero (exit $RC, status $(set_status))"
fi
# The report's YAML doubles an apostrophe, so the check reads past one.
grep -m1 '^failure_reason:' "$(REPORT)" | grep -q "stop had to kill the application (keploy stopped the application at the recording" ||
    fail "11 (a recording cut by its stop): the report does not name the recording's kill: $(grep -m1 '^failure_reason:' "$(REPORT)")"

APP_ENV="STARTUP_PUBLISH=1"
record_startup startup
echo "== 12. HTTP, the recorded startup publish =="
replay startup as-recorded http
expect_pass "12 (HTTP, startup publish as recorded)"
echo "== 13. HTTP, a changed startup publish =="
replay startup changed http "-changed"
if [ "$RC" -eq 0 ] || [ "$(set_status)" != FAILED ]; then
    fail "13 (HTTP, changed startup publish): the set must fail, keploy test non-zero (exit $RC, status $(set_status))"
fi
yq -r '.unclaimed_calls[]? | select(.before_set == true) | .actual_summary // ""' "$(REPORT)" 2>/dev/null | grep -q '/publish' ||
    fail "13 (HTTP, changed startup publish): the report does not name the publish among the calls made before the set's first test: $(grep -n -A4 'unclaimed' "$(REPORT)" | head -14)"
# The report's yaml doubles the quote in "set's": match around it.
grep -m1 '^failure_reason:' "$(REPORT)" | grep -q "before the set.*first test" ||
    fail "13 (HTTP, changed startup publish): the reason does not say the call was made before the set's first test: $(grep -m1 '^failure_reason:' "$(REPORT)")"
[ "$(test_status get-c-ping-1)" = PASSED ] ||
    fail "13 (HTTP, changed startup publish): c-ping did not pass ($(test_status get-c-ping-1))"

if [ "$FAIL" -ne 0 ]; then
    echo "::error::$FAIL check(s) failed"
    exit 1
fi
echo "all checks passed"
