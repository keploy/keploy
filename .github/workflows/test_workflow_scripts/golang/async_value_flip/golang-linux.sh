#!/usr/bin/env bash
# E2E: an async lane whose value changes mid-recording replays, and keeps
# replaying after keploy test --remove-unused-mocks.
#
# The app (./app, role svc) polls a config service (role cfg, outside keploy)
# in the background, declared as an async lane in keploy.yml, and answers
# GET /value with the value its last poll read. The recording: /healthz, then
# /value seven times (v1); the config flips to v2; /value three times (v2).
# The first v2 epoch is recorded after 8 tests began, so a replay serves it
# from the 9th test (/value 8) on (models.AsyncMeta.AnchorPos).
#
# For each lane type (http: every poll recorded; httpPoll: one epoch per value):
#   1. keploy test: every test passes. Test 8 (/value 7) used to be answered
#      v2: the test set's staging counted as a test window, and every epoch
#      was served one test early.
#   2. keploy test --remove-unused-mocks: every test passes; the epochs a
#      replay serves (per position, the last recorded) are all still there,
#      and, for the http lane, the ones it never serves are pruned. The
#      prune used to delete every epoch recorded after the first tests: no
#      epoch is ever consumed.
#   3. keploy test again: every test passes (it used to answer /value 8-10
#      v1).
#
# Readiness is event-driven: the config service answering, the app's port,
# the test files keploy writes, and the config service's count of v2 polls it
# answered. Ports: 8098 (app), 9098 (config service). Env: RECORD_BIN,
# REPLAY_BIN.
set -uo pipefail
# This lane guards the switch-off prune (--remove-unused-mocks) of async
# epochs, which the proven switch ignores (design v14 U18). So it runs keploy
# with the switch off (KEPLOY_PROVEN=0), where that still works: the
# open-source build's default, and keploy enterprise's on Windows and macOS.
# Unset, keploy enterprise runs the proven model on Linux.
export KEPLOY_PROVEN=0

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-$RECORD_BIN}"
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP=8098
CFG=9098
FAIL=0
fail() {
    echo "::error::$*"
    FAIL=$((FAIL + 1))
}

WORK="${WORK:-$(mktemp -d)}"
mkdir -p "$WORK"
echo "work dir: $WORK"
(cd "$SRC" && go build -o "$WORK/app" ./app) || { echo "::error::build failed"; exit 1; }

listening() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
# wait_listen <port> <pid>: until something listens on port; fails at once
# when pid (the process that should start it) has exited.
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
newest_run() { ls -1d keploy/reports/test-run-* 2>/dev/null | sort -t- -k3 -n | tail -n1; }
REPORT() { echo "$(newest_run)/test-set-0-report.yaml"; }
set_status() { grep -m1 '^status:' "$(REPORT)" 2>/dev/null | awk '{print $2}'; }

CFG_PID=""
REC_PID=""
cleanup() {
    [ -n "$REC_PID" ] && stop_pid "$REC_PID"
    [ -n "$CFG_PID" ] && kill "$CFG_PID" 2>/dev/null
}
trap cleanup EXIT

for p in "$APP" "$CFG"; do
    if listening "$p"; then echo "::error::port $p is in use"; exit 1; fi
done

# value <k> <want>: GET /value while recording.
value() {
    local got
    got=$(curl -s "http://127.0.0.1:$APP/value")
    [ "$got" = "{\"value\":\"$2\"}" ] || fail "$LANE: GET /value $1 answered $got while recording, want $2"
}

