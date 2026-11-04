#!/usr/bin/env bash
# End-to-end guard for parallel test workers in `keploy mock`, both ways:
#
# Under the proven switch (KEPLOY_PROVEN=1), a REFUSAL check: test processes
# that declare no worker count run one per keploy run, each with one test
# open at a time. So, in `keploy mock record`, over scope protocol v1:
#   - a test process that claims the run while another live one holds it is
#     refused: 422 one_test_process_at_a_time;
#   - a test that begins while another test of the same process is open is
#     refused: 422 one_test_at_a_time;
# each with a message that says what ran at once, and a remedy that names
# running one at a time, and no later release. Each refused process fails, as
# an integration must on any non-2xx answer, so the run does not pass: keploy
# exits non-zero and keeps no recording. A control run, one process running
# the same two tests one after the other, passes and records both.
#
# Without the switch (today's default, which the flip deletes), PER-PID mock
# scoping, which keploy serves only in a run with the suite mark (or an
# app-start mark): two named scopes recorded inside a suite mark (t1->/a,
# t2->/b), then TWO CONCURRENT workers in one replay, inside a suite mark, that BOTH call
# /b, each registered under its own pid: the worker scoped to t2 must be
# SERVED /b, the worker scoped to t1 must MISS it (its scope allows only /a's
# mock). Without per-PID scoping both would be served, so t1's MISS is the
# proof of isolation. The pid travels on the scope API as it was, which is
# how today's parallel workers call it; v1 has no pid. A run recorded without
# the marks refuses a test that begins with a pid (422, naming the test, with
# the marks and a pid-less begin as the two ways out), fails it, and so fails
# the run: there a connection the app opened before the test began would be
# served every test's mocks. A harness that ignores the 422 and runs the test
# anyway is served nothing in it, and the replay fails all the same, with an
# ERROR naming the test, whatever the harness exits.
#
# And, with the marks, an app the tests did not start (an app server beside
# them), whose connection was opened at boot, before any test began, as a
# database pool's is (mock/boot-pool, a Go app): while exactly one test is
# open its calls are narrowed to that test's mocks, as a pid-less begin
# narrows every connection; while two are open keploy cannot tell which test
# they belong to, so it refuses them, naming the app's pid, and serves
# neither test's mocks.
#
# Both ways, the control plane refuses a caller with no token (401) and still
# answers /agent/health without one.
#
# MOCK_LANE_SWITCH (lane_switch.sh) picks the half a run runs; CI runs each in
# a job of its own.
#
# Uses the PR `build` binary for record and replay (mock_linux.yml).
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
# shellcheck source=sudo_env.sh
source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/mock/sudo_env.sh" || exit 1
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true

# This lane's directory: the shared harness (keploy_v1.py).
# shellcheck source-path=SCRIPTDIR
MOCK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-keploy}"
WORK="$(mktemp -d)"
cd "$WORK" || exit 1
PORT=18876
FAIL=0

fail() {
  echo "FAIL: $*"
  FAIL=1
}

# A lane that could not load it would skip both halves and pass: fail instead.
# shellcheck source=lane_switch.sh
if ! source "$MOCK_DIR/lane_switch.sh" || ! declare -F half >/dev/null; then
  echo "FAIL: could not load lane_switch.sh from $MOCK_DIR"
  exit 1
fi
cp "$MOCK_DIR/keploy_v1.py" keploy_v1.py || { echo "FAIL: no keploy_v1.py beside this lane, in $MOCK_DIR"; exit 1; }

cat > dep.py <<'PY'
import http.server, socketserver, sys
PORT = int(sys.argv[1])
class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"  # keeps the boot-pool app's connection open between calls
    def do_GET(self):
        body = {"/a": b"AAA", "/b": b"BBB"}.get(self.path, b"??")
        self.send_response(200); self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def log_message(self, *a): pass
socketserver.TCPServer.allow_reuse_address = True
with socketserver.TCPServer(("127.0.0.1", PORT), H) as s:
    print("ready", flush=True)
    s.serve_forever()
PY

