#!/usr/bin/env bash
# E2E (KEPLOY_PROVEN=1): keploy test answers a call keploy's judging marked as
# the application's own background work again when the application makes it
# once more than recorded, and never requires it; an unmarked copy made once
# more fails its test, naming the recording it repeats.
#
# The app (app/main.go) stamps an API key's use in a key service
# (upstream/main.go): POST /keys/k1/use reads the key and then makes the same
# POST /keys/k1/stamp STAMPS times, as a stamp on the application's clock
# fires more or less often in a request's window. The recording stamps twice,
# so the use's test lists two copies of the stamp in mappings.yaml. Judging
# (keploy enterprise's) marks the copy recorded last when a replay makes the
# call once more than recorded ("more_calls: ..." on that copy's entry) and
# keeps the first copy required. This script writes that mark as judging
# writes it, then replays the recording with keploy test, the key service
# down, four times with the same mappings.yaml but for the mark:
#
#   marked, 3 stamps    the third stamp repeats the marked copy: answered
#                       again with it, unchecked (answer: background_again),
#                       the run says so, and every test passes;
#   unmarked, 3 stamps  the third stamp is one call more than recorded: the
#                       use's test fails on an extra_call miss that names the
#                       copy recorded last, says the call was recorded 2
#                       times and what it writes (HTTP POST);
#   marked, 1 stamp     the marked copy is never made: it is not required,
#                       every test passes, and the use's result lists it as
#                       an unused background recording;
#   marked, 0 stamps    the first copy, unmarked, is never made: it stays
#                       required, and the use's test fails expecting it,
#                       never the marked copy.
#
# Not here, with the reason: keploy test's rule for a call made between two
# tests (charged to no test, listed in the set's background_calls) and its
# serving of a set-aside test's recordings to the tests after it. keploy test
# sends each test as soon as the one before it is judged, with no wall-clock
# wait (replay is never slowed down), so no application can place a call in the
# gap between two tests on purpose; and a test is set aside only by
# enterprise's verdicts gate (config.Test.SkipReasons is never read from
# keploy.yml). Both are proven by unit rows (pkg/agent/proxy
# between_tests_miss_test.go, pkg/service/replay background_calls_test.go,
# pkg/service/agent and pkg/service/replay set-aside rows) and, on keploy
# mock's path, by enterprise's Go lane.
#
# Nothing here rests on a time: STAMPS sets how many stamps each run makes,
# and the script checks by the app's own lines that each run made them.
# Readiness is event-driven: the ports, the test files keploy writes.
#
# Only this build's replayer knows the marks: a release replaying is skipped.
# The recording may be a release's (record_latest_replay_build): the copies
# the mark is written on are recorded as any proven recording records them.
#
# Never `pgrep keploy`/`pkill keploy` here: keploy is stopped by its own pid.
# Ports: 8097 (app), 9097 (key service). Env: RECORD_BIN, REPLAY_BIN.
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-$RECORD_BIN}"
case "$REPLAY_BIN" in
    */build/keploy | */build-no-race/keploy) ;;
    *)
        echo "$REPLAY_BIN predates keploy test's background marks; skipping suite"
        exit 0
        ;;
esac

SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP=8097
UP=9097
SET=test-set-0
FAIL=0
fail() {
    echo "::error::$*"
    FAIL=$((FAIL + 1))
}

WORK="$(mktemp -d)"
echo "work dir: $WORK"
(cd "$SRC" && go build -o "$WORK/app" ./app && go build -o "$WORK/upstream" ./upstream) || { echo "::error::build failed"; exit 1; }
cd "$WORK" || exit 1