TESTS=11
record() {
    rm -rf keploy
    "$WORK/app" cfg >cfg.log 2>&1 &
    CFG_PID=$!
    wait_listen "$CFG" "$CFG_PID" || { echo "::error::the config service did not start"; exit 1; }
    sudo -E env PATH="$PATH" "$RECORD_BIN" record -c "$WORK/app svc" \
        --generateGithubActions=false --disable-tele >record.log 2>&1 </dev/null &
    REC_PID=$!
    wait_listen "$APP" "$REC_PID" || { echo "::error::the app did not start under keploy record"; tail -30 record.log; exit 1; }
    curl -fsS "http://127.0.0.1:$APP/healthz" >/dev/null || fail "$LANE: GET /healthz failed while recording"
    for k in 1 2 3 4 5 6 7; do value "$k" v1; done
    # The value changes once the 8 tests so far are written, so the recorder
    # anchors it after them, and before /value 8 begins: once the config
    # service answered two v2 polls, the app read the first one, which keploy
    # recorded before passing it on (the app polls one poll after another).
    wait_tests 8 "$REC_PID" || fail "$LANE: keploy record wrote $(test_files) test cases before the flip, want 8"
    curl -fsS "http://127.0.0.1:$CFG/flip" >/dev/null || fail "$LANE: the flip failed"
    until [ "$(curl -s "http://127.0.0.1:$CFG/served?value=v2")" -ge 2 ] 2>/dev/null; do
        kill -0 "$REC_PID" 2>/dev/null || { fail "$LANE: keploy record exited before the app read v2"; break; }
        kill -0 "$CFG_PID" 2>/dev/null || { fail "$LANE: the config service exited before the app read v2"; break; }
        sleep 0.1
    done
    for k in 8 9 10; do value "$k" v2; done
    wait_tests "$TESTS" "$REC_PID" || fail "$LANE: keploy record wrote $(test_files) test cases, want $TESTS"
    stop_pid "$REC_PID"
    REC_PID=""
    kill "$CFG_PID" 2>/dev/null
    wait "$CFG_PID" 2>/dev/null
    CFG_PID=""
    own
}

# replay <name> [flags...]: keploy test of the recording, the config service
# down; RC is its exit code.
replay() {
    local name="$1" before
    shift
    before="$(newest_run)"
    sudo -E env PATH="$PATH" "$REPLAY_BIN" test -c "$WORK/app svc" --delay 1 --health-path /healthz --api-timeout 30 \
        --generateGithubActions=false --disable-tele "$@" >"$name.log" 2>&1 </dev/null
    RC=$?
    own
    if [ "$(newest_run)" = "$before" ] || [ ! -f "$(REPORT)" ]; then
        fail "$name: keploy test wrote no report (exit $RC)"
        tail -40 "$name.log"
    fi
}
expect_pass() {
    if [ "$RC" -ne 0 ] || [ "$(set_status)" != PASSED ]; then
        fail "$1: the set must pass (exit $RC, status $(set_status))"
        yq -N '.tests[] | select(.status != "PASSED") | [.test_case_id, .status, (.result.body_result[0].expected // ""), (.result.body_result[0].actual // "")] | @tsv' "$(REPORT)" 2>/dev/null | head -20
    fi
}

# epochs <mocks.yaml>: one line per async mock: lane, position, seq, value.
epochs() {
    # The value out of the body, which @tsv may quote ("{""value"":""v1""}").
    yq -N 'select(.async != null) | [.async.lane, .async.anchorPos, .async.seq, (.spec.resp.body | sub("\n"; ""))] | @tsv' "$1" |
        sed -E 's/\t[^\t]*value"+:"+([A-Za-z0-9_-]+)[^\t]*$/\t\1/'
}
# served_epochs <mocks.yaml>: per lane and position, the epoch a replay
# serves there (the last recorded), sorted.
served_epochs() {
    epochs "$1" | sort -t$'\t' -k1,1 -k2,2n -k3,3n | awk -F'\t' '{ last[$1 "\t" $2] = $0 } END { for (k in last) print last[k] }' | sort
}

for LANE in http httpPoll; do
    echo "== lane type $LANE =="
    mkdir -p "$WORK/$LANE" && cd "$WORK/$LANE" || exit 1
    cat >keploy.yml <<YML
async:
    lanes:
        - name: config-watch
          type: $LANE
          match:
              pathRegex: "^/config\$"
          matchQuery:
              watch: "true"
YML
    record
    cp keploy/test-set-0/mocks.yaml mocks.recorded.yaml
    echo "recorded: $(test_files) tests, $(epochs mocks.recorded.yaml | wc -l) async mocks (v2: $(epochs mocks.recorded.yaml | awk -F'\t' '$4 == "v2"' | wc -l))"
    # The recording the checks below need: v2 first recorded after 8 tests.
    first_v2=$(epochs mocks.recorded.yaml | awk -F'\t' '$4 == "v2"' | sort -t$'\t' -k3,3n | head -n1 | cut -f2)
    [ "$first_v2" = 8 ] || fail "$LANE: the first v2 epoch is at position ${first_v2:-none}, want 8: the recording does not show the change where it happened"

    replay "$LANE-replay"
    expect_pass "$LANE: keploy test"

    replay "$LANE-prune" --remove-unused-mocks --debug
    expect_pass "$LANE: keploy test --remove-unused-mocks"
    grep -Eq '"keptAsAsyncEpoch": ?[1-9]' "$LANE-prune.log" ||
        fail "$LANE: the prune kept no epoch as one a replay serves (keptAsAsyncEpoch in $LANE-prune.log): it did not run, or kept the v2 epoch for another reason"
    if [ "$(served_epochs mocks.recorded.yaml)" != "$(served_epochs keploy/test-set-0/mocks.yaml)" ]; then
        fail "$LANE: the prune changed the epochs a replay serves: $(diff <(served_epochs mocks.recorded.yaml) <(served_epochs keploy/test-set-0/mocks.yaml) | grep '^[<>]' | head -5 | tr '\n' ' ')"
    fi
    kept=$(epochs keploy/test-set-0/mocks.yaml | wc -l)
    echo "pruned: $kept of $(epochs mocks.recorded.yaml | wc -l) async mocks kept"
    if [ "$LANE" = http ] && [ "$kept" -ge "$(epochs mocks.recorded.yaml | wc -l)" ]; then
        fail "$LANE: the prune kept every async mock, the ones a replay never serves among them"
    fi

    replay "$LANE-after-prune"
    expect_pass "$LANE: keploy test after the prune"
    if grep -q 'WARNING: DATA RACE' ./*.log; then fail "$LANE: data race in a keploy log"; fi
    cd "$WORK" || exit 1
done

if [ "$FAIL" -ne 0 ]; then
    echo "$FAIL check(s) failed"
    exit 1
fi
echo "all checks passed"