# The control plane's guard, checked by every driver before its first call.
# Without it the whole feature can regress to a no-op with a green pipeline:
# an agent that was handed no token serves everything, and every other
# assertion here still passes. Unconditional: record and replay both run the
# PR build here, so a missing token is the regression, not an older binary.
cat > guard.py <<'PY'
import json, os, urllib.request
import keploy_v1 as k
def check():
    assert k.TOKEN, "keploy exported KEPLOY_MOCK_AGENT but no KEPLOY_MOCK_AGENT_TOKEN into the wrapped command"
    status, raw = k.post("/agent/scope/begin", {"name": "unauthenticated-probe", "canSkip": True}, auth=False)
    assert status == 401, "expected 401 for an unauthenticated scope call, got %d: %r" % (status, raw[:200])
    # Health stays reachable without a token, so callers can still find the agent.
    assert urllib.request.urlopen(k.AGENT + "/agent/health").status == 200
PY

# The suite mark of the scope API as it was (no v1 call: v1 has no pid and
# no suite): mark("begin") before a process's first test and mark("end")
# after its last, under this process's pid.
cat > marks.py <<'PY'
import json, os
import keploy_v1 as k
def mark(edge):
    status, raw = k.post("/agent/scope/" + edge, {"name": "suite", "pid": os.getpid(), "suite": True})
    assert status == 200, "the suite mark's %s answered %d: %r" % (edge, status, raw[:512])
PY

# One test process running t1 (/a) then t2 (/b), each in a scope of its own.
# Each call is the wire's own: v1 under the switch, and without it the scope
# API as it was, with this worker's pid (keploy_v1.py), inside a suite mark
# when argv[2] is "marks".
cat > rec.py <<'PY'
import os, sys, urllib.request
import guard, keploy_v1 as k, marks
PORT = sys.argv[1]
MARKS = sys.argv[2:] == ["marks"]
def call(p): return urllib.request.urlopen("http://127.0.0.1:%s%s" % (PORT, p)).read()
guard.check()
mode = k.claim(".", os.getcwd())
print("CLAIM=" + mode, flush=True)
if MARKS:
    assert mode == k.LEGACY, "the suite mark is the scope API without the proven switch, and the agent answered the claim: " + mode
    marks.mark("begin")
pid = os.getpid() if mode == k.LEGACY else None
for name, path, want in (("t1", "/a", b"AAA"), ("t2", "/b", b"BBB")):
    assert k.begin(name, "rec.py", pid=pid) is None, "keploy told the record to skip " + name
    assert call(path) == want
    reason = k.end(name, "passed", file="rec.py", pid=pid)
    assert reason is None, "keploy failed %s: %s" % (name, reason)
if MARKS:
    marks.mark("end")
print("RECORD_OK", flush=True)
PY

# Without the switch: one replay worker, registered under its OWN pid in the
# scope argv[2], calls /b. Prints <scope>=SERVED:<body> or <scope>=MISS. Both
# workers hold their scopes open until both have begun (a byte each on the
# shared pipes), so the scopes overlap whatever the machine's load.
cat > worker.py <<'PY'
import os, sys, urllib.request
import guard, keploy_v1 as k
PORT, name, mine, other = sys.argv[1], sys.argv[2], int(sys.argv[3]), int(sys.argv[4])
guard.check()
mode = k.claim(".", os.getcwd())
assert mode == k.LEGACY, "this check is the scope API without the proven switch, and the agent answered the claim: " + mode
assert k.begin(name, "worker.py", pid=os.getpid()) is None
os.write(mine, b"x")
assert os.read(other, 1) == b"x", "the other worker exited before it began its scope"
try:
    body = urllib.request.urlopen("http://127.0.0.1:%s/b" % PORT).read().decode()
    print("%s=SERVED:%s" % (name, body), flush=True)
except Exception:
    print("%s=MISS" % name, flush=True)
k.end(name, "passed", file="worker.py", pid=os.getpid())
PY

cat > run.py <<'PY'
import os, subprocess, sys
import marks
PORT = sys.argv[1]
marks.mark("begin")
r1, w1 = os.pipe()
r2, w2 = os.pipe()
ps = [subprocess.Popen(["python3", "worker.py", PORT, n, str(mine), str(other)], stdout=subprocess.PIPE, pass_fds=(mine, other))
      for n, mine, other in (("t1", w1, r2), ("t2", w2, r1))]
for fd in (r1, w1, r2, w2):
    os.close(fd)
