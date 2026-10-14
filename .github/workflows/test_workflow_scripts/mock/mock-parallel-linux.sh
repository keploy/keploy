#!/usr/bin/env bash
# End-to-end guard for parallel test workers in `keploy mock`, both ways:
#
# Under the proven switch (KEPLOY_PROVEN=1), a REFUSAL check until release 2.
# The first release runs one test process per keploy run, with one test open
# at a time (release1-cut §2.1); parallel workers arrive in release 2. So, in
# `keploy mock record`, over scope protocol v1:
#   - a test process that claims the run while another live one holds it is
#     refused: 422 one_test_process_at_a_time;
#   - a test that begins while another test of the same process is open is
#     refused: 422 one_test_at_a_time;
# each with a message that says what ran at once, and a remedy that names
# running one at a time and the later release. Each refused process fails, as
# an integration must on any non-2xx answer, so the run does not pass: keploy
# exits non-zero and keeps no recording. A control run, one process running
# the same two tests one after the other, passes and records both.
#
# Without the switch (today's default, which the flip deletes), PER-PID mock
# scoping: two named scopes recorded (t1->/a, t2->/b), then TWO CONCURRENT
# workers in one replay that BOTH call /b, each registered under its own pid:
# the worker scoped to t2 must be SERVED /b, the worker scoped to t1 must MISS
# it (its scope allows only /a's mock). Without per-PID scoping both would be
# served, so t1's MISS is the proof of isolation. The pid travels on the scope
# API as it was, which is how today's parallel workers call it; v1 has no pid.
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
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true

# This lane's directory: the shared harness (keploy_v1.py).
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

[ -f "$MOCK_DIR/lane_switch.sh" ] || { echo "FAIL: no lane_switch.sh beside this lane, in $MOCK_DIR"; exit 1; }
source "$MOCK_DIR/lane_switch.sh"
cp "$MOCK_DIR/keploy_v1.py" keploy_v1.py || { echo "FAIL: no keploy_v1.py beside this lane, in $MOCK_DIR"; exit 1; }

cat > dep.py <<'PY'
import http.server, socketserver, sys
PORT = int(sys.argv[1])
class H(http.server.BaseHTTPRequestHandler):
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

# One test process running t1 (/a) then t2 (/b), each in a scope of its own.
# argv: PORT [legacy-pid]: with legacy-pid, an agent without the switch is
# sent this process's pid on each call, as today's parallel workers send it.
cat > rec.py <<'PY'
import os, sys, urllib.request
import guard, keploy_v1 as k
PORT, legacy_pid = sys.argv[1], len(sys.argv) > 2
def call(p): return urllib.request.urlopen("http://127.0.0.1:%s%s" % (PORT, p)).read()
guard.check()
mode = k.claim(".", os.getcwd())
pid = os.getpid() if legacy_pid else None
print("CLAIM=" + mode, flush=True)
for name, path, want in (("t1", "/a", b"AAA"), ("t2", "/b", b"BBB")):
    assert k.begin(name, "rec.py", pid=pid) is None, "keploy told the record to skip " + name
    assert call(path) == want
    reason = k.end(name, "passed", file="rec.py", pid=pid)
    assert reason is None, "keploy failed %s: %s" % (name, reason)
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
PORT = sys.argv[1]
r1, w1 = os.pipe()
r2, w2 = os.pipe()
ps = [subprocess.Popen(["python3", "worker.py", PORT, n, str(mine), str(other)], stdout=subprocess.PIPE, pass_fds=(mine, other))
      for n, mine, other in (("t1", w1, r2), ("t2", w2, r1))]
for fd in (r1, w1, r2, w2):
    os.close(fd)
for p in ps:
    print(p.communicate()[0].decode().strip(), flush=True)
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
sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$RECORD_BIN" mock record -c "python3 rec.py $PORT" --name p-proven --disable-tele 2>&1 | tee rec-proven.log
RC=${PIPESTATUS[0]}
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
sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$RECORD_BIN" mock record -c "python3 refuse.py $PORT" --name p-refused --disable-tele 2>&1 | tee refused.log
RC=${PIPESTATUS[0]}
A_PID=$(line_of refused.log A_PID)
B_PID=$(line_of refused.log B_PID)
# Two tests at once in one process.
[ "$(line_of refused.log A_REFUSED)" = "422 one_test_at_a_time" ] ||
  fail "a test begun while another was open was not refused with 422 one_test_at_a_time (got '$(line_of refused.log A_REFUSED)')"
