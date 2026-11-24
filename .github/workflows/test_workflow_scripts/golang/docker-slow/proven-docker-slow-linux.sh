#!/usr/bin/env bash
# E2E: under the proven switch, a native record names container destinations
# by the local Docker daemon's containers, and its start waited, before the
# app ran, for the daemon to list them, however long that took (a Docker
# Desktop waking its VM: 8 to 26 s), for an app that reaches no container.
# Here the app's one dependency is a server on this host, and DOCKER_HOST is a
# daemon (docker-slow.py, in a session of its own as a daemon runs) that
# answers its list of containers only after LIST_DELAY seconds.
#
#   1. keploy record: the app starts before the daemon has listed (the daemon
#      marks the moment it answers), its calls are recorded, and keploy never
#      says it waits for the daemon before the app starts.
#   2. keploy test, the daemon as slow: the set passes.
#
# Env: RECORD_BIN, REPLAY_BIN. Ports: 8080 (app), 18090 (upstream).
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
# shellcheck source=../../sudo_env.sh
source "$(dirname "${BASH_SOURCE[0]}")/../../sudo_env.sh" || exit 1

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-$RECORD_BIN}"
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP=8080
UP=18090
LIST_DELAY="${LIST_DELAY:-20}"
FAIL=0
fail() {
    echo "::error::$*"
    FAIL=$((FAIL + 1))
}

WORK="$(mktemp -d)"
chmod 755 "$WORK"
echo "work dir: $WORK"
# The app and its upstream are the daemonized e2e's: GET /go calls the
# upstream from the app, a child and a daemon.
(cd "$SRC/../daemonized" && GOWORK=off go build -o "$WORK/daemonapp" ./app) || { echo "::error::build failed"; exit 1; }
cd "$WORK" || exit 1

listening() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
pid_running() {
    local st
    case $1 in "" | *[!0-9]*) return 1 ;; esac
    { read -r st <"/proc/$1/stat"; } 2>/dev/null || return 1
    st=${st##*) }
    [ "${st%% *}" != Z ]
}
# wait_listen PORT PID: until PORT listens, while PID runs; a bound of two
# minutes only turns a hang into a failure.
wait_listen() {
    local i
    for i in $(seq 1 600); do
        listening "$1" && return 0
        pid_running "$2" || return 1
        sleep 0.2
    done
    return 1
}
test_files() { find keploy/test-set-0/tests -maxdepth 1 -name '*.yaml' -size +0c 2>/dev/null | wc -l; }
wait_tests() {
    local i
    for i in $(seq 1 600); do
        [ "$(test_files)" -ge "$1" ] && return 0
        pid_running "$2" || return 1
        sleep 0.2
    done
    return 1
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
    [ -s "$WORK/docker.pid" ] && kill "$(cat "$WORK/docker.pid")" 2>/dev/null
}
trap cleanup EXIT

for p in "$APP" "$UP"; do
    if listening "$p"; then echo "::error::port $p is in use"; exit 1; fi
done
# The slow daemon, in a session of its own (setsid -f), as a daemon runs.
setsid -f python3 "$SRC/docker-slow.py" "$WORK/docker.sock" "$LIST_DELAY" "$WORK" 2>"$WORK/docker.log"
for _ in $(seq 1 50); do [ -S "$WORK/docker.sock" ] && break; sleep 0.1; done
[ -S "$WORK/docker.sock" ] || { echo "::error::the slow Docker daemon did not start"; cat "$WORK/docker.log"; exit 1; }
DOCKER=("DOCKER_HOST=unix://$WORK/docker.sock")
start_upstream() {
    "$WORK/daemonapp" upstream "127.0.0.1:$UP" "$1" >upstream.log 2>&1 &
    UP_PID=$!
    wait_listen "$UP" "$UP_PID" || { echo "::error::the upstream did not start"; exit 1; }
}
stop_upstream() { kill "$UP_PID" 2>/dev/null; wait "$UP_PID" 2>/dev/null; UP_PID=""; }
APPCMD="./daemonapp app 127.0.0.1:$APP 127.0.0.1:$UP"

echo "== 1. record, the daemon answering its list after ${LIST_DELAY} s =="
start_upstream up-record.log
rm -f "$WORK/listed" "$WORK/asked"
sudo env PATH="$PATH" "${SUDO_ENV[@]}" "${DOCKER[@]}" KEPLOY_PROVEN=1 "$RECORD_BIN" record -c "$APPCMD" \
    --generateGithubActions=false --disable-tele >record.log 2>&1 </dev/null &
REC_PID=$!
wait_listen "$APP" "$REC_PID" || { echo "::error::the app did not start under keploy record"; tail -30 record.log; exit 1; }
if [ -e "$WORK/listed" ]; then
    fail "record: the app started only after the Docker daemon had listed its containers"
fi
curl -s --max-time 120 "http://127.0.0.1:$APP/go"
echo
wait_tests 1 "$REC_PID" || fail "keploy record wrote $(test_files) test cases, want 1"
# The calls to the upstream, on this host, may have been a container's: their
# mocks are filed once the daemon has listed. Stop after that.
for _ in $(seq 1 $(( (LIST_DELAY + 60) * 5 ))); do [ -e "$WORK/listed" ] && break; sleep 0.2; done
sleep 2
stop_pid "$REC_PID"
REC_PID=""
stop_upstream
own
verify_ran 1 record.log || FAIL=$((FAIL + 1))
[ -e "$WORK/asked" ] || fail "record: keploy never asked the Docker daemon for its list (DOCKER_HOST did not reach the agent)"
if grep -aq "before the app starts" record.log; then
    fail "record: keploy waited for the Docker daemon before the app started"
fi
for path in /from-app /from-child /from-daemon; do
    grep -q "$path" keploy/test-set-0/mocks.yaml 2>/dev/null || fail "record: the call to $path was not recorded"
done

echo "== 2. replay, the daemon as slow =="
sudo env PATH="$PATH" "${SUDO_ENV[@]}" "${DOCKER[@]}" KEPLOY_PROVEN=1 "$REPLAY_BIN" test -c "$APPCMD" \
    --delay 2 --generateGithubActions=false --disable-tele >replay.log 2>&1 </dev/null
RC=$?
verify_ran 1 replay.log || FAIL=$((FAIL + 1))
own
report="$(ls -1d keploy/reports/test-run-* 2>/dev/null | sort -V | tail -n1)/test-set-0-report.yaml"
status="$(grep -m1 '^status:' "$report" 2>/dev/null | awk '{print $2}')"
if [ "$RC" -ne 0 ] || [ "$status" != PASSED ]; then
    fail "replay: the set must pass (exit $RC, status ${status:-none})"
    tail -40 replay.log
fi

if [ "$FAIL" -ne 0 ]; then
    echo "--- record.log (tail)"; tail -40 record.log
    echo "--- the slow daemon"; tail -20 "$WORK/docker.log"
    exit 1
fi
echo "✅ a Docker daemon slow to list its containers holds up no app that reaches none"