listening() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
# pid_running PID: whether PID still runs, by its state in /proc, which shows
# root's processes to this user too (keploy runs under sudo, and kill -0 from
# this user fails on a sudo that is alive).
pid_running() {
    local st
    case $1 in "" | *[!0-9]*) return 1 ;; esac
    { read -r st <"/proc/$1/stat"; } 2>/dev/null || return 1
    st=${st##*) }
    [ "${st%% *}" != Z ]
}
# wait_listen <port> <pid>: until something listens on port; fails at once
# when pid (the process that should start it) has exited.
wait_listen() {
    until listening "$1"; do
        pid_running "$2" || return 1
        sleep 0.2
    done
}
test_files() { find keploy/$SET/tests -maxdepth 1 -name '*.yaml' -size +0c 2>/dev/null | wc -l; }
own() { sudo chown -R "$(id -u):$(id -g)" . 2>/dev/null || true; }
stop_pid() {
    local sudo_pid="$1" child
    child="$(pgrep -P "$sudo_pid" 2>/dev/null | head -n1)"
    [ -n "$child" ] && sudo kill -INT "$child" 2>/dev/null
    wait "$sudo_pid" 2>/dev/null
}
newest_run() { ls -1d keploy/reports/test-run-* 2>/dev/null | sort -t- -k3 -n | tail -n1; }
REPORT() { echo "$(newest_run)/$SET-report.yaml"; }
# plain LOG: LOG without keploy's colors.
plain() { local esc=$'\033'; sed -e "s/${esc}\[[0-9;]*m//g" "$1"; }
# has LOG TEXT: LOG, without its colors, holds TEXT (a fixed string). awk
# reads to the end: a grep -q that quits at its match could end plain with
# SIGPIPE, and under pipefail that reads as no match.
has() { plain "$1" | awk -v s="$2" 'index($0, s) { n++ } END { exit n ? 0 : 1 }'; }
# use_field <yq path under the use's result>: that field of the use's test in
# the newest report, one value a line.
use_field() { T="$USE_TEST" yq -N ".tests[] | select(.test_case_id == strenv(T)) | $1" "$(REPORT)" 2>/dev/null; }

UP_PID=""
start_upstream() {
    ./upstream >upstream.log 2>&1 &
    UP_PID=$!
    wait_listen "$UP" "$UP_PID" || { echo "::error::the key service did not start"; cat upstream.log; exit 1; }
}
stop_upstream() {
    [ -n "$UP_PID" ] && kill "$UP_PID" 2>/dev/null && wait "$UP_PID" 2>/dev/null
    UP_PID=""
}
REC_PID=""
cleanup() {
    [ -n "$REC_PID" ] && stop_pid "$REC_PID"
    stop_upstream
}
trap cleanup EXIT

# send <method> <path> <want body>: one request while recording.
send() {
    local body
    body=$(curl -sS -X "$1" "http://127.0.0.1:$APP$2") || { echo "::error::$1 $2 failed while recording"; tail -30 record.log; exit 1; }
    [ "$body" = "$3" ] || { echo "::error::$1 $2 answered $body while recording, want $3"; tail -30 record.log; exit 1; }
}

# ── record ──────────────────────────────────────────────────────────────────
echo "======== Recording (each use stamps the key twice) ========"
rm -rf keploy keploy.yml
start_upstream
sudo -E env -u KEPLOY_PROVEN PATH="$PATH" KEPLOY_PROVEN=1 STAMPS=2 "$RECORD_BIN" record -c "./app" \
    --generateGithubActions=false --disable-tele >record.log 2>&1 </dev/null &
REC_PID=$!
wait_listen "$APP" "$REC_PID" || { echo "::error::the app did not start under keploy record"; tail -30 record.log; exit 1; }
send POST /keys/k1 '{"created":"k1"}'
send POST /keys/k1/use '{"used":"k1"}'
until [ "$(test_files)" -ge 2 ]; do
    pid_running "$REC_PID" || { echo "::error::keploy record wrote $(test_files) test cases, want 2"; tail -30 record.log; exit 1; }
    sleep 0.2
done
stop_pid "$REC_PID"
REC_PID=""
own
stop_upstream

has record.log 'stamp: k1 stamped (2 of 2)' || { echo "::error::the recording did not stamp the key twice"; tail -30 record.log; exit 1; }
use_file="$(grep -l '/keys/k1/use' keploy/$SET/tests/*.yaml 2>/dev/null | head -1)"
[ -n "$use_file" ] || { echo "::error::the use was not recorded as a test case"; ls keploy/$SET/tests; exit 1; }
USE_TEST="$(basename "$use_file" .yaml)"
[ -f keploy/$SET/mappings.yaml ] || { echo "::error::the proven recording wrote no mappings.yaml"; tail -30 record.log; exit 1; }