for p in ps:
    print(p.communicate()[0].decode().strip(), flush=True)
marks.mark("end")
PY

# Without the switch, in a run recorded without the marks: one worker begins
# t1 with its pid. keploy refuses it; the driver prints BARE_STATUS= and
# BARE_PID=, keeps the whole answer in bare-body.txt (k.post: a Refused keeps
# only its first 512 bytes), and fails, as an integration must on a non-2xx
# answer.
cat > bare.py <<'PY'
import os, sys, urllib.request
import guard, keploy_v1 as k
PORT = sys.argv[1]
guard.check()
mode = k.claim(".", os.getcwd())
assert mode == k.LEGACY, "this check is the scope API without the proven switch, and the agent answered the claim: " + mode
print("BARE_PID=%d" % os.getpid(), flush=True)
status, raw = k.post("/agent/scope/begin", {"name": "t1", "file": "bare.py", "pid": os.getpid(), "canSkip": True})
print("BARE_STATUS=%d" % status, flush=True)
open("bare-body.txt", "wb").write(raw)
if status == 200:
    try:
        print("BARE_SERVED=" + urllib.request.urlopen("http://127.0.0.1:%s/b" % PORT).read().decode(), flush=True)
    except Exception:
        print("BARE_MISS", flush=True)
    k.end("t1", "passed", file="bare.py", pid=os.getpid())
    sys.exit(0)
sys.exit(1)
PY

# Without the switch, in a run recorded without the marks, a harness that
# does not check the begin's answer (as one that only logs a non-2xx does):
# one worker begins t1 with its pid, prints IGN_STATUS=, runs t1 anyway,
# calling t2's /b (IGN_B=SERVED:<body> or IGN_B=MISS), ends t1 as passed,
# and exits 0.
cat > ignore.py <<'PY'
import os, sys, urllib.request
import guard, keploy_v1 as k
PORT = sys.argv[1]
guard.check()
mode = k.claim(".", os.getcwd())
assert mode == k.LEGACY, "this check is the scope API without the proven switch, and the agent answered the claim: " + mode
status, _ = k.post("/agent/scope/begin", {"name": "t1", "file": "ignore.py", "pid": os.getpid(), "canSkip": True})
print("IGN_STATUS=%d" % status, flush=True)
try:
    print("IGN_B=SERVED:" + urllib.request.urlopen("http://127.0.0.1:%s/b" % PORT).read().decode(), flush=True)
except Exception:
    print("IGN_B=MISS", flush=True)
k.post("/agent/scope/end", {"name": "t1", "file": "ignore.py", "pid": os.getpid(), "outcome": "passed"})
sys.exit(0)
PY

