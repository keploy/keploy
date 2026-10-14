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
#      is the pool `keploy mock replay` stages once at BaseTime; a duplicate of
#      each matched recording used to make it 1,1,2,2,3,3.
#   2. two CONCURRENT workers, each registered under its own pid and scope: t1
#      must get 1,2,3 and t2 4,5 -- each walks only its own test's recordings,
#      neither advances the other's. The per-worker wrap used to drop the
#      cursor entirely (1,1,1), and a shared cursor would interleave them. The
#      pid travels on the scope API as it was, as today's parallel workers send
#      it: v1 has no pid (one test process per run until release 2).
#   3. t1 run twice by ONE worker process: the second run is a new scope (same
#      pid), so its sequence starts over at 1,2,3 instead of saturating.
#   4. ONE keep-alive connection reused for t1 and then t2 in one process, over
#      the v1 bodies (no pid, the scope v1 opens): what it sees follows the
#      test to t2 (4,5), not the scope it was opened in.
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
PORT=18877
FAIL=0
cp "$MOCK_DIR/keploy_v1.py" keploy_v1.py

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

# Record: one test process, two tests in turn -> t1: 1,2,3  t2: 4,5.
cat > rec.py <<'PY'
import os
import keploy_v1 as k
from kp import read
print("CLAIM=" + k.claim(".", os.getcwd()), flush=True)
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

# Replay 4: ONE keep-alive connection reused across two tests of one process,
# over the v1 bodies: what the connection sees must follow the test.
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
k.claim(".", os.getcwd())
assert k.begin("t1", "rec.py") is None
a = read(3); k.end("t1", "passed", file="rec.py")
assert k.begin("t2", "rec.py") is None
b = read(2); k.end("t2", "passed", file="rec.py")
print("KA_t1=" + a, flush=True)
print("KA_t2=" + b, flush=True)
print("KA_sockets=%d" % len(socks), flush=True)
PY

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

echo "== 1'. without the switch: record the same =="
start_dep
sudo -E env PATH="$PATH" KEPLOY_PROVEN= "$RECORD_BIN" mock record -c "python3 rec.py $PORT" --name s --disable-tele 2>&1 | tee rec.log
grep -qx "CLAIM=legacy" rec.log || { echo "FAIL: without the switch the record driver did not fall back to the scope API as it was"; FAIL=1; }
grep -q "RECORD_OK" rec.log || { echo "FAIL: record driver did not complete"; FAIL=1; }
[ "$(mocks_of rec.log)" = "5" ] || { echo "FAIL: expected 5 recorded mocks, got $(mocks_of rec.log)"; FAIL=1; }
stop_dep

echo "== 2. without the switch: replay unscoped, dependency DOWN: record order, then saturate =="
sudo -E env PATH="$PATH" KEPLOY_PROVEN= "$REPLAY_BIN" mock replay -c "python3 unscoped.py $PORT" --name s --disable-tele 2>&1 | tee rep-unscoped.log
has() { grep -qE "(^|[^[:alnum:]])$1([^[:alnum:],]|\$)" "$2"; }
has "UNSCOPED=1,2,3,4,5,5" rep-unscoped.log || { echo "FAIL: unscoped replay did not serve 1,2,3,4,5,5 (got: $(grep -o 'UNSCOPED=[^ ]*' rep-unscoped.log))"; FAIL=1; }

echo "== 3. without the switch: replay two concurrent pid-scoped workers, then a re-run, dependency DOWN =="
sudo -E env PATH="$PATH" KEPLOY_PROVEN= "$REPLAY_BIN" mock replay -c "python3 run.py $PORT" --name s --disable-tele 2>&1 | tee rep-workers.log
has "t1=1,2,3"      rep-workers.log || { echo "FAIL: worker t1 was not served its own 1,2,3 (got: $(grep -o 't1=[0-9,A-Z:a-z]*' rep-workers.log | head -1))"; FAIL=1; }
has "t2=4,5"        rep-workers.log || { echo "FAIL: worker t2 was not served its own 4,5 (got: $(grep -o 't2=[0-9,A-Z:a-z]*' rep-workers.log | head -1))"; FAIL=1; }
has "t1first=1,2,3" rep-workers.log || { echo "FAIL: t1 in a new worker process was not served 1,2,3 (got: $(grep -o 't1first=[0-9,A-Z:a-z]*' rep-workers.log | head -1))"; FAIL=1; }
has "t1again=1,2,3" rep-workers.log || { echo "FAIL: a re-run of t1 in the same process did not start its sequence over (got: $(grep -o 't1again=[0-9,A-Z:a-z]*' rep-workers.log | head -1))"; FAIL=1; }

echo "== 4. without the switch: one keep-alive connection across two tests, dependency DOWN =="
sudo -E env PATH="$PATH" KEPLOY_PROVEN= "$REPLAY_BIN" mock replay -c "python3 keepalive.py $PORT" --name s --disable-tele 2>&1 | tee rep-keepalive.log
has "KA_t1=1,2,3" rep-keepalive.log || { echo "FAIL: t1 on the keep-alive connection was not served its own 1,2,3 (got: $(grep -o 'KA_t1=[0-9,A-Z:a-z]*' rep-keepalive.log | head -1))"; FAIL=1; }
has "KA_t2=4,5" rep-keepalive.log || { echo "FAIL: t2 on the connection t1 opened was not served its own 4,5 (got: $(grep -o 'KA_t2=[0-9,A-Z:a-z]*' rep-keepalive.log | head -1))"; FAIL=1; }
has "KA_sockets=1" rep-keepalive.log || { echo "FAIL: the client reconnected, so step 4 did not test one connection across two tests ($(grep -o 'KA_sockets=[0-9]*' rep-keepalive.log | head -1))"; FAIL=1; }

[ "$FAIL" = 0 ] && echo "MOCK STATEFUL E2E: PASSED" || echo "MOCK STATEFUL E2E: FAILED"
exit $FAIL