# The use's own recordings, as mappings.yaml lists them under its test.
own_mocks() {
    awk -v test="$USE_TEST" '
        /^[^ ]/ { cur = "" }
        /^    - id: / { cur = $3 }
        cur == test && $1 == "-" && $2 == "name:" { print $3 }
    ' keploy/$SET/mappings.yaml
}
# The recordings of the stamp, by their requests in mocks.yaml.
stamp_mocks() { yq -N 'select(.spec.req.url == "*/keys/k1/stamp") | .name' keploy/$SET/mocks.yaml; }
mapfile -t STAMP_COPIES < <(grep -Fxf <(stamp_mocks) <(own_mocks) | sort -V)
echo "$USE_TEST's copies of the stamp: ${STAMP_COPIES[*]:-none}"
if [ "${#STAMP_COPIES[@]}" -ne 2 ]; then
    echo "::error::$USE_TEST should list two copies of the stamp (POST /keys/k1/stamp), it lists ${#STAMP_COPIES[@]}"
    sed -n '1,60p' keploy/$SET/mappings.yaml
    exit 1
fi
FIRST="${STAMP_COPIES[0]}"
LAST="${STAMP_COPIES[1]}"
cp -a keploy keploy.rec

# mark_last: write judging's mark on the copy recorded last, under the use's
# test, as judging writes it (a "background: more_calls: ..." line on that
# entry).
WHY="more_calls: round F replayed $SET/$USE_TEST with its inputs as recorded and the application made this call (a write: HTTP POST) once more than recorded in its window, with every other call answered by its own recording, so the extra copy is the application's own work on its clock (a timer, a start-up job, a delayed write); keploy answers each copy of it in the window with this recording and never requires it; delete this line to require it again"
mark_last() {
    local quoted="'${WHY//\'/\'\'}'"
    awk -v test="$USE_TEST" -v mock="$LAST" -v why="$quoted" '
        /^[^ ]/ { cur = "" }
        /^    - id: / { cur = $3 }
        { print }
        cur == test && $1 == "-" && $2 == "name:" && $3 == mock {
            printf "%s  background: %s\n", substr($0, 1, index($0, "-") - 1), why
            marked++
        }
        END { if (marked != 1) exit 1 }
    ' keploy/$SET/mappings.yaml >keploy/$SET/mappings.yaml.new &&
        mv keploy/$SET/mappings.yaml.new keploy/$SET/mappings.yaml
}

# replay <name> <stamps> <marked|unmarked>: keploy test of the recording, the
# key service down, the app making <stamps> stamps per use, with judging's
# mark on the copy recorded last when marked. OUT is its output, STATUS the
# set's status in its report, RC its exit code.
replay() {
    local name="$1" stamps="$2" marked="$3" before
    echo "======== Replaying: $name ($stamps stamps per use, $marked) ========"
    rm -rf keploy && cp -a keploy.rec keploy
    if [ "$marked" = marked ]; then
        mark_last || fail "$name: could not write the mark on $LAST under $USE_TEST"
        grep -n 'background: ' keploy/$SET/mappings.yaml | cut -c1-120
    fi
    OUT="$name.log"
    before="$(newest_run)"
    sudo -E env -u KEPLOY_PROVEN PATH="$PATH" KEPLOY_PROVEN=1 STAMPS="$stamps" "$REPLAY_BIN" test -c "./app" \
        --delay 1 --health-path /healthz --generateGithubActions=false --disable-tele >"$OUT" 2>&1 </dev/null
    RC=$?
    own
    STATUS=none
    if [ "$(newest_run)" = "$before" ] || [ ! -f "$(REPORT)" ]; then
        fail "$name: keploy test wrote no report (exit $RC)"
        tail -40 "$OUT"
        return
    fi
    STATUS="$(grep -m1 '^status:' "$(REPORT)" | awk '{print $2}')"
    echo "$name: $SET $STATUS (exit $RC)"
    has "$OUT" "stamps per use: $stamps" || fail "$name: the app did not start with $stamps stamps per use"
}
# expect_set <name> <PASSED|FAILED> <why>: the set's status and keploy's exit.
expect_set() {
    local want_rc=0
    [ "$2" = FAILED ] && want_rc=1
    if [ "$STATUS" != "$2" ] || { [ "$want_rc" = 0 ] && [ "$RC" -ne 0 ]; } || { [ "$want_rc" = 1 ] && [ "$RC" -eq 0 ]; }; then
        fail "$1: the set is $STATUS (exit $RC), want $2: $3"
        grep -nE '^status:|^    status:|test_case_id|kind: |closest_mock: |name: mock-' "$(REPORT)" 2>/dev/null | head -40
    fi
}