# Without the switch, with the marks: an app the tests did not start (as an
# app server is), which opened its connection at boot, before any test (as a
# database pool does): mock/boot-pool takes a path on stdin and answers on
# stdout. rpool.py records t1 (/a) and t2 (/b) through it, its own child.
# At replay two of it are started beside shared.py, never under it: one
# reads app.in and answers on app.out, the other app2.in and app2.out. Two
# workers (sworker.py) begin t2, then t1, with their own pids and hold them
# open: the driver has the app call /b on its boot connection with t2 alone
# open (ONE_OPEN:), and the second app t1's /a on its own boot connection
# (BOOT2_ONE_OPEN:), then the app /b with both open (TWO_OPEN:), then ends t1
# and has the app call t1's /a with t2 alone open again (ONE_OPEN:), then
# ends t2.
cat > rpool.py <<'PY'
import os, subprocess, sys
import guard, keploy_v1 as k, marks
guard.check()
mode = k.claim(".", os.getcwd())
assert mode == k.LEGACY, "the suite mark is the scope API without the proven switch, and the agent answered the claim: " + mode
marks.mark("begin")
app = subprocess.Popen(["./bootpool", "127.0.0.1:" + sys.argv[1]], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
ready = app.stdout.readline().split()
assert ready and ready[0] == "READY", "the app did not open its boot connection: %r" % ready
for name, path, want in (("t1", "/a", "AAA"), ("t2", "/b", "BBB")):
    assert k.begin(name, "rpool.py", pid=os.getpid()) is None, "keploy told the record to skip " + name
    app.stdin.write(path + "\n")
    app.stdin.flush()
    got = app.stdout.readline().strip()
    assert got.startswith("%s=SERVED:%s:" % (path, want)), "the app's call of %s in %s: %s" % (path, name, got)
    reason = k.end(name, "passed", file="rpool.py", pid=os.getpid())
    assert reason is None, "keploy failed %s: %s" % (name, reason)
app.stdin.close()
app.wait()
marks.mark("end")
print("RECORD_OK", flush=True)
PY

cat > sworker.py <<'PY'
import os, sys
import guard, keploy_v1 as k
name = sys.argv[1]
guard.check()
mode = k.claim(".", os.getcwd())
assert mode == k.LEGACY, "this check is the scope API without the proven switch, and the agent answered the claim: " + mode
assert k.begin(name, "sworker.py", pid=os.getpid()) is None
print("began", flush=True)
sys.stdin.readline()
k.end(name, "passed", file="sworker.py", pid=os.getpid())
PY

cat > shared.py <<'PY'
import subprocess
import marks
def app(n):
    i, o = open("app%s.in" % n, "w"), open("app%s.out" % n)
    ready = o.readline().split()
    assert ready and ready[0] == "READY", "the app did not open its boot connection: %r" % ready
    print("APP%s_PID=%s" % (n, ready[1]), flush=True)
    return i, o
app1, app2 = app(""), app("2")
def ask(tag, path, a=app1):
    a[0].write(path + "\n")
    a[0].flush()
    print(tag + a[1].readline().strip(), flush=True)
marks.mark("begin")
def begin(n):
    w = subprocess.Popen(["python3", "sworker.py", n], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
    assert w.stdout.readline().strip() == "began", n + " did not begin its test"
    return w
def end(w):
    w.stdin.write("end\n")
    w.stdin.flush()
    w.wait()
w2 = begin("t2")
ask("ONE_OPEN:", "/b")
ask("BOOT2_ONE_OPEN:", "/a", app2)
w1 = begin("t1")
ask("TWO_OPEN:", "/b")
end(w1)
ask("ONE_OPEN:", "/a")
end(w2)
marks.mark("end")
app1[0].close()
app2[0].close()
PY

# Under the switch: two test processes, A and B, started side by side by a
# driver that claims nothing (so B does not inherit A's KEPLOY_TEST_PID and
# claims for itself, as a second `go test` binary or pytest worker would).
#   A claims the run, begins t1 and calls /a, then begins t2 while t1 is
#     open: refused. It then tells B to claim, and waits for B to finish.
#   B claims while A, alive, holds the run: refused.
# Each prints <who>_REFUSED=<status> <code>, then <who>_MESSAGE= and
# <who>_REMEDY=, and exits 1: a refused integration fails its run. Each says
# <who>_END=refused (or accepted) last, so an exit 1 that is the refusal is
# told apart from one that is a crash. The pipes are the order; nothing waits
# on a clock.
cat > refuse.py <<'PY'
import os, subprocess, sys, urllib.request
import guard, keploy_v1 as k
PORT = sys.argv[1]
def report(who, e):
    print("%s_REFUSED=%d %s" % (who, e.status, e.code), flush=True)
    print("%s_MESSAGE=%s" % (who, e.message), flush=True)
    print("%s_REMEDY=%s" % (who, e.remedy), flush=True)
if len(sys.argv) > 2:
    who, mine, other = sys.argv[2], int(sys.argv[3]), int(sys.argv[4])
    guard.check()
    if who == "A":
        assert k.claim(".", os.getcwd()) == k.V1, "the agent did not take the first test process's claim on scope protocol v1"
        print("A_PID=%d" % os.getpid(), flush=True)
        assert k.begin("t1", "refuse.py") is None
        assert urllib.request.urlopen("http://127.0.0.1:%s/a" % PORT).read() == b"AAA"
        refused = False
        try:
            k.begin("t2", "refuse.py")
            print("A_BEGAN_T2_WHILE_T1_WAS_OPEN", flush=True)
        except k.Refused as e:
            report("A", e)
            refused = True
        os.write(mine, b"x")
        assert os.read(other, 1) == b"x", "B exited before it claimed"
        assert k.end("t1", "passed", file="refuse.py") is None
        print("A_END=" + ("refused" if refused else "accepted"), flush=True)
        sys.exit(1 if refused else 0)
    # B
    assert os.read(other, 1) == b"x", "A exited before it claimed"
    print("B_PID=%d" % os.getpid(), flush=True)
    refused = False
    try:
        mode = k.claim(".", os.getcwd())
        print("B_CLAIMED_WHILE_A_HELD_THE_RUN=" + mode, flush=True)
    except k.Refused as e:
        report("B", e)
        refused = True
    os.write(mine, b"x")
    print("B_END=" + ("refused" if refused else "accepted"), flush=True)
    sys.exit(1 if refused else 0)
r1, w1 = os.pipe()
r2, w2 = os.pipe()
ps = [subprocess.Popen(["python3", "refuse.py", PORT, who, str(mine), str(other)], stdout=subprocess.PIPE, pass_fds=(mine, other))
      for who, mine, other in (("A", w1, r2), ("B", w2, r1))]
for fd in (r1, w1, r2, w2):
    os.close(fd)
codes = []
for p in ps:
    print(p.communicate()[0].decode().strip(), flush=True)
    codes.append(p.returncode)
print("EXITS=%s" % ",".join(map(str, codes)), flush=True)
sys.exit(0 if codes == [0, 0] else 1)
PY

python3 dep.py "$PORT" > dep.log 2>&1 &
DP=$!
trap 'kill "$DP" 2>/dev/null' EXIT
until grep -qx ready dep.log 2>/dev/null; do
  kill -0 "$DP" 2>/dev/null || { echo "FAIL: the dependency exited before it listened"; cat dep.log; exit 1; }
  sleep 0.1
done

# mocks_of LOG: the mock count keploy's record logged last.
mocks_of() {
  sed 's/\x1b\[[0-9;]*m//g' "$1" | grep -oE '"mocks": *[0-9]+' | grep -oE '[0-9]+' | tail -1
}
# line_of LOG KEY: the value of the driver's KEY= line.
line_of() {
  sed -n "s/^$2=//p" "$1" | head -1
}

if half on; then
echo "== 1. under the proven switch: one test process, two tests one after the other, passes =="
sudo env PATH="$PATH" "${SUDO_ENV[@]}" KEPLOY_PROVEN=1 "$RECORD_BIN" mock record -c "python3 rec.py $PORT" --name p-proven --disable-tele 2>&1 | tee rec-proven.log
RC=${PIPESTATUS[0]}
verify_ran 1 rec-proven.log || FAIL=1
[ "$RC" -eq 0 ] || fail "under the switch, one test process running two tests in turn should record (exit 0), got $RC"
[ "$(line_of rec-proven.log CLAIM)" = v1 ] || fail "under the switch the agent did not take the claim on scope protocol v1 (claim: '$(line_of rec-proven.log CLAIM)')"
grep -q "RECORD_OK" rec-proven.log || fail "the record driver did not complete under the switch"
[ "$(mocks_of rec-proven.log)" = "2" ] || fail "under the switch, expected 2 recorded mocks, got $(mocks_of rec-proven.log)"
# Each test's ID names its file, and its mapping its own one mock.
python3 - keploy/p-proven/mappings.yaml <<'PY' || FAIL=1
import re, sys
try:
    t = open(sys.argv[1]).read()
except OSError as e:
    sys.exit("FAIL: under the switch the record wrote no mapping: %s" % e)
got = {}
for block in re.split(r"\n(?=\s*- id: )", t):
    m = re.search(r"- id: (\S+)", block)
    if m:
        got[m.group(1)] = re.findall(r"- name: (\S+)", block)
if sorted(got) != ["rec.py::t1", "rec.py::t2"] or any(len(v) != 1 for v in got.values()) or got["rec.py::t1"] == got["rec.py::t2"]:
    sys.exit("FAIL: under the switch want rec.py::t1 and rec.py::t2 each mapped to its own one mock, got %s" % got)
PY

echo "== 2. under the proven switch: two test processes, and two tests at once, are refused =="
sudo env PATH="$PATH" "${SUDO_ENV[@]}" KEPLOY_PROVEN=1 "$RECORD_BIN" mock record -c "python3 refuse.py $PORT" --name p-refused --disable-tele 2>&1 | tee refused.log
RC=${PIPESTATUS[0]}
verify_ran 1 refused.log || FAIL=1
A_PID=$(line_of refused.log A_PID)
B_PID=$(line_of refused.log B_PID)
# Two tests at once in one process.
[ "$(line_of refused.log A_REFUSED)" = "422 one_test_at_a_time" ] ||
  fail "a test begun while another was open was not refused with 422 one_test_at_a_time (got '$(line_of refused.log A_REFUSED)')"
line_of refused.log A_MESSAGE | grep -F "refuse.py::t2 began while refuse.py::t1 was still open: two tests ran at once in one test process" >/dev/null ||
  fail "the one_test_at_a_time refusal does not say which tests ran at once: '$(line_of refused.log A_MESSAGE)'"
line_of refused.log A_REMEDY | grep -F "run them one at a time" >/dev/null ||
  fail "the one_test_at_a_time refusal does not say to run them one at a time: '$(line_of refused.log A_REMEDY)'"
line_of refused.log A_REMEDY | grep -F "later" >/dev/null &&
  fail "the one_test_at_a_time refusal promises a later release: '$(line_of refused.log A_REMEDY)'"
grep -q "A_BEGAN_T2_WHILE_T1_WAS_OPEN" refused.log && fail "the agent opened a second test while the first was open"
# A second test process.
[ "$(line_of refused.log B_REFUSED)" = "422 one_test_process_at_a_time" ] ||
  fail "a second test process's claim was not refused with 422 one_test_process_at_a_time (got '$(line_of refused.log B_REFUSED)')"
[ -n "$A_PID" ] && [ -n "$B_PID" ] &&
  line_of refused.log B_MESSAGE | grep -F "process $B_PID asked to run tests while process $A_PID still runs this keploy run's tests: two test processes ran at once" >/dev/null ||
  fail "the one_test_process_at_a_time refusal does not name both processes (A=$A_PID, B=$B_PID): '$(line_of refused.log B_MESSAGE)'"
line_of refused.log B_REMEDY | grep -F "run one test process at a time" >/dev/null ||
  fail "the one_test_process_at_a_time refusal does not say to run one test process at a time: '$(line_of refused.log B_REMEDY)'"
[ "$(line_of refused.log B_REMEDY)" = "run one test process at a time (go test -p 1, pytest without -n, jest --runInBand)" ] ||
  fail "the one_test_process_at_a_time refusal's fix is not to run one test process at a time, alone (this keploy runs one test worker): '$(line_of refused.log B_REMEDY)'"
grep -q "B_CLAIMED_WHILE_A_HELD_THE_RUN" refused.log && fail "the agent let a second test process claim the run while the first held it"
# The run does not pass: each process reached its end, refused, and failed.
[ "$(line_of refused.log A_END)" = refused ] || fail "the first test process did not end refused (A_END='$(line_of refused.log A_END)'): its exit is not the refusal"
[ "$(line_of refused.log B_END)" = refused ] || fail "the second test process did not end refused (B_END='$(line_of refused.log B_END)'): its exit is not the refusal"
grep -qx "EXITS=1,1" refused.log || fail "each refused test process should fail (exit 1): $(line_of refused.log EXITS)"
[ "$RC" -ne 0 ] || fail "a run whose test processes were refused passed (keploy exited 0)"
grep -q "tests failed; the recording was not kept" refused.log ||
  fail "keploy did not say the refused run's recording was not kept"
[ -e keploy/p-refused/mocks.yaml ] && fail "keploy kept a recording of a run it refused"
fi

if half off; then
echo "== 3. without the switch: record two endpoints under two pid scopes, with the suite mark and without it, and through the boot-pool app =="
# record_off NAME [marks]: records NAME without the switch.
record_off() {
  sudo env PATH="$PATH" "${SUDO_ENV[@]}" KEPLOY_PROVEN= "$RECORD_BIN" mock record -c "python3 rec.py $PORT ${2:-}" --name "$1" --disable-tele 2>&1 | tee "rec-$1.log"
  local rc=${PIPESTATUS[0]}
  [ "$rc" -eq 0 ] || fail "without the switch the record of $1 should pass (exit 0), got $rc"
  case "$(line_of "rec-$1.log" CLAIM)" in
    legacy) ;;
    "") fail "without the switch the record driver of $1 never claimed the run" ;;
    *) fail "without the switch the agent took the claim of $1 ($(line_of "rec-$1.log" CLAIM)): it has no claim route without the switch" ;;
  esac
  grep -q "RECORD_OK" "rec-$1.log" || fail "the record driver of $1 did not complete without the switch"
  [ "$(mocks_of "rec-$1.log")" = "2" ] || fail "expected 2 recorded mocks in $1, got $(mocks_of "rec-$1.log")"
}
record_off p marks
record_off p-bare
grep -q "^suites:" keploy/p/mappings.yaml || fail "the record with the suite mark kept no suite in its mapping"
grep -q "^suites:" keploy/p-bare/mappings.yaml && fail "the record without the suite mark kept a suite in its mapping"
( cd "$MOCK_DIR/boot-pool" && go build -o "$WORK/bootpool" . ) || fail "could not build the boot-pool app"
sudo env PATH="$PATH" "${SUDO_ENV[@]}" KEPLOY_PROVEN= "$RECORD_BIN" mock record -c "python3 rpool.py $PORT" --name pb --disable-tele 2>&1 | tee rec-pb.log
[ "${PIPESTATUS[0]}" -eq 0 ] || fail "without the switch the record of the boot-pool app should pass (exit 0)"
grep -q "RECORD_OK" rec-pb.log || fail "the boot-pool app's record driver did not complete"
[ "$(mocks_of rec-pb.log)" = "2" ] || fail "expected 2 recorded mocks in pb, got $(mocks_of rec-pb.log)"
kill "$DP" 2>/dev/null
wait "$DP" 2>/dev/null

