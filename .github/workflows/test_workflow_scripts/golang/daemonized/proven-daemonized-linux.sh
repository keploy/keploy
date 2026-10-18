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
#   3. keploy mock record, twice, the command starting a server daemon and
#      calling it: the call to the daemon's listener is served live, never
#      recorded; the daemon ends with the run, and the next run's daemon can
#      listen on its port again.
#   4. Two recordings, then keploy test of both sets, each app starting a
#      server daemon that ignores SIGTERM on one port as it starts: keploy
#      kills each once it makes no progress on its way out, before the next
#      app starts, so every app starts and both sets pass.
#
# Env: RECORD_BIN, REPLAY_BIN. Ports: 8080 (app), 18090 (upstream), 18091
# and 18092 (the daemons' servers).
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

for p in "$APP" "$UP" 18091 18092; do
    if listening "$p"; then echo "::error::port $p is in use"; exit 1; fi
done
start_upstream() {
    "$WORK/daemonapp" upstream "127.0.0.1:$UP" "$1" >upstream.log 2>&1 &
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
curl -s --max-time 120 "http://127.0.0.1:$APP/go"
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
said="$(grep -m1 'daemon said' replay.log)"
echo "$said"
case "$said" in
*"(exe $(readlink -f "$REPLAY_BIN")):"*) ;;
*) fail "replay: the daemon's parent was not keploy's CLI: ${said:-it said nothing}" ;;
esac
case "$said" in
*'200 OK {"path":"/from-daemon"}'*) ;;
*) fail "replay: the daemon's call was not answered with its recording: $said" ;;
esac
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

echo "== 3. keploy mock: a daemon's listener is the run's, and the daemon ends with the run =="
# The command keploy runs starts a server daemon (pg_ctl start's way), waits
# until it listens, and calls it. The call is to a listener of the run's own
# tree: served live, never recorded or mocked. When the command ends, so does
# the daemon, so the next run's daemon can listen on the same port.
SRV=18091
HITS="$WORK/server-hits.log"
cat >fixture.sh <<EOF
set -e
rm -f "$WORK/server.ready"
"$WORK/daemonapp" daemonize-serve 127.0.0.1:$SRV "$HITS" "$WORK/server.ready"
until [ -s "$WORK/server.ready" ]; do sleep 0.05; done
case "\$(cat "$WORK/server.ready")" in listen*) cat "$WORK/server.ready"; echo; exit 1 ;; esac
"$WORK/daemonapp" get http://127.0.0.1:$SRV/hi
EOF
hits() { grep -cx /hi "$HITS" 2>/dev/null || echo 0; }
mock_run() {
    sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$RECORD_BIN" mock record -c "sh $WORK/fixture.sh" --name "$1" \
        --disable-tele >"$1.log" 2>&1 </dev/null
}
# Two runs, one after the other: the second's daemon listens on the port the
# first's had, which it can only once the first's has ended with its run.
# (keploy mock replay under the switch needs a build that judges each test,
# keploy enterprise; a record is this build's run of the command.)
for run in 1 2; do
    mock_run "dm$run"
    RC=$?
    own
    [ "$RC" -eq 0 ] || { fail "mock record $run: exit $RC"; tail -30 "dm$run.log"; }
    [ "$(hits)" = "$run" ] || fail "mock record $run: the daemon's server answered $(hits) calls in all, want $run"
    if grep -q '/hi' "keploy/dm$run/mocks.yaml" 2>/dev/null; then
        fail "mock record $run: the call to the daemon's own listener was recorded as a mock: the daemon was taken for an outside dependency"
    fi
    if listening "$SRV"; then
        fail "mock record $run: the daemon still listens on $SRV after the run: it did not end with the run"
    fi
done

echo "== 4. two test sets, each app starting a server daemon that ignores SIGTERM =="
# Each app starts a daemon on one port as it starts, a daemon that does not
# stop when asked; the app of the next recording, and of the next test set,
# can only start once the earlier one's daemon is gone, which keploy kills
# once it makes no progress on its way out, before the next app starts.
DSRV=18092
DAPP="env DAEMON_SRV=127.0.0.1:$DSRV DEAF=1 ../daemonapp app 127.0.0.1:$APP 127.0.0.1:$UP"
mkdir -p "$WORK/sets" && cd "$WORK/sets" || exit 1
start_upstream up-sets.log
for set in 0 1; do
    sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$RECORD_BIN" record -c "$DAPP" \
        --generateGithubActions=false --disable-tele >"record-$set.log" 2>&1 </dev/null &
    REC_PID=$!
    wait_listen "$APP" "$REC_PID" || { fail "set $set: the app did not start under keploy record"; tail -30 "record-$set.log"; break; }
    curl -s --max-time 120 "http://127.0.0.1:$APP/go" >/dev/null
    until [ "$(find "keploy/test-set-$set/tests" -maxdepth 1 -name '*.yaml' -size +0c 2>/dev/null | wc -l)" -ge 1 ]; do
        kill -0 "$REC_PID" 2>/dev/null || break
        sleep 0.2
    done
    stop_pid "$REC_PID"
    REC_PID=""
    own
    grep -q "made no progress on its way out" "record-$set.log" ||
        fail "record $set: keploy did not stop the daemon that ignores SIGTERM"
    if listening "$DSRV"; then fail "record $set: the daemon still listens on $DSRV after the recording"; fi
done
stop_upstream
sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$REPLAY_BIN" test -c "$DAPP" \
    --delay 2 --generateGithubActions=false --disable-tele >replay-sets.log 2>&1 </dev/null
RC=$?
own
run="$(ls -1d keploy/reports/test-run-* 2>/dev/null | sort -V | tail -n1)"
for set in 0 1; do
    st="$(grep -m1 '^status:' "$run/test-set-$set-report.yaml" 2>/dev/null | awk '{print $2}')"
    [ "$st" = PASSED ] || fail "replay: test-set-$set is ${st:-not reported}, want PASSED (an app that could not start for a daemon left from the set before)"
done
[ "$RC" -eq 0 ] || { fail "replay of the two sets: exit $RC"; tail -40 replay-sets.log; }
[ "$(grep -c "made no progress on its way out" replay-sets.log)" -ge 2 ] ||
    fail "replay: keploy did not stop each set's daemon that ignores SIGTERM"
if listening "$DSRV"; then fail "replay: the daemon still listens on $DSRV after keploy test"; fi
cd "$WORK" || exit 1

if [ "$FAIL" -ne 0 ]; then
    echo "== $FAIL failure(s); logs =="
    tail -40 record.log
    exit 1
fi
echo "== daemonized members: all checks passed =="
