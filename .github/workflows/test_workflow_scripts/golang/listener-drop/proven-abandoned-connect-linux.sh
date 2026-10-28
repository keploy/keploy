#!/usr/bin/env bash
# E2E: a connect the application abandons mid-handshake, inside a test, leaves
# that test judged under the proven switch: in an SDK-driven keploy mock record
# (each test in its window on keploy's scope API) its window carries no capture
# loss, and in keploy test every test is judged.
#
# A process killed in a connect (go test's -timeout, on a host loaded enough
# that the handshake does not finish inside connect()) leaves keploy's proxy
# listener a half-open connection whose SYN-ACK only the gone client's reset
# meets: the kernel adds one to the listener's sk_drops, and none to its
# network namespace's TcpExtListenDrops, as no connection was lost. keploy
# charges a test only for drops both counts saw (the smaller rise), so this
# one is charged to nothing.
#
# The app's /abandon calls the dependency, then makes that state on purpose
# (app/main.go): the proxy's SYN-ACKs held by an iptables rule, a child
# connecting to the dependency (redirected to the proxy), killed and reaped
# mid-handshake, the rule removed; it answers once the listener counted the reset. Every
# step waits on what it needs, so the load the state stands for is not
# needed. The app logs the listener's drops and the namespace's ListenDrops
# before and after: the lane requires the first to rise and the second not
# to, so a run where the drop did not happen proves nothing and fails.
#
#   1. keploy record of /ping, /abandon, /after: three test cases written,
#      /abandon's with its answer. keploy record opens no test windows, so it
#      charges no listener drop on any rule: this step makes the recording
#      step 3 replays, and checks nothing of the drop's charging.
#   2. keploy mock record of the app as a test command (-harness) that marks
#      each test's window on keploy's scope API, as keploy's SDKs do: TestPing,
#      TestAbandon (a call to the dependency, then the abandoned connect),
#      TestAfter. Every test passes, and the window keploy closed for each
#      (GET /agent/scope/windows, read by the harness into windows.json)
#      carries no evidence. Charged on the listener's count alone, TestAbandon's
#      window carried listener_dropped; charged as a lost connection (keploy
#      before this lane), it was unread and its end answered an error.
#   3. keploy test of step 1's recording, the dependency down: the set passes,
#      every test (and /abandon's, its drop made again) is judged and passes.
#      Charged as a lost connection (keploy before this lane), the drop left
#      /abandon's consumed mocks unread and the set not judged (INTERNAL_ERR).
#
# Env: RECORD_BIN, REPLAY_BIN. Ports: 8080 (app), 9099 (the dependency), 16789
# (keploy's proxy). Runs as a user with passwordless sudo; needs iptables, ss
# and python3.
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-$RECORD_BIN}"
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP=8080
DEP=9099
PROXY=16789
FAIL=0
fail() {
    echo "::error::$*"
    FAIL=$((FAIL + 1))
}

for t in iptables ss python3; do
    command -v "$t" >/dev/null 2>&1 || sudo -n sh -c "command -v $t" >/dev/null 2>&1 ||
        { echo "::error::$t is needed (the app holds SYN-ACKs with it or reads the listener's drops, or the lane reads the windows)"; exit 1; }
done

WORK="$(mktemp -d)"
echo "work dir: $WORK"
(cd "$SRC" && go build -o "$WORK/ld-app" ./app) || { echo "::error::build failed"; exit 1; }
cd "$WORK" || exit 1

