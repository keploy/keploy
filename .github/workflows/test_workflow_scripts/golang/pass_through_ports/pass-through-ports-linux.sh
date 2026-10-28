#!/usr/bin/env bash
# E2E: keploy record and keploy test take --pass-through-ports (and its
# spelling --passThroughPorts) beside keploy.yml's record.passThroughPorts
# telemetry rules, and each does what it says.
#
# From v3.6.13 (#4423, which made record.passThroughPorts a list of telemetry
# rules) to the fix, `keploy record --pass-through-ports N` stopped at startup:
# the flag was bound to that key, and the config failed to load with
# "'record.passThroughPorts[0]' expected a map or struct, got "uint"".
#
# The app (./app) answers GET /n with its dependency's body (port 9094) and the
# status of a metric it posts to its telemetry service (port 9095). keploy.yml
# says port 9095 is telemetry: record.passThroughPorts, {port: 9095, mode:
# skip}, so it is never recorded and keploy test answers it 200 itself.
#   1, 2. For each spelling: keploy record --<spelling> 9094 starts, records
#      the two GET /n as tests, and records no mock of either port (the flag
#      kept the dependency's port off keploy's proxy; the rule kept the
#      telemetry call out). keploy test --<spelling> 9094, the telemetry
#      service down: the set passes, its two dependency calls reaching the
#      real dependency (the flag), its telemetry calls answered by keploy (the
#      rule).
#   3. Ten pass-through ports, the dependency's the 10th: it is not recorded.
#      Eleven: keploy record refuses to start, naming the dependency's port as
#      the one the kernel's 10-port list cannot hold (before, it was dropped
#      from the list without a word, and its calls recorded).
#   4. bypassRules with a host on the dependency's port: one for another host
#      leaves its call recorded (only port-only rules reach the kernel; before,
#      the rule's port did, and every host's calls on it passed through), one
#      for its host passes its call through in the proxy, in record and test,
#      and one on a mysqlPorts port, where no call shows a host, stops keploy
#      record at start, naming the rule.
#   5. Control, no flag: the dependency's call is recorded as a mock and the
#      telemetry call is not; keploy test, both services down, passes.
#
# Readiness is each port listening, keploy's test files, and keploy's own
# probe of the app (--health-path) while replaying. The script stops only the
# processes it started. Ports: 8094 (app), 9094 (dependency), 9095
# (telemetry). Env: RECORD_BIN, REPLAY_BIN.
set -uo pipefail
RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-$RECORD_BIN}"
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP=8094
DEP=9094
TEL=9095
FAIL=0
fail() {
    echo "::error::$*"
    FAIL=$((FAIL + 1))
}

WORK="$(mktemp -d)"
echo "work dir: $WORK"
(cd "$SRC" && go build -o "$WORK/ptp-app" ./app) || { echo "::error::build failed"; exit 1; }
cd "$WORK" || exit 1
APP_CMD="env DEP_URL=http://127.0.0.1:$DEP TEL_URL=http://127.0.0.1:$TEL ./ptp-app"

listening() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
# pid_running PID: whether PID still runs, by its state in /proc, which shows
# root's processes to this user too (kill -0 fails on a sudo that is alive).
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
newest_run() { ls -1d keploy/reports/test-run-* 2>/dev/null | sort -t- -k3 -n | tail -n1; }
REPORT() { echo "$(newest_run)/test-set-0-report.yaml"; }
set_status() { grep -m1 '^status:' "$(REPORT)" 2>/dev/null | awk '{print $2}'; }

for p in "$APP" "$DEP" "$TEL"; do
    if listening "$p"; then echo "::error::port $p is in use"; exit 1; fi
done

# srv.py dep PORT: GET /n answers {"n":7} and is counted, GET /count says the
# count. srv.py tel PORT: POST /metrics answers 200.
cat >srv.py <<'PY'
import http.server, sys
role, port, hits = sys.argv[1], int(sys.argv[2]), 0
class H(http.server.BaseHTTPRequestHandler):
    def answer(self, body):
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def do_GET(self):
        global hits
        if self.path == "/count":
            return self.answer(str(hits).encode())
        hits += 1
        self.answer(b'{"n":7}')
    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length", 0)))
        self.answer(b"")
    def log_message(self, *a): pass
http.server.HTTPServer(("127.0.0.1", port), H).serve_forever()
PY
DEP_PID=""
TEL_PID=""
REC_PID=""
start_srv() { # start_srv ROLE PORT: sets SRV_PID
    python3 -u srv.py "$1" "$2" >"$1.out" 2>&1 &
    SRV_PID=$!
    wait_listen "$2" "$SRV_PID" || { echo "::error::the $1 service did not start"; cat "$1.out"; exit 1; }
}
stop_srv() { [ -n "$1" ] && kill "$1" 2>/dev/null && wait "$1" 2>/dev/null; }
cleanup() {
    [ -n "$REC_PID" ] && stop_pid "$REC_PID"
    stop_srv "$DEP_PID"
    stop_srv "$TEL_PID"
}
trap cleanup EXIT
dep_hits() { curl -s "http://127.0.0.1:$DEP/count"; }

