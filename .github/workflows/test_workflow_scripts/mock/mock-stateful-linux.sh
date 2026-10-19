#!/usr/bin/env bash
# End-to-end guard for STATEFUL dependency replay in `keploy mock replay`: the
# same request recorded several times (a counter, a created-then-read row) is
# answered with its recorded responses in record order, then the last one again
# -- not the first one forever.
#
# Records a counter dependency under two tests in one process (t1 reads it 3
# times, t2 twice: 1,2,3 then 4,5), over scope protocol v1 (keploy_v1.py).
#
# Under the proven switch (which the flip makes the default) the record keeps
# each test's mocks under its ID, rec.py::t1 the three and rec.py::t2 the two;
# this build, which does not judge each test (keploy enterprise does), refuses
# the replay before it runs the test command.
#
# Without the switch (today's default; the flip deletes these replays with the
# switch), it replays with the dependency DOWN:
#   1. unscoped: one driver reads it six times and must get 1,2,3,4,5,5. This
#      is the pool `keploy mock replay` begins its set with; a duplicate of
#      each matched recording used to make it 1,1,2,2,3,3.
#   2. two CONCURRENT workers, each registered under its own pid and scope: t1
#      must get 1,2,3 and t2 4,5 -- each walks only its own test's recordings,
#      neither advances the other's. The per-worker wrap used to drop the
#      cursor entirely (1,1,1), and a shared cursor would interleave them. The
#      pid travels on the scope API as it was, as today's parallel workers send
#      it: v1 has no pid (one test process per run until release 2).
#   3. t1 run twice by ONE worker process: the second run is a new scope (same
#      pid), so its sequence starts over at 1,2,3 instead of saturating.
#   4. ONE keep-alive connection reused for t1 and then t2 in one process:
#      what it sees follows the test to t2 (4,5), not the scope it was opened
#      in. Twice: with the worker's pid on each call, which takes the
#      per-worker path (SetWorkerScope, and scopedMockDb resolving the view at
#      each read; a connection that kept the view it opened with was served
#      3,3 in t2), and with no pid (the one pid-less scope, which the v1
#      wire's tests narrow under the switch).
#   5. a mock a test consumed is not served again after that test's end, on
#      the worker's pid (the pid path only). With consume-once mocks
#      (KEPLOY_STRICT_MOCK_WINDOW=1, which makes HTTP mocks per-test), t1
#      calls /c, then a call between tests, a test the recording does not
#      have and t2, which recorded no call, each call /c again: each must
#      miss. A control first: with t1 making no call, the call between tests
#      gets t1's recording, so the misses are t1's consumption. (A pid-less
#      end stages the whole pool again, t1's consumed mock with it: the call
#      between tests is served it. Reported, not guarded here.)
# Without the switch every call sends the worker's pid, as today's workers
# do; step 4's pid-less run says no pid on purpose.
#
# MOCK_LANE_SWITCH (lane_switch.sh) picks the half a run runs; CI runs each in
# a job of its own.
# Uses the PR `build` binary for record and replay (mock_linux.yml).
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true

# This lane's directory: the shared harness (keploy_v1.py).
# shellcheck source-path=SCRIPTDIR
MOCK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-keploy}"
WORK="$(mktemp -d)"
cd "$WORK" || exit 1
PORT=18877
FAIL=0
# A lane that could not load it would skip both halves and pass: fail instead.
# shellcheck source=lane_switch.sh
if ! source "$MOCK_DIR/lane_switch.sh" || ! declare -F half >/dev/null; then
  echo "FAIL: could not load lane_switch.sh from $MOCK_DIR"
  exit 1
fi
cp "$MOCK_DIR/keploy_v1.py" keploy_v1.py || { echo "FAIL: no keploy_v1.py beside this lane, in $MOCK_DIR"; exit 1; }

cat > dep.py <<'PY'
import http.server, socketserver, sys
PORT = int(sys.argv[1]); n = {"c": 0}
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        n["c"] += 1
        body = str(n["c"]).encode()
        self.send_response(200); self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def log_message(self, *a): pass