listening() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
# pid_running PID: whether PID still runs, by its state in /proc (kill -0 from
# this user fails on a sudo that is alive; see proven-fire-and-forget-linux.sh).
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
wait_tests() {
    until [ "$(test_files)" -ge "$1" ]; do
        pid_running "$2" || return 1
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

DEP_PID=""
REC_PID=""
cleanup() {
    [ -n "$REC_PID" ] && stop_pid "$REC_PID"
    [ -n "$DEP_PID" ] && kill "$DEP_PID" 2>/dev/null
    # The app removes its rule as it answers; one left by an app that died
    # mid-request goes here.
    while sudo iptables -w -D OUTPUT -p tcp --sport "$PROXY" --tcp-flags SYN,ACK SYN,ACK -j DROP 2>/dev/null; do :; done
}
trap cleanup EXIT

for p in "$APP" "$DEP" "$PROXY"; do
    if listening "$p"; then echo "::error::port $p is in use"; exit 1; fi
done

# dropped_alone <log> <step>: the app's line for the abandoned connect says
# the listener counted a drop and the namespace's ListenDrops did not move.
dropped_alone() {
    local line lb la nb na
    line="$(grep -m1 'abandoned connect: listener drops' "$1")"
    if [ -z "$line" ]; then
        fail "$2: the app made no abandoned connect: $(grep -m1 'abandoned connect:' "$1")"
        return
    fi
    echo "$2: $line"
    read -r lb la nb na < <(echo "$line" | sed -E 's/.*listener drops ([0-9]+) -> ([0-9]+), namespace ListenDrops ([0-9]+) -> ([0-9]+).*/\1 \2 \3 \4/')
    [ "$la" -gt "$lb" ] || fail "$2: the listener counted no drop ($lb -> $la): the lane proved nothing"
    [ "$na" -eq "$nb" ] || fail "$2: the namespace's ListenDrops rose ($nb -> $na) beside the abandoned connect, so the drop was not the listener's alone"
}
# charged <log>: keploy's lines for a listener drop charged to a test.
charged() { grep -iE "dropped at keploy's proxy listener|listener_dropped|connection attempts at keploy's proxy listener" "$1"; }
# windows_clean <windows.json> <step>: the harness's three tests each closed a
# window, passed, with no evidence: nothing keploy charged them.
windows_clean() {
    python3 - "$1" "$2" <<'PY2'
import json, sys
path, step = sys.argv[1], sys.argv[2]
try:
    windows = json.load(open(path)) or []
except (OSError, ValueError) as e:
    print("::error::%s: the harness wrote no windows (%s)" % (step, e))
    sys.exit(1)
bad = 0
def fail(msg):
    global bad
    print("::error::%s: %s" % (step, msg))
    bad += 1
# A test's window, not one of the rows the agent keeps beside them: an app's
# start, a suite, a claim, a refusal, or where a call to a local listener went.
tests = [w for w in windows if not any(w.get(k) for k in ("app", "suite", "claim", "refused", "live", "mocked"))]
for want in ("TestPing", "TestAbandon", "TestAfter"):
    got = [w for w in tests if w.get("name", "").endswith(want)]
    if len(got) != 1:
        fail("%d windows of %s, want 1 (windows: %s)" % (len(got), want, [w.get("name") for w in tests]))
        continue
    w = got[0]
    ev = w.get("evidence")
    print("%s: window %s: outcome %s, evidence %s" % (step, w["name"], w.get("outcome"), json.dumps(ev)))
    if w.get("outcome") != "passed":
        fail("%s's window closed %r, want passed" % (want, w.get("outcome")))
    if ev:
        what = "a listener drop (listener_dropped)" if "listener_dropped" in json.dumps(ev) else \
            "an unread window" if ev.get("unread") else "evidence"
        fail("keploy charged %s %s: %s" % (want, what, ev.get("reason") or json.dumps(ev)))
if len(tests) != 3:
    fail("%d test windows, want 3: %s" % (len(tests), [(w.get("name"), sorted(k for k in w if w[k])) for w in tests]))
sys.exit(1 if bad else 0)
PY2
}

echo "== 1. keploy record: /ping, /abandon, /after =="
mkdir -p rec && cd rec || exit 1
../ld-app -dep "127.0.0.1:$DEP" >dep.log 2>&1 &
DEP_PID=$!
wait_listen "$DEP" "$DEP_PID" || { echo "::error::the dependency did not start"; exit 1; }
sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$RECORD_BIN" record -c "../ld-app" \
    --generateGithubActions=false --disable-tele >record.log 2>&1 </dev/null &
REC_PID=$!
wait_listen "$APP" "$REC_PID" || { echo "::error::the app did not start under keploy record"; tail -30 record.log; exit 1; }
curl -s "http://127.0.0.1:$APP/ping" >/dev/null
abandon_rec="$(curl -s -w ' (%{http_code})' "http://127.0.0.1:$APP/abandon")"
curl -s "http://127.0.0.1:$APP/after" >/dev/null
[ "$abandon_rec" = "abandoned, dep said dep-ok (200)" ] || fail "1: /abandon answered $abandon_rec"
wait_tests 3 "$REC_PID" || fail "1: keploy record wrote $(test_files) test cases, want 3"
stop_pid "$REC_PID"
REC_PID=""
own
dropped_alone record.log 1
grep -lq 'abandoned, dep said dep-ok' keploy/test-set-0/tests/*.yaml 2>/dev/null ||
    fail "1: no test case holds /abandon's answer"

echo "== 2. keploy mock record: TestPing, TestAbandon, TestAfter, each in its window =="
# A directory of its own: keploy test (step 3) replays every set in rec's.
mkdir -p ../scope && cd ../scope || exit 1
sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$RECORD_BIN" mock record \
    -c "../ld-app -harness" --name ld-scope --disable-tele >scope.log 2>&1 </dev/null
RC=$?
own
[ "$RC" -eq 0 ] || { fail "2: keploy mock record exited $RC"; grep -E '^Test|harness:|abandoned connect' scope.log | head -10; tail -20 scope.log; }
dropped_alone scope.log 2
windows_clean windows.json 2 || FAIL=$((FAIL + 1))
cd ../rec || exit 1
kill "$DEP_PID" 2>/dev/null
wait "$DEP_PID" 2>/dev/null
DEP_PID=""

echo "== 3. keploy test of step 1's recording, the dependency down =="
sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$REPLAY_BIN" test -c "../ld-app" \
    --delay 2 --generateGithubActions=false --disable-tele >test.log 2>&1 </dev/null
RC=$?
own
REPORT="$(ls -1d keploy/reports/test-run-* 2>/dev/null | sort -V | tail -n1)/test-set-0-report.yaml"
if [ ! -f "$REPORT" ]; then
    fail "3: keploy test wrote no report (exit $RC)"
    tail -30 test.log
else
    st="$(grep -m1 '^status:' "$REPORT" | awk '{print $2}')"
    ok="$(grep -m1 '^success:' "$REPORT" | awk '{print $2}')"
    bad="$(grep -m1 '^failure:' "$REPORT" | awk '{print $2}')"
    if [ "$RC" -ne 0 ] || [ "$st" != PASSED ] || [ "$ok" != 3 ] || [ "$bad" != 0 ]; then
        fail "3: the set must pass with its 3 tests judged (exit $RC, status $st, success $ok, failure $bad)"
        grep -nE 'test_case_id|status:|failure_reason|reason' "$REPORT" | head -30
    fi
fi
dropped_alone test.log 3
if charged test.log >/dev/null; then
    fail "3: keploy test charged the abandoned connect as a listener drop:"
    charged test.log | head -5
fi

if [ "$FAIL" -ne 0 ]; then
    echo "::error::$FAIL check(s) failed"
    exit 1
fi
echo "all checks passed"