cat >keploy.yml <<YML
record:
  passThroughPorts:
    - port: $TEL
      mode: skip
YML

# record NAME [FLAG...]: keploy record of two GET /n, both services up.
record() {
    local name="$1"
    shift
    rm -rf keploy
    [ -n "$DEP_PID" ] || { start_srv dep "$DEP"; DEP_PID=$SRV_PID; }
    [ -n "$TEL_PID" ] || { start_srv tel "$TEL"; TEL_PID=$SRV_PID; }
    # shellcheck disable=SC2024 # the log is the runner's file, opened by this shell
    sudo -E env -u KEPLOY_PROVEN PATH="$PATH" "$RECORD_BIN" record -c "$APP_CMD" "$@" \
        --generateGithubActions=false --disable-tele >"record-$name.log" 2>&1 </dev/null &
    REC_PID=$!
    if ! wait_listen "$APP" "$REC_PID"; then
        fail "$name: the app did not start under keploy record $*"
        grep -a -m3 -E 'ERROR|expected a map' "record-$name.log"
        REC_PID=""
        return 1
    fi
    for _ in 1 2; do
        out="$(curl -s "http://127.0.0.1:$APP/n")"
        [ "$out" = '{"dep":{"n":7},"telemetry":"200"}' ] || fail "$name: GET /n answered '$out' while recording"
    done
    wait_tests 2 "$REC_PID" || fail "$name: keploy record wrote $(test_files) test cases, want 2"
    stop_pid "$REC_PID"
    REC_PID=""
    own
}
# mocked TEXT: whether a recorded mock holds TEXT: the dependency's answer
# ('"n":7') or the telemetry call's path ('/metrics').
mocked() { grep -qsF "$1" keploy/test-set-0/mocks.yaml; }
show_mocks() { grep -asE '^kind:|url:' keploy/test-set-0/mocks.yaml | head -20; }

# replay NAME [FLAG...]: keploy test of the recording; RC is its exit code.
replay() {
    local name="$1" before
    shift
    before="$(newest_run)"
    # shellcheck disable=SC2024 # the log is the runner's file, opened by this shell
    sudo -E env -u KEPLOY_PROVEN PATH="$PATH" "$REPLAY_BIN" test -c "$APP_CMD" "$@" \
        --delay 1 --health-path /healthz --generateGithubActions=false --disable-tele >"test-$name.log" 2>&1 </dev/null
    RC=$?
    own
    if [ "$(newest_run)" = "$before" ] || [ ! -f "$(REPORT)" ]; then
        fail "$name: keploy test wrote no report (exit $RC)"
        grep -a -m3 -E 'ERROR|expected a map' "test-$name.log"
        return 1
    fi
    if [ "$RC" -ne 0 ] || [ "$(set_status)" != PASSED ]; then
        fail "$name: the set must pass (exit $RC, status $(set_status))"
    fi
}

for spelling in pass-through-ports passThroughPorts; do
    if record "$spelling" "--$spelling" "$DEP"; then
        ! mocked '"n":7' || { fail "$spelling: keploy record --$spelling $DEP recorded the dependency's call"; show_mocks; }
        ! mocked /metrics || { fail "$spelling: keploy record recorded the telemetry call keploy.yml's rule skips"; show_mocks; }
        stop_srv "$TEL_PID"
        TEL_PID=""
        before="$(dep_hits)"
        replay "$spelling" "--$spelling" "$DEP"
        after="$(dep_hits)"
        [ "$((after - before))" -eq 2 ] ||
            fail "$spelling: keploy test --$spelling $DEP sent $((after - before)) calls to the real dependency, want 2"
    fi
done

# The kernel's pass-through list holds 10 ports. Ten port-only rules fit: the
# dependency's port, the 10th, is passed through and not recorded.
NINE=9001,9002,9003,9004,9005,9006,9007,9008,9009
if record ten --pass-through-ports "$NINE,$DEP"; then
    ! mocked '"n":7' || { fail "ten: keploy record recorded the dependency's call, the 10th pass-through port"; show_mocks; }
fi

# Eleven are refused at start, naming the one that does not fit. Before, the
# 11th was left out of the kernel's list without a word, and its calls were
# recorded.
rm -rf keploy
# shellcheck disable=SC2024 # the log is the runner's file, opened by this shell
sudo -E env -u KEPLOY_PROVEN PATH="$PATH" "$RECORD_BIN" record -c "$APP_CMD" --pass-through-ports "$NINE,9010,$DEP" \
    --generateGithubActions=false --disable-tele >record-eleven.log 2>&1 </dev/null &