socketserver.TCPServer.allow_reuse_address = True
with socketserver.TCPServer(("127.0.0.1", PORT), H) as s:
    print("ready", flush=True)
    s.serve_forever()
PY

# Shared by the drivers: the counter, read k times over one connection each.
cat > kp.py <<'PY'
import os, sys, urllib.request
import keploy_v1 as k
PORT = sys.argv[1]
assert k.TOKEN, "keploy exported KEPLOY_MOCK_AGENT but no KEPLOY_MOCK_AGENT_TOKEN into the wrapped command"
def read(n):
    return ",".join(urllib.request.urlopen("http://127.0.0.1:%s/counter" % PORT).read().decode() for _ in range(n))
PY

# Record: one test process, two tests in turn -> t1: 1,2,3  t2: 4,5. Each
# call is the wire's own: v1 under the switch, and without it the scope API
# as it was, with this worker's pid (keploy_v1.py).
cat > rec.py <<'PY'
import os, sys
import keploy_v1 as k
from kp import read
mode = k.claim(".", os.getcwd())
print("CLAIM=" + mode, flush=True)
for name, n, want in (("t1", 3, "1,2,3"), ("t2", 2, "4,5")):
    assert k.begin(name, "rec.py") is None
    assert read(n) == want
    assert k.end(name, "passed", file="rec.py") is None
print("RECORD_OK", flush=True)
PY

# Replay 1: no scope at all -- the whole pool.
cat > unscoped.py <<'PY'
from kp import read
print("UNSCOPED=" + read(6), flush=True)
PY

# Replay 2/3, without the switch: one worker process under its own pid.
# argv: PORT name reads runs [mine other]: runs the test `runs` times in this
# process, each a new scope, and prints <name>=<served> for the first run and
# <name>again=<served> for the second. With mine and other (pipe ends shared
# with a concurrent worker), its first scope stays open until both workers
# have begun theirs, so the two overlap whatever the machine's load.
cat > worker.py <<'PY'
import os, sys
import keploy_v1 as k
from kp import read
name, n, runs = sys.argv[2], int(sys.argv[3]), int(sys.argv[4])
pipes = (int(sys.argv[5]), int(sys.argv[6])) if len(sys.argv) > 6 else None
mode = k.claim(".", os.getcwd())
assert mode == k.LEGACY, "per-pid workers are the scope API without the proven switch, and the agent answered the claim: " + mode
for i in range(runs):
    label = name if i == 0 else name + "again"
    assert k.begin(name, "worker.py", pid=os.getpid()) is None
    if pipes and i == 0:
        os.write(pipes[0], b"x")
        assert os.read(pipes[1], 1) == b"x", "the other worker exited before it began its scope"
    try:
        print("%s=%s" % (label, read(n)), flush=True)
    except Exception as e:
        print("%s=ERROR:%s" % (label, e), flush=True)
    k.end(name, "passed", file="worker.py", pid=os.getpid())
PY

cat > run.py <<'PY'
import os, subprocess, sys
PORT = sys.argv[1]
r1, w1 = os.pipe()
r2, w2 = os.pipe()
ps = [subprocess.Popen(["python3", "worker.py", PORT, n, k, "1", str(mine), str(other)], stdout=subprocess.PIPE, pass_fds=(mine, other))
      for n, k, mine, other in (("t1", "3", w1, r2), ("t2", "2", w2, r1))]
for fd in (r1, w1, r2, w2):
    os.close(fd)
for p in ps:
    print(p.communicate()[0].decode().strip(), flush=True)
# One process running t1 twice: same pid, so only the second run being a NEW
# scope can start its sequence over (a fresh process would hide that).
print(subprocess.run(["python3", "worker.py", PORT, "t1", "3", "2"], stdout=subprocess.PIPE).stdout.decode().strip().replace("t1=", "t1first="), flush=True)
PY