# The recording's two copies answer the first two stamps; the third repeats
# the marked copy and is answered again with it, unchecked.
replay marked-3 3 marked
expect_set marked-3 PASSED "the third stamp, a copy of the marked $LAST, was not answered again"
has "$OUT" "stamp: k1 stamped (3 of 3)" || fail "marked-3: the app's third stamp was not answered"
has "$OUT" "1 background call answered again, unchecked ($LAST)" || fail "marked-3: the run does not say $LAST answered one call again, unchecked"
[ "$(use_field '.answers[] | select(.answer == "background_again") | .closest_mock')" = "$LAST" ] ||
    fail "marked-3: $USE_TEST's result does not file the third stamp as answered again by $LAST (answer: background_again)"

# Without the mark the third stamp is one call more than recorded: it is
# refused, and the use's test fails on a miss that names the copy recorded
# last, says how many times the recording holds the call and what it writes.
replay unmarked-3 3 unmarked
expect_set unmarked-3 FAILED "one stamp more than recorded, unmarked, must fail $USE_TEST"
has "$OUT" "stamp: k1 stamped (2 of 3)" || fail "unmarked-3: the app's first two stamps were not answered"
has "$OUT" "stamp: k1 stamp 3 of 3 not answered" || fail "unmarked-3: the app's third stamp was answered"
miss='.failure_info.unmatched_calls[] | select(.kind == "extra_call")'
[ "$(use_field "[$miss] | length")" = 1 ] || fail "unmarked-3: $USE_TEST's result does not hold one extra_call miss"
[ "$(use_field "$miss | .closest_mock")" = "$LAST" ] || fail "unmarked-3: the miss does not name $LAST, the copy recorded last"
[ "$(use_field "$miss | .recorded")" = 2 ] || fail "unmarked-3: the miss does not say the stamp was recorded 2 times"
[ "$(use_field "$miss | .write")" = "HTTP POST" ] || fail "unmarked-3: the miss does not say what the call writes (HTTP POST)"
has "$OUT" "the call repeats $LAST, which answered it already" || fail "unmarked-3: the failure does not say the call repeats $LAST"
has "$OUT" "once more than the 2 time(s) it was recorded" || fail "unmarked-3: the failure does not say the call was made once more than its 2 recordings"

# One stamp: the marked copy is never made, and is not required.
replay marked-1 1 marked
expect_set marked-1 PASSED "the marked $LAST, never made, was required"
has "$OUT" "stamp: k1 stamped (1 of 1)" || fail "marked-1: the app's stamp was not answered"
use_field '.failure_info.unused_background[].name' | grep -Fxq "$LAST" ||
    fail "marked-1: $USE_TEST's result does not list $LAST as a background recording it did not use"

# No stamp: the first copy, unmarked, is never made, and stays required: the
# use's test fails expecting it, and never expects the marked copy.
replay marked-0 0 marked
expect_set marked-0 FAILED "$FIRST, the unmarked copy, never made, must stay required"
unconsumed="$(use_field '.failure_info.unconsumed_mocks[].name' | tr '\n' ' ')"
echo "marked-0: $USE_TEST unconsumed: $unconsumed"
case " $unconsumed" in *" $FIRST "*) ;; *) fail "marked-0: $USE_TEST's failure does not name $FIRST, the required copy left unmade" ;; esac
case " $unconsumed" in *" $LAST "*) fail "marked-0: $USE_TEST's failure names $LAST, the marked copy, which is never required" ;; esac

for f in marked-3 unmarked-3 marked-1 marked-0; do
    if grep -q "WARNING: DATA RACE" "$f.log"; then
        fail "$f: a data race"
    fi
done
if [ "$FAIL" -gt 0 ]; then
    echo "::error::$FAIL check(s) failed"
    for f in marked-3 unmarked-3 marked-1 marked-0; do
        echo "--- $f.log"
        plain "$f.log" | grep -nE 'stamp|background|extra|repeats|not made|FAILED|PASSED|ERROR|WARN' | cut -c1-240 | tail -30
    done
    exit 1
fi
echo "PASS: a marked background call made once more is answered again, a marked copy is never required, and an unmarked copy made once more fails its test naming the copy recorded last."
