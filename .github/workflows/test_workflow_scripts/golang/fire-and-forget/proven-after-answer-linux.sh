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
#
# Not here, a known limit: a call made after the set's LAST test is judged by
# a read of the set's misses at its end (unit tests: unclaimed_calls_test.go),
# but only if the app makes it before that read; keploy reads without waiting
# on the app's clock and stops the app right after, so whether a delayed
# publish lands before the read is load-dependent, and no case here asserts on
# it (P101-b: stop the app, then read).
#
# Env: RECORD_BIN, REPLAY_BIN. Ports: 8080 (app), 9099 (broker, HTTP), 9098
# (broker, TCP), 16789 (keploy's proxy).
set -uo pipefail

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
    sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$RECORD_BIN" record -c "env PROTO=$proto BROKER=$broker ../ff-app" \
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

# replay <dir> <name> <proto> [suffix]: keploy test of dir's recording, the
# broker down; RC is its exit code.
replay() {
    local dir="$1" name="$2" proto="$3" suffix="${4:-}" broker before
    broker="$(broker_of "$proto")"
    REPLAY_DIR="$WORK/$dir"
    cd "$REPLAY_DIR" || exit 1
    before="$(newest_run)"
    sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$REPLAY_BIN" test -c "env PROTO=$proto BROKER=$broker SUFFIX=$suffix ../ff-app" \
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

if [ "$FAIL" -ne 0 ]; then
    echo "::error::$FAIL check(s) failed"
    exit 1
fi
echo "all checks passed"