echo "== 4. without the switch, with the marks: two concurrent workers, both hitting /b, dependency DOWN =="
sudo env PATH="$PATH" "${SUDO_ENV[@]}" KEPLOY_PROVEN= "$REPLAY_BIN" mock replay -c "python3 run.py $PORT" --name p --disable-tele 2>&1 | tee rep.log
grep -q "t2=SERVED:BBB" rep.log || fail "the worker scoped to t2 was NOT served /b"
grep -q "t1=MISS"       rep.log || fail "the worker scoped to t1 was NOT isolated (it saw another test's mock)"

echo "== 5. without the switch, without the marks: a test that begins with a pid is refused, and fails the run =="
sudo env PATH="$PATH" "${SUDO_ENV[@]}" KEPLOY_PROVEN= "$REPLAY_BIN" mock replay -c "python3 bare.py $PORT" --name p-bare --disable-tele 2>&1 | tee bare.log
RC=${PIPESTATUS[0]}
BARE_PID=$(line_of bare.log BARE_PID)
[ "$(line_of bare.log BARE_STATUS)" = 422 ] ||
  fail "a pid begin in a run without the marks was not refused with 422 (got '$(line_of bare.log BARE_STATUS)', $(grep -E '^BARE_(SERVED|MISS)' bare.log))"