# Replay 4: ONE keep-alive connection reused across two tests of one process:
# what the connection sees must follow the test. argv: PORT pid|nopid: pid
# sends this worker's pid on each call (the agent scopes the worker); nopid
# sends none (the agent narrows the one pid-less scope, as it does for the v1
# wire's tests under the switch). Prints KA_<argv[2]>_t1=, _t2= and _sockets=.
# The recording was made with urllib, so each request carries the header keys
# urllib sends (matching requires them); Connection: keep-alive keeps the one
# socket open, and the socket's address proves it was never reopened -- a new
# connection opened in t2 would pass this step without the fix.
cat > keepalive.py <<'PY'
import http.client, os, sys
import keploy_v1 as k
conn = http.client.HTTPConnection("127.0.0.1", int(sys.argv[1]))
socks = set()
def read(n):
    out = []
    for _ in range(n):
        conn.request("GET", "/counter", headers={"User-Agent": "Python-urllib/3", "Connection": "keep-alive"})
        out.append(conn.getresponse().read().decode())
        # http.client drops its socket when the peer will close after the
        # response; a reconnect per request would otherwise hide here.
        assert conn.sock is not None, "the connection was closed after a response"
        socks.add(conn.sock.getsockname())
    return ",".join(out)
how = sys.argv[2]
mode = k.claim(".", os.getcwd())
assert mode == k.LEGACY, "this replay is the scope API without the proven switch, and the agent answered the claim: " + mode
# pid: the worker's own (keploy_v1's default); nopid: none, the pid-less scope.
pid = k.WORKER if how == "pid" else None
assert k.begin("t1", "rec.py", pid=pid) is None
a = read(3)
assert k.end("t1", "passed", file="rec.py", pid=pid) is None, "keploy failed t1"
assert k.begin("t2", "rec.py", pid=pid) is None
b = read(2)
assert k.end("t2", "passed", file="rec.py", pid=pid) is None, "keploy failed t2"
print("KA_%s_t1=%s" % (how, a), flush=True)
print("KA_%s_t2=%s" % (how, b), flush=True)
print("KA_%s_sockets=%d" % (how, len(socks)), flush=True)
PY

# Step 5: argv PORT record|replay|control. record: t1 reads the counter
# once, t2 makes no call. replay: t1 reads it, then a call between tests, a
# test t3 the recording does not have, and t2 each read it again. control:
# t1 makes no call, then a call between tests reads it, which t1's
# unconsumed recording can answer. Prints ONCE_<who>= what each got, MISS
# when nothing answered.
cat > once.py <<'PY'
import os, sys, urllib.request
import keploy_v1 as k
PORT, phase = sys.argv[1], sys.argv[2]
mode = k.claim(".", os.getcwd())
print("CLAIM=" + mode, flush=True)
def get():
    try:
        return urllib.request.urlopen("http://127.0.0.1:%s/counter" % PORT).read().decode()
    except Exception:
        return "MISS"
def test(name, call):
    assert k.begin(name, "once.py") is None, "keploy told this run to skip " + name
    got = get() if call else None
    reason = k.end(name, "passed", file="once.py")
    if reason:
        print("ONCE_%s_FAILED_BY_KEPLOY=%s" % (name, reason.replace("\n", " | ")), flush=True)
    return got
if phase == "record":
    assert test("t1", True) == "1"
    test("t2", False)
    print("ONCE_RECORD_OK", flush=True)
elif phase == "control":
    test("t1", False)
    print("ONCE_control_between=" + get(), flush=True)
else:
    print("ONCE_t1=" + test("t1", True), flush=True)
    print("ONCE_between=" + get(), flush=True)
    print("ONCE_t3=" + test("t3", True), flush=True)
    print("ONCE_t2=" + test("t2", True), flush=True)
PY

# mapped_counts FILE: "<test id>=<mocks mapped>" per test, sorted.
mapped_counts() {
  python3 - "$1" <<'PY'
import re, sys
try:
    t = open(sys.argv[1]).read()
except OSError:
    t = ""
out = []
for block in re.split(r"\n(?=\s*- id: )", t):
    m = re.search(r"- id: (\S+)", block)
    if m:
        out.append("%s=%d" % (m.group(1), len(re.findall(r"- name: (\S+)", block))))
print(" ".join(sorted(out)))
PY
}

