#!/usr/bin/env bash
# E2E: a fire-and-forget call is the test's own under the proven switch, however
# late its bytes reach keploy's proxy (design v14 §5.2, verify-v14 M1): on a
# connection it opens (the agent's count of the application's connections) and
# on one it opened long before and keeps (the bytes on the connections the
# proxy holds).
#
# The app's /a-publish starts a publish to a broker without waiting for it and
# answers 200 ms later; /b-other takes 3 s; /c-ping answers at once. The
# publish goes over HTTP (a test-scoped mock) or a plain TCP line protocol (a
# Generic, session-tier mock), on a connection of its own (http, tcp) or on one
# the app dialled at startup and keeps (httpkeep, tcpkeep). Each recording is
# replayed with every non-SYN packet to the proxy's port held 1 s (tc netem on
# lo): a connection's proxy side completes, and a kept connection's bytes
# arrive, a second after /a-publish has answered.
#
#   1. HTTP, the publish in the middle (N, then N+1): every test passes, and
#      the publish's mock is consumed in N.
#   2. HTTP, the publish last: every test passes.
#   3. TCP, in the middle, the recorded message: every test passes (no false
#      red).
#   4. TCP, in the middle, a changed message: the set fails, and N fails for
#      it (never only N+1).
#   5. TCP, last, a changed message: the set fails, keploy test exits
#      non-zero (never green with the call unseen).
#   6. TCP kept, in the middle, the recorded message: every test passes.
#   7. TCP kept, in the middle, a changed message: N fails for it.
#   8. TCP kept, last, a changed message: the set fails, keploy test exits
#      non-zero.
#   9. HTTP kept, the publish last: every test passes.
#   Each recording also replays as is, without the hold: it passes; and the
#   kept TCP ones with a changed message, without the hold, fail as 7 and 8 do.
#
# Env: RECORD_BIN, REPLAY_BIN. Ports: 8080 (app), 9099 (broker, HTTP), 9098
# (broker, TCP), 16789 (keploy's proxy).
set -uo pipefail
# The proven switch, stated for every keploy this lane runs: the steps that
# say KEPLOY_PROVEN=1 run under it, and every other one with it off
# (KEPLOY_PROVEN=0, the default on Windows and macOS), as this lane always ran
# them. Unset, a Linux keploy runs the proven model.
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

hold_on() {
    sudo modprobe sch_netem 2>/dev/null || true
    sudo tc qdisc add dev lo root handle 1: prio bands 4 priomap 1 2 2 2 1 2 0 0 1 1 1 1 1 1 1 1 &&
        sudo tc qdisc add dev lo parent 1:4 handle 40: netem delay 1000ms limit 100000 &&
        sudo tc filter add dev lo protocol ip parent 1:0 prio 1 u32 \
            match ip protocol 6 0xff match ip dport "$PROXY" 0xffff match u8 0x00 0x02 at 33 flowid 1:4 ||
        { echo "::error::cannot hold the packets to keploy's proxy (tc netem)"; hold_off; exit 1; }
}
hold_off() { sudo tc qdisc del dev lo root 2>/dev/null || true; }
# held: how many packets netem has held so far (a held case that held none
# proved nothing).
held() { sudo tc -s qdisc show dev lo | awk '/qdisc netem 40:/ { getline; print $4; exit }'; }
replay_held() {
    local before
    before="$(held)"
    replay "$@"
    [ "$(held)" -gt "${before:-0}" ] || fail "$1, $2: netem held no packet to keploy's proxy"
}

BROKER_PID=""
REC_PID=""
cleanup() {
    hold_off
    [ -n "$REC_PID" ] && stop_pid "$REC_PID"
    [ -n "$BROKER_PID" ] && kill "$BROKER_PID" 2>/dev/null
}
trap cleanup EXIT

for p in "$APP" 9098 9099 "$PROXY"; do
    if listening "$p"; then echo "::error::port $p is in use"; exit 1; fi
done

# record <dir> <proto> <order: mid|last>: a recording of /c-ping (the
# readiness probe), then /a-publish and /b-other in that order or the
# reverse.
broker_of() { case "$1" in tcp | tcpkeep) echo 127.0.0.1:9098 ;; *) echo 127.0.0.1:9099 ;; esac; }