[ -n "$BARE_PID" ] && head -1 bare-body.txt | grep -F "t1 began with the pid $BARE_PID: keploy narrows a test to its own mocks by the pid of its test process only in a run recorded with the suite mark or an app-start mark" >/dev/null ||
  fail "the refusal does not name the test and its pid (pid $BARE_PID): $(head -c 400 bare-body.txt)"
MARKS_WAY="Either add the suite mark, at record and at replay: begin a scope with \"suite\": true and the pid before the first test and end it after the last; when the tests start the app, also POST /agent/app/start {\"pid\": <the app's pid>} each time they do."
PIDLESS_WAY="Or send the begin without a pid, and run the tests one at a time: keploy then narrows every connection to the open test."
grep -qxF -- "$MARKS_WAY" bare-body.txt ||
  fail "the refusal does not give the marks, with the calls to make, as one way out on a line of its own: $(cat bare-body.txt)"
grep -qxF -- "$PIDLESS_WAY" bare-body.txt ||
  fail "the refusal does not give a pid-less begin, one test at a time, as the other way out on a line of its own: $(cat bare-body.txt)"
[ "$RC" -ne 0 ] || fail "a replay whose test was refused passed (keploy exited 0)"

echo "== 5b. without the switch, without the marks: a harness that ignores the refusal and runs the test is served nothing, and the replay fails =="
sudo env PATH="$PATH" "${SUDO_ENV[@]}" KEPLOY_PROVEN= "$REPLAY_BIN" mock replay -c "python3 ignore.py $PORT" --name p-bare --disable-tele 2>&1 | tee ignore.log
RC=${PIPESTATUS[0]}
sed 's/\x1b\[[0-9;]*m//g' ignore.log > ignore-plain.log
[ "$(line_of ignore.log IGN_STATUS)" = 422 ] ||
  fail "the ignoring harness's pid begin was not refused with 422 (got '$(line_of ignore.log IGN_STATUS)')"