# start_dep: a new counter (from 1), once it says it listens. stop_dep stops
# it by its pid and waits for it to go.
DP=""
trap '[ -z "$DP" ] || kill "$DP" 2>/dev/null' EXIT
start_dep() {
  : > dep.log
  python3 dep.py "$PORT" > dep.log 2>&1 &
  DP=$!
  until grep -qx ready dep.log 2>/dev/null; do
    kill -0 "$DP" 2>/dev/null || { echo "FAIL: the dependency exited before it listened"; cat dep.log; exit 1; }
    sleep 0.1
  done
}
stop_dep() { kill "$DP" 2>/dev/null; wait "$DP" 2>/dev/null; DP=""; }
mocks_of() {
  sed 's/\x1b\[[0-9;]*m//g' "$1" | grep -oE '"mocks": *[0-9]+' | grep -oE '[0-9]+' | tail -1
}

has() { grep -qE "(^|[^[:alnum:]])$1([^[:alnum:],]|\$)" "$2"; }

if half on; then
echo "== 1. under the proven switch: record a counter under two tests (t1: 1,2,3  t2: 4,5) =="
start_dep
sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$RECORD_BIN" mock record -c "python3 rec.py $PORT" --name s-proven --disable-tele 2>&1 | tee rec-proven.log
RC=${PIPESTATUS[0]}
stop_dep
[ "$RC" -eq 0 ] || { echo "FAIL: under the switch the record should pass (exit 0), got $RC"; FAIL=1; }
grep -qx "CLAIM=v1" rec-proven.log || { echo "FAIL: under the switch the agent did not take the claim on scope protocol v1"; FAIL=1; }
grep -q "RECORD_OK" rec-proven.log || { echo "FAIL: the record driver did not complete under the switch"; FAIL=1; }
[ "$(mocks_of rec-proven.log)" = "5" ] || { echo "FAIL: under the switch, expected 5 recorded mocks, got $(mocks_of rec-proven.log)"; FAIL=1; }
python3 - keploy/s-proven/mappings.yaml <<'PY' || FAIL=1
import re, sys
t = open(sys.argv[1]).read()
got = {}
for block in re.split(r"\n(?=\s*- id: )", t):
    m = re.search(r"- id: (\S+)", block)
    if m:
        got[m.group(1)] = len(re.findall(r"- name: (\S+)", block))
if got != {"rec.py::t1": 3, "rec.py::t2": 2}:
    sys.exit("FAIL: under the switch want rec.py::t1 mapped to 3 mocks and rec.py::t2 to 2, got %s" % got)
PY
rm -f ran.txt
sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$REPLAY_BIN" mock replay -c "sh -c 'echo ran > ran.txt; python3 unscoped.py $PORT'" --name s-proven --disable-tele 2>&1 | tee rep-proven.log
RC=${PIPESTATUS[0]}
[ "$RC" -eq 1 ] && grep -q "needs a keploy build that judges each test" rep-proven.log ||
  { echo "FAIL: under the switch this build should refuse the replay (exit 1, saying it needs a build that judges each test), got $RC"; FAIL=1; }
[ -e ran.txt ] && { echo "FAIL: under the switch the refused replay ran the test command"; FAIL=1; }

echo "== 5. under the proven switch: record t1 calling /c once and t2 calling nothing =="
# The replay that checks t1's consumed mock is not served again after t1's end
# needs a build that judges each test (keploy enterprise; this one refuses it,
# step 1), and no lane replays this shape yet. Here, only its recording.
start_dep
sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$RECORD_BIN" mock record -c "python3 once.py $PORT record" --name once-proven --disable-tele 2>&1 | tee rec-once-proven.log
RC=${PIPESTATUS[0]}
stop_dep
[ "$RC" -eq 0 ] && grep -q "ONCE_RECORD_OK" rec-once-proven.log || { echo "FAIL: under the switch the once record should pass (exit 0), got $RC"; FAIL=1; }
GOT=$(mapped_counts keploy/once-proven/mappings.yaml)
[ "$GOT" = "once.py::t1=1 once.py::t2=0" ] || { echo "FAIL: under the switch want once.py::t1 mapped to 1 mock and once.py::t2 to none, got '$GOT'"; FAIL=1; }
fi