record() {
    local dir="$1" proto="$2" order="$3" broker
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
    if [ "$order" = last ]; then
        curl -s "http://127.0.0.1:$APP/b-other" >/dev/null
        curl -s "http://127.0.0.1:$APP/a-publish?id=1&hold=200ms" >/dev/null
    else
        curl -s "http://127.0.0.1:$APP/a-publish?id=1&hold=200ms" >/dev/null
        curl -s "http://127.0.0.1:$APP/b-other" >/dev/null
    fi
    wait_tests 3 "$REC_PID" || fail "$dir: keploy record wrote $(test_files) test cases, want 3"
    # The publish is recorded once the broker answered it; the app logs it.
    until grep -q "publish 1: broker said" record.log; do kill -0 "$REC_PID" 2>/dev/null || break; sleep 0.2; done
    stop_pid "$REC_PID"
    REC_PID=""
    kill "$BROKER_PID" 2>/dev/null
    wait "$BROKER_PID" 2>/dev/null
    BROKER_PID=""
    own
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
# expect_n_fails <step>: the set fails, keploy test exits non-zero, and the
# publish's test is one that fails.
expect_n_fails() {
    if [ "$RC" -eq 0 ] || [ "$(set_status)" != FAILED ]; then
        fail "$1: the set must fail, keploy test non-zero (exit $RC, status $(set_status))"
    fi
    [ "$(test_status get-a-publish-1)" = FAILED ] ||
        fail "$1: the publish's own test passed ($(test_status get-a-publish-1)); its changed call was blamed elsewhere or not seen"
}

echo "== recordings =="
record http-mid http mid
record http-last http last
record tcp-mid tcp mid
record tcp-last tcp last
record tcpkeep-mid tcpkeep mid
record tcpkeep-last tcpkeep last
record httpkeep-last httpkeep last

for d in http-mid http-last tcp-mid tcp-last tcpkeep-mid tcpkeep-last httpkeep-last; do
    proto=${d%%-*}
    replay "$d" "$d-as-recorded" "$proto"
    expect_pass "$d replayed as recorded"
done
echo "== the kept TCP connection, a changed message, no hold =="
replay tcpkeep-mid changed "tcpkeep" "-changed"
expect_n_fails "TCP kept, middle, changed, no hold"
replay tcpkeep-last changed "tcpkeep" "-changed"
expect_n_fails "TCP kept, last, changed, no hold"

hold_on
echo "== 1. HTTP, the publish in the middle, its connection held =="
replay_held http-mid held "http"
expect_pass "1 (HTTP, middle)"
yq -e '.tests[] | select(.test_case_id == "get-a-publish-1") | .mock_mismatches.actual_mocks | length > 0' "$(REPORT)" >/dev/null ||
    fail "1 (HTTP, middle): the publish's mock was not consumed in its own test"
echo "== 2. HTTP, the publish last, its connection held =="
replay_held http-last held "http"
expect_pass "2 (HTTP, last)"
echo "== 3. TCP, the publish in the middle, the recorded message, held =="
replay_held tcp-mid held "tcp"
expect_pass "3 (TCP, middle, as recorded)"
echo "== 4. TCP, the publish in the middle, a changed message, held =="
replay_held tcp-mid held-changed "tcp" "-changed"
expect_n_fails "4 (TCP, middle, changed)"
echo "== 5. TCP, the publish last, a changed message, held =="
replay_held tcp-last held-changed "tcp" "-changed"
expect_n_fails "5 (TCP, last, changed)"
echo "== 6. TCP kept, the publish in the middle, the recorded message, its bytes held =="
replay_held tcpkeep-mid held "tcpkeep"
expect_pass "6 (TCP kept, middle, as recorded)"
echo "== 7. TCP kept, the publish in the middle, a changed message, its bytes held =="
replay_held tcpkeep-mid held-changed "tcpkeep" "-changed"
expect_n_fails "7 (TCP kept, middle, changed)"
echo "== 8. TCP kept, the publish last, a changed message, its bytes held =="
replay_held tcpkeep-last held-changed "tcpkeep" "-changed"
expect_n_fails "8 (TCP kept, last, changed)"
echo "== 9. HTTP kept, the publish last, its bytes held =="
replay_held httpkeep-last held "httpkeep"
expect_pass "9 (HTTP kept, last)"
hold_off

if [ "$FAIL" -ne 0 ]; then
    echo "::error::$FAIL check(s) failed"
    exit 1
fi
echo "all checks passed"