grep -qx "IGN_B=MISS" ignore.log ||
  fail "the test keploy refused at its begin was served another test's recording: $(grep '^IGN_B=' ignore.log)"
grep -F "keploy serves no recording to a call of a test it refused at its begin" ignore-plain.log >/dev/null ||
  fail "the agent did not say why it refused the refused test's call"
grep -P '\tWARN\tkeploy refused a test at its begin' ignore-plain.log | grep -F '"test": "t1"' >/dev/null ||
  fail "the agent did not log the refusal of t1 at WARN"
grep -P '\tERROR\treplay failed: keploy refused a test at its begin' ignore-plain.log | grep -F '"test": "t1"' >/dev/null ||
  fail "keploy mock replay did not say, as an ERROR naming t1, that a test it refused fails the run"
grep -qx "runnerExitCode: 0" keploy/p-bare/last-replay.yaml ||
  fail "the ignoring harness should exit 0 (receipt: $(grep -E '^(exitCode|runnerExitCode|failedBy):' keploy/p-bare/last-replay.yaml | tr '\n' ' '))"
grep -qx "failedBy: begin_refused" keploy/p-bare/last-replay.yaml ||
  fail "the receipt does not say a test keploy refused at its begin failed the run: $(grep -E '^(exitCode|runnerExitCode|failedBy):' keploy/p-bare/last-replay.yaml | tr '\n' ' ')"