if half off; then
echo "== 1'. without the switch: record the same, each call with the worker's pid =="
start_dep
sudo -E env PATH="$PATH" KEPLOY_PROVEN= "$RECORD_BIN" mock record -c "python3 rec.py $PORT" --name s --disable-tele 2>&1 | tee rec.log
grep -qx "CLAIM=legacy" rec.log || { echo "FAIL: without the switch the record driver did not fall back to the scope API as it was"; FAIL=1; }
grep -q "RECORD_OK" rec.log || { echo "FAIL: record driver did not complete"; FAIL=1; }
[ "$(mocks_of rec.log)" = "5" ] || { echo "FAIL: expected 5 recorded mocks, got $(mocks_of rec.log)"; FAIL=1; }
stop_dep

echo "== 2. without the switch: replay unscoped, dependency DOWN: record order, then saturate =="
sudo -E env PATH="$PATH" KEPLOY_PROVEN= "$REPLAY_BIN" mock replay -c "python3 unscoped.py $PORT" --name s --disable-tele 2>&1 | tee rep-unscoped.log
has "UNSCOPED=1,2,3,4,5,5" rep-unscoped.log || { echo "FAIL: unscoped replay did not serve 1,2,3,4,5,5 (got: $(grep -o 'UNSCOPED=[^ ]*' rep-unscoped.log))"; FAIL=1; }

echo "== 3. without the switch: replay two concurrent pid-scoped workers, then a re-run, dependency DOWN =="
sudo -E env PATH="$PATH" KEPLOY_PROVEN= "$REPLAY_BIN" mock replay -c "python3 run.py $PORT" --name s --disable-tele 2>&1 | tee rep-workers.log
has "t1=1,2,3"      rep-workers.log || { echo "FAIL: worker t1 was not served its own 1,2,3 (got: $(grep -o 't1=[0-9,A-Z:a-z]*' rep-workers.log | head -1))"; FAIL=1; }
has "t2=4,5"        rep-workers.log || { echo "FAIL: worker t2 was not served its own 4,5 (got: $(grep -o 't2=[0-9,A-Z:a-z]*' rep-workers.log | head -1))"; FAIL=1; }
has "t1first=1,2,3" rep-workers.log || { echo "FAIL: t1 in a new worker process was not served 1,2,3 (got: $(grep -o 't1first=[0-9,A-Z:a-z]*' rep-workers.log | head -1))"; FAIL=1; }
has "t1again=1,2,3" rep-workers.log || { echo "FAIL: a re-run of t1 in the same process did not start its sequence over (got: $(grep -o 't1again=[0-9,A-Z:a-z]*' rep-workers.log | head -1))"; FAIL=1; }

# 4a with the worker's pid, 4b with none: the same connection reused across
# the two tests must be served each test's own recordings.
for how in pid nopid; do
  echo "== 4($how). without the switch: one keep-alive connection across two tests, dependency DOWN =="
  sudo -E env PATH="$PATH" KEPLOY_PROVEN= "$REPLAY_BIN" mock replay -c "python3 keepalive.py $PORT $how" --name s --disable-tele 2>&1 | tee "rep-keepalive-$how.log"
  has "KA_${how}_t1=1,2,3" "rep-keepalive-$how.log" || { echo "FAIL: ($how) t1 on the keep-alive connection was not served its own 1,2,3 (got: $(grep -o "KA_${how}_t1=[0-9,A-Z:a-z]*" "rep-keepalive-$how.log" | head -1))"; FAIL=1; }
  has "KA_${how}_t2=4,5" "rep-keepalive-$how.log" || { echo "FAIL: ($how) t2 on the connection t1 opened was not served its own 4,5 (got: $(grep -o "KA_${how}_t2=[0-9,A-Z:a-z]*" "rep-keepalive-$how.log" | head -1))"; FAIL=1; }
  has "KA_${how}_sockets=1" "rep-keepalive-$how.log" || { echo "FAIL: ($how) the client reconnected, so step 4 did not test one connection across two tests ($(grep -o "KA_${how}_sockets=[0-9]*" "rep-keepalive-$how.log" | head -1))"; FAIL=1; }