REC_PID=$!
if wait_listen "$APP" "$REC_PID"; then
    fail "eleven: keploy record started with 11 pass-through ports, one more than the kernel holds"
    for _ in 1 2; do curl -s "http://127.0.0.1:$APP/n" >/dev/null; done
    wait_tests 2 "$REC_PID" || echo "eleven: keploy record wrote $(test_files) test cases"
    stop_pid "$REC_PID"
    own
    if mocked '"n":7'; then
        fail "eleven: the 11th pass-through port's calls were recorded"
        show_mocks
    fi
else
    wait "$REC_PID"
    rc=$?
    [ "$rc" -ne 0 ] || fail "eleven: keploy record refused the ports but exited 0"
    grep -a "do not fit" record-eleven.log | grep -aqF "instead: $DEP." ||
        { fail "eleven: keploy record stopped without naming port $DEP as the one that does not fit"; tail -n 5 record-eleven.log; }
fi
REC_PID=""
own

# bypassRules with a host. A rule for another host on the dependency's port
# leaves the dependency's call recorded: only port-only rules go to the
# kernel. Before, every rule's port did, and the kernel passed every host's
# calls on it through.
BASE_YML="$(cat keploy.yml)"
cat >keploy.yml <<YML
$BASE_YML
bypassRules:
  - host: elsewhere.example
    port: $DEP
YML
if record otherhost; then
    mocked '"n":7' || { fail "otherhost: a bypassRule for another host on port $DEP passed the dependency's call through"; show_mocks; }
fi
# A rule for the dependency's host passes its call through in the proxy (its
# HTTP Host), in record and in test: keploy test, the telemetry service down,
# sends both calls to the real dependency.
cat >keploy.yml <<YML
$BASE_YML
bypassRules:
  - host: '^127\.0\.0\.1:'
    port: $DEP
YML
if record samehost; then
    ! mocked '"n":7' || { fail "samehost: keploy record recorded the call a bypassRule names by its host"; show_mocks; }
    stop_srv "$TEL_PID"
    TEL_PID=""
    before="$(dep_hits)"
    replay samehost
    after="$(dep_hits)"
    [ "$((after - before))" -eq 2 ] ||
        fail "samehost: keploy test sent $((after - before)) calls to the real dependency, want 2 (the rule passes them through)"
fi
# A rule with a host on a mysqlPorts port can match no call (MySQL shows no
# host), so keploy record refuses it at start, naming it.
cat >keploy.yml <<YML
$BASE_YML
mysqlPorts: [$DEP]
bypassRules:
  - host: db.internal
    port: $DEP
YML
rm -rf keploy
# shellcheck disable=SC2024 # the log is the runner's file, opened by this shell
sudo -E env -u KEPLOY_PROVEN PATH="$PATH" "$RECORD_BIN" record -c "$APP_CMD" \
    --generateGithubActions=false --disable-tele >record-mysqlhost.log 2>&1 </dev/null &
REC_PID=$!
if wait_listen "$APP" "$REC_PID"; then
    fail "mysqlhost: keploy record started with a host bypassRule on a mysqlPorts port"
    stop_pid "$REC_PID"
else
    wait "$REC_PID"
    rc=$?
    [ "$rc" -ne 0 ] || fail "mysqlhost: keploy record refused the rule but exited 0"
    grep -aqF "{host: \"db.internal\", path: \"\", port: $DEP}" record-mysqlhost.log ||
        { fail "mysqlhost: keploy record stopped without naming the rule it cannot apply"; tail -n 5 record-mysqlhost.log; }
fi
REC_PID=""
own
printf '%s\n' "$BASE_YML" >keploy.yml

# Control: without the flag, the dependency's call is mocked, and the replay
# needs neither service.
if record control; then
    mocked '"n":7' || { fail "control: keploy record did not record the dependency's call"; show_mocks; }
    ! mocked /metrics || { fail "control: keploy record recorded the telemetry call keploy.yml's rule skips"; show_mocks; }
    stop_srv "$DEP_PID"
    DEP_PID=""
    stop_srv "$TEL_PID"
    TEL_PID=""
    replay control
fi

if [ "$FAIL" -eq 0 ]; then
    echo "pass-through-ports e2e passed"
else
    echo "pass-through-ports e2e FAILED ($FAIL)"
    for f in record-*.log test-*.log; do echo "== $f"; grep -aE 'ERROR|status|PASSED|FAILED' "$f" | tail -5; done
fi
exit "$FAIL"