[ "$RC" -ne 0 ] || fail "a replay whose harness ran a test keploy refused at its begin passed (keploy exited 0, as the harness did)"
echo "== 6. without the switch, with the marks: an app's boot connection, of no test process, while one test is open and while two are =="
rm -f app.in app.out app2.in app2.out && mkfifo app.in app.out app2.in app2.out || fail "could not make the boot-pool apps' pipes"
sudo env PATH="$PATH" "${SUDO_ENV[@]}" KEPLOY_PROVEN= "$REPLAY_BIN" mock replay \
  -c "bash -c './bootpool 127.0.0.1:$PORT <app.in >app.out & ./bootpool 127.0.0.1:$PORT <app2.in >app2.out & python3 shared.py; rc=\$?; wait; exit \$rc'" \
  --name pb --disable-tele 2>&1 | tee shared.log
APP_PID=$(line_of shared.log APP_PID)
grep -qx "ONE_OPEN:/b=SERVED:BBB:boot" shared.log ||
  fail "with only t2 open, the app's call of t2's /b on its boot connection was not served: $(grep '^ONE_OPEN:/b' shared.log)"
grep -qx "BOOT2_ONE_OPEN:/a=MISS:boot" shared.log ||
  fail "with only t2 open, an app's boot connection was served t1's /a: $(grep '^BOOT2_ONE_OPEN:' shared.log)"
grep -qx "TWO_OPEN:/b=MISS:boot" shared.log ||
  fail "with two tests open, the app's boot connection, of neither test's process, was not refused: $(grep '^TWO_OPEN:' shared.log)"
[ -n "$APP_PID" ] && sed 's/\x1b\[[0-9;]*m//g' shared.log | grep -F "keploy cannot tell which test the call from pid $APP_PID belongs to: 2 tests are open" >/dev/null ||
  fail "the agent did not say why it refused the app's call (pid '$APP_PID')"
grep -qE "^ONE_OPEN:/a=MISS:" shared.log ||
  fail "with only t2 open, the app was served t1's /a: $(grep '^ONE_OPEN:/a' shared.log)"
fi

[ "$FAIL" = 0 ] && echo "MOCK PARALLEL E2E: PASSED" || echo "MOCK PARALLEL E2E: FAILED"
exit $FAIL