done

echo "== 5. without the switch: a mock t1 consumed is not served again after t1's end (the pid path) =="
# On the worker's pid, as every call here sends: an end clears the worker's
# scope. (A pid-less end stages the whole pool again, t1's consumed mock with
# it, and the call between tests is served it: reported, not guarded here.)
start_dep
sudo -E env PATH="$PATH" KEPLOY_PROVEN= "$RECORD_BIN" mock record -c "python3 once.py $PORT record" --name once --disable-tele 2>&1 | tee rec-once.log
RC=${PIPESTATUS[0]}
stop_dep
[ "$RC" -eq 0 ] && grep -q "ONCE_RECORD_OK" rec-once.log || { echo "FAIL: the once record should pass (exit 0), got $RC"; FAIL=1; }
GOT=$(mapped_counts keploy/once/mappings.yaml)
[ "$GOT" = "t1=1 t2=0" ] || { echo "FAIL: want t1 mapped to 1 mock and t2 to none, got '$GOT'"; FAIL=1; }
# Consume-once mocks: KEPLOY_STRICT_MOCK_WINDOW=1 makes HTTP mocks per-test,
# where an HTTP mock is otherwise reusable and saturates by design.
# The control first: with t1 making no call, a call between tests is answered
# by t1's unconsumed recording, so a miss below is t1's consumption, not a
# call between tests that nothing could ever answer.
sudo -E env PATH="$PATH" KEPLOY_PROVEN= KEPLOY_STRICT_MOCK_WINDOW=1 "$REPLAY_BIN" mock replay -c "python3 once.py $PORT control" --name once --disable-tele 2>&1 | tee rep-once-control.log
has "ONCE_control_between=1" rep-once-control.log ||
  { echo "FAIL: control: with t1 making no call, the call between tests got '$(grep -o 'ONCE_control_between=[^ ]*' rep-once-control.log | head -1)', want t1's recording (1)"; FAIL=1; }
sudo -E env PATH="$PATH" KEPLOY_PROVEN= KEPLOY_STRICT_MOCK_WINDOW=1 "$REPLAY_BIN" mock replay -c "python3 once.py $PORT replay" --name once --disable-tele 2>&1 | tee rep-once.log
RC=${PIPESTATUS[0]}
echo "the once replay exited $RC (without --strict, the test command's code)"
has "ONCE_t1=1" rep-once.log || { echo "FAIL: t1 was not served its own recording (got: $(grep -o 'ONCE_t1=[^ ]*' rep-once.log | head -1))"; FAIL=1; }
for who in between t3 t2; do
  case $who in
    between) what="made between tests" ;;
    t3) what="in t3, a test the recording does not have" ;;
    t2) what="in t2, which recorded no call" ;;
  esac
  has "ONCE_${who}=MISS" rep-once.log ||
    { echo "FAIL: after t1 consumed the recording's one call, the call $what got '$(grep -o "ONCE_${who}=[^ ]*" rep-once.log | head -1)', want a miss"; FAIL=1; }
done
# keploy saw the three misses; the scope API without the switch never fails a
# test at its end, so no end says it did.
grep -qx 'missed: 3' keploy/once/last-replay.yaml || { echo "FAIL: the once replay's receipt does not count the 3 misses: $(grep -E '^(missed|consumed|loaded):' keploy/once/last-replay.yaml | tr '\n' ' ')"; FAIL=1; }
grep -q "_FAILED_BY_KEPLOY=" rep-once.log && { echo "FAIL: an end answered fail on the scope API without the switch: $(grep -o 'ONCE_[a-z0-9]*_FAILED_BY_KEPLOY=.*' rep-once.log | head -1)"; FAIL=1; }
fi

[ "$FAIL" = 0 ] && echo "MOCK STATEFUL E2E: PASSED" || echo "MOCK STATEFUL E2E: FAILED"
exit $FAIL