line_of refused.log A_MESSAGE | grep -qF "refuse.py::t2 began while refuse.py::t1 was still open: two tests ran at once in one test process" ||
  fail "the one_test_at_a_time refusal does not say which tests ran at once: '$(line_of refused.log A_MESSAGE)'"
line_of refused.log A_REMEDY | grep -qF "run them one at a time" ||
  fail "the one_test_at_a_time refusal does not say to run them one at a time: '$(line_of refused.log A_REMEDY)'"
line_of refused.log A_REMEDY | grep -qF "parallel test workers arrive in a later keploy release" ||
  fail "the one_test_at_a_time refusal does not say parallel workers come later: '$(line_of refused.log A_REMEDY)'"
grep -q "A_BEGAN_T2_WHILE_T1_WAS_OPEN" refused.log && fail "the agent opened a second test while the first was open"
# A second test process.
[ "$(line_of refused.log B_REFUSED)" = "422 one_test_process_at_a_time" ] ||
  fail "a second test process's claim was not refused with 422 one_test_process_at_a_time (got '$(line_of refused.log B_REFUSED)')"
[ -n "$A_PID" ] && [ -n "$B_PID" ] &&
  line_of refused.log B_MESSAGE | grep -qF "process $B_PID asked to run tests while process $A_PID still runs this keploy run's tests: two test processes ran at once" ||
  fail "the one_test_process_at_a_time refusal does not name both processes (A=$A_PID, B=$B_PID): '$(line_of refused.log B_MESSAGE)'"
line_of refused.log B_REMEDY | grep -qF "run one test process at a time" ||
  fail "the one_test_process_at_a_time refusal does not say to run one test process at a time: '$(line_of refused.log B_REMEDY)'"
line_of refused.log B_REMEDY | grep -qF "parallel test workers arrive in a later keploy release" ||
  fail "the one_test_process_at_a_time refusal does not say parallel workers come later: '$(line_of refused.log B_REMEDY)'"
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
echo "== 3. without the switch: record two endpoints under two pid scopes =="
sudo -E env PATH="$PATH" KEPLOY_PROVEN= "$RECORD_BIN" mock record -c "python3 rec.py $PORT legacy-pid" --name p --disable-tele 2>&1 | tee rec.log
RC=${PIPESTATUS[0]}
[ "$RC" -eq 0 ] || fail "without the switch the record should pass (exit 0), got $RC"
case "$(line_of rec.log CLAIM)" in
  legacy) ;;
  "") fail "without the switch the record driver never claimed the run" ;;
  *) fail "without the switch the agent took the claim ($(line_of rec.log CLAIM)): it has no claim route without the switch" ;;
esac
grep -q "RECORD_OK" rec.log || fail "the record driver did not complete without the switch"
[ "$(mocks_of rec.log)" = "2" ] || fail "expected 2 recorded mocks, got $(mocks_of rec.log)"
kill "$DP" 2>/dev/null
wait "$DP" 2>/dev/null

echo "== 4. without the switch: two concurrent workers, both hitting /b, dependency DOWN =="
sudo -E env PATH="$PATH" KEPLOY_PROVEN= "$REPLAY_BIN" mock replay -c "python3 run.py $PORT" --name p --disable-tele 2>&1 | tee rep.log
grep -q "t2=SERVED:BBB" rep.log || fail "the worker scoped to t2 was NOT served /b"
grep -q "t1=MISS"       rep.log || fail "the worker scoped to t1 was NOT isolated (it saw another test's mock)"
fi

[ "$FAIL" = 0 ] && echo "MOCK PARALLEL E2E: PASSED" || echo "MOCK PARALLEL E2E: FAILED"
exit $FAIL
