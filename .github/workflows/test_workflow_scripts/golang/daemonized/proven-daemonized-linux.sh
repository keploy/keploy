#!/usr/bin/env bash
# E2E: a daemon the application starts is a member of its run under the proven
# switch (design v14 §3.1: membership is set at fork, and a reparent changes
# nothing; P146). The app's GET /go calls an upstream itself, through a child,
# and through a daemon: a child that starts a grandchild in a session of its
# own and exits, as pg_ctl start and redis-server --daemonize do, so the
# grandchild is orphaned and reparented before it calls.
#
#   1. keploy record: all three calls are recorded, the daemon's among them.
#   2. keploy test, the upstream up: the set passes, and none of the three
#      calls reaches the upstream: each is answered from its mock. A daemon
#      that left the run's tree called the real upstream, neither mocked nor
#      judged, and the set passed all the same.
#
# Env: RECORD_BIN, REPLAY_BIN. Ports: 8080 (app), 18090 (upstream).
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-$RECORD_BIN}"
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP=8080
UP=18090
FAIL=0
fail() {
    echo "::error::$*"
    FAIL=$((FAIL + 1))
}

WORK="$(mktemp -d)"
chmod 755 "$WORK"
echo "work dir: $WORK"
(cd "$SRC" && GOWORK=off go build -o "$WORK/daemonapp" ./app) || { echo "::error::build failed"; exit 1; }
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

UP_PID=""
REC_PID=""
cleanup() {
    [ -n "$REC_PID" ] && stop_pid "$REC_PID"
    [ -n "$UP_PID" ] && kill "$UP_PID" 2>/dev/null
}
trap cleanup EXIT

for p in "$APP" "$UP"; do
    if listening "$p"; then echo "::error::port $p is in use"; exit 1; fi
done
start_upstream() {
    ./daemonapp upstream "127.0.0.1:$UP" "$1" >upstream.log 2>&1 &
    UP_PID=$!
    wait_listen "$UP" "$UP_PID" || { echo "::error::the upstream did not start"; exit 1; }
}
stop_upstream() { kill "$UP_PID" 2>/dev/null; wait "$UP_PID" 2>/dev/null; UP_PID=""; }
APPCMD="./daemonapp app 127.0.0.1:$APP 127.0.0.1:$UP"

echo "== 1. record =="
start_upstream up-record.log
sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$RECORD_BIN" record -c "$APPCMD" \
    --generateGithubActions=false --disable-tele >record.log 2>&1 </dev/null &
REC_PID=$!
wait_listen "$APP" "$REC_PID" || { echo "::error::the app did not start under keploy record"; tail -30 record.log; exit 1; }
curl -s "http://127.0.0.1:$APP/go"
wait_tests 1 "$REC_PID" || fail "keploy record wrote $(test_files) test cases, want 1"
stop_pid "$REC_PID"
REC_PID=""
stop_upstream
own
grep -m1 'daemon said' record.log
for path in /from-app /from-child /from-daemon; do
    grep -qx "$path" up-record.log || fail "record: the upstream never saw $path"
    grep -q "$path" keploy/test-set-0/mocks.yaml 2>/dev/null || fail "record: the call to $path was not recorded"
done

echo "== 2. replay, the upstream up =="
start_upstream up-replay.log
sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$REPLAY_BIN" test -c "$APPCMD" \
    --delay 2 --generateGithubActions=false --disable-tele >replay.log 2>&1 </dev/null
RC=$?
stop_upstream
own
grep -m1 'daemon said' replay.log
for path in /from-app /from-child /from-daemon; do
    if grep -qx "$path" up-replay.log; then
        fail "replay: the call to $path reached the real upstream: it was neither mocked nor judged"
    fi
done
report="$(ls -1d keploy/reports/test-run-* 2>/dev/null | sort -V | tail -n1)/test-set-0-report.yaml"
status="$(grep -m1 '^status:' "$report" 2>/dev/null | awk '{print $2}')"
if [ "$RC" -ne 0 ] || [ "$status" != PASSED ]; then
    fail "replay: the set must pass (exit $RC, status ${status:-none})"
    tail -40 replay.log
fi

if [ "$FAIL" -ne 0 ]; then
    echo "== $FAIL failure(s); logs =="
    tail -40 record.log
    exit 1
fi
echo "== daemonized members: all checks passed =="
