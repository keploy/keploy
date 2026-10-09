#!/usr/bin/env bash
# End-to-end guard for STATEFUL dependency replay in `keploy mock replay`: the
# same request recorded several times (a counter, a created-then-read row) is
# answered with its recorded responses in record order, then the last one again
# — not the first one forever.
#
# Records a counter dependency under two named scopes (t1 reads it 3 times, t2
# twice: 1,2,3 then 4,5), then replays with the dependency DOWN:
#   1. unscoped: one driver reads it six times and must get 1,2,3,4,5,5. This
#      is the pool `keploy mock replay` stages once at BaseTime; a duplicate of
#      each matched recording used to make it 1,1,2,2,3,3.
#   2. two CONCURRENT workers, each registered under its own pid and scope: t1
#      must get 1,2,3 and t2 4,5 — each walks only its own test's recordings,
#      neither advances the other's. The per-worker wrap used to drop the
#      cursor entirely (1,1,1), and a shared cursor would interleave them.
#   3. t1 run twice by ONE worker process: the second run is a new scope (same
#      pid), so its sequence starts over at 1,2,3 instead of saturating.
#   4. ONE keep-alive connection reused for t1 and then t2: what it sees
#      follows the worker to t2 (4,5), not the scope it was opened in.
# Uses the PR `build` binary for record and replay (mock_linux.yml).
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-keploy}"
WORK="$(mktemp -d)"
cd "$WORK"
PORT=18877
FAIL=0

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
with socketserver.TCPServer(("127.0.0.1", PORT), H) as s: s.serve_forever()
PY

# Shared by the drivers: the agent's scope API (token-guarded) and the counter.
cat > kp.py <<'PY'
import os, json, urllib.request, sys
AG = os.environ["KEPLOY_MOCK_AGENT"]; PORT = sys.argv[1]
def _auth():
    t = os.environ.get("KEPLOY_MOCK_AGENT_TOKEN")
    assert t, "keploy exported KEPLOY_MOCK_AGENT but no KEPLOY_MOCK_AGENT_TOKEN into the wrapped command"
    return {"Authorization": "Bearer " + t}
def scope(path, name):
    d = json.dumps({"name": name, "pid": os.getpid()}).encode()
    h = {"Content-Type": "application/json"}; h.update(_auth())
    urllib.request.urlopen(urllib.request.Request(AG + path, data=d, headers=h, method="POST"), timeout=5).read()
def read(k):
    return ",".join(urllib.request.urlopen("http://127.0.0.1:%s/counter" % PORT, timeout=5).read().decode() for _ in range(k))
PY

# Record: one process, two SEQUENTIAL pid-scoped tests -> t1: 1,2,3  t2: 4,5.
cat > rec.py <<'PY'
import time
from kp import scope, read
time.sleep(3)  # let keploy warm up eBPF redirect so the FIRST call is intercepted
scope("/agent/scope/begin", "t1"); assert read(3) == "1,2,3"; scope("/agent/scope/end", "t1")
scope("/agent/scope/begin", "t2"); assert read(2) == "4,5";   scope("/agent/scope/end", "t2")
print("RECORD_OK", flush=True)
PY

# Replay 1: no scope at all — the whole pool.
cat > unscoped.py <<'PY'
import time
from kp import read
time.sleep(3)
print("UNSCOPED=" + read(6), flush=True)
PY

# Replay 2/3: one worker process under its own pid. argv: PORT name reads
# [runs]: runs the test `runs` times in this process, each a new scope, and
# prints <name>=<served> for the first run and <name>again=<served> for the
# second.
cat > worker.py <<'PY'
import sys, time
from kp import scope, read
name, k = sys.argv[2], int(sys.argv[3])
runs = int(sys.argv[4]) if len(sys.argv) > 4 else 1
for i in range(runs):
    label = name if i == 0 else name + "again"
    scope("/agent/scope/begin", name)
    time.sleep(2)  # overlap concurrent workers' scopes AND let the replay proxy warm up
    try:
        print("%s=%s" % (label, read(k)), flush=True)
    except Exception as e:
        print("%s=ERROR:%s" % (label, e), flush=True)
    scope("/agent/scope/end", name)
PY

cat > run.py <<'PY'
import subprocess, sys
PORT = sys.argv[1]
ps = [subprocess.Popen(["python3", "worker.py", PORT, n, k], stdout=subprocess.PIPE) for n, k in (("t1", "3"), ("t2", "2"))]
for p in ps:
    print(p.communicate()[0].decode().strip(), flush=True)
# One process running t1 twice: same pid, so only the second run being a NEW
# scope can start its sequence over (a fresh process would hide that).
print(subprocess.run(["python3", "worker.py", PORT, "t1", "3", "2"], stdout=subprocess.PIPE).stdout.decode().strip().replace("t1=", "t1first="), flush=True)
PY

# Replay 4: ONE keep-alive connection reused across two scopes in one worker:
# what the connection sees must follow the worker to its next test.
# The recording was made with urllib, so each request carries the header keys
# urllib sends (matching requires them); Connection: keep-alive keeps the one
# socket open, and the socket's address proves it was never reopened — a new
# connection opened in t2 would pass this step without the fix.
cat > keepalive.py <<'PY'
import http.client, sys, time
from kp import scope
conn = http.client.HTTPConnection("127.0.0.1", int(sys.argv[1]), timeout=5)
socks = set()
def read(k):
    out = []
    for _ in range(k):
        conn.request("GET", "/counter", headers={"User-Agent": "Python-urllib/3", "Connection": "keep-alive"})
        out.append(conn.getresponse().read().decode())
        # http.client drops its socket when the peer will close after the
        # response; a reconnect per request would otherwise hide here.
        assert conn.sock is not None, "the connection was closed after a response"
        socks.add(conn.sock.getsockname())
    return ",".join(out)
scope("/agent/scope/begin", "t1"); time.sleep(2)
a = read(3); scope("/agent/scope/end", "t1")
scope("/agent/scope/begin", "t2")
b = read(2); scope("/agent/scope/end", "t2")
print("KA_t1=" + a, flush=True)
print("KA_t2=" + b, flush=True)
print("KA_sockets=%d" % len(socks), flush=True)
PY

start_dep() { python3 dep.py "$PORT" >dep.log 2>&1 & echo $!; }
DP=$(start_dep); sleep 1

echo "== 1. record a counter under two scopes (t1: 1,2,3  t2: 4,5) =="
sudo -E env PATH="$PATH" "$RECORD_BIN" mock record -c "python3 rec.py $PORT" --name s --disable-tele 2>&1 | tee rec.log
grep -q "RECORD_OK" rec.log || { echo "FAIL: record driver did not complete"; FAIL=1; }
MOCKS=$(sed 's/\x1b\[[0-9;]*m//g' rec.log | grep -oE '"mocks": *[0-9]+' | grep -oE '[0-9]+' | tail -1)
[ "${MOCKS:-0}" = "5" ] || { echo "FAIL: expected 5 recorded mocks, got ${MOCKS:-0}"; FAIL=1; }
kill "$DP" 2>/dev/null; sleep 1

echo "== 2. replay unscoped, dependency DOWN: record order, then saturate =="
sudo -E env PATH="$PATH" "$REPLAY_BIN" mock replay -c "python3 unscoped.py $PORT" --name s --disable-tele 2>&1 | tee rep-unscoped.log
has() { grep -qE "(^|[^[:alnum:]])$1([^[:alnum:],]|\$)" "$2"; }
has "UNSCOPED=1,2,3,4,5,5" rep-unscoped.log || { echo "FAIL: unscoped replay did not serve 1,2,3,4,5,5 (got: $(grep -o 'UNSCOPED=[^ ]*' rep-unscoped.log))"; FAIL=1; }

echo "== 3. replay two concurrent scoped workers, then a re-run, dependency DOWN =="
sudo -E env PATH="$PATH" "$REPLAY_BIN" mock replay -c "python3 run.py $PORT" --name s --disable-tele 2>&1 | tee rep-workers.log
has "t1=1,2,3"      rep-workers.log || { echo "FAIL: worker t1 was not served its own 1,2,3 (got: $(grep -o 't1=[0-9,A-Z:a-z]*' rep-workers.log | head -1))"; FAIL=1; }
has "t2=4,5"        rep-workers.log || { echo "FAIL: worker t2 was not served its own 4,5 (got: $(grep -o 't2=[0-9,A-Z:a-z]*' rep-workers.log | head -1))"; FAIL=1; }
has "t1first=1,2,3" rep-workers.log || { echo "FAIL: t1 in a new worker process was not served 1,2,3 (got: $(grep -o 't1first=[0-9,A-Z:a-z]*' rep-workers.log | head -1))"; FAIL=1; }
has "t1again=1,2,3" rep-workers.log || { echo "FAIL: a re-run of t1 in the same process did not start its sequence over (got: $(grep -o 't1again=[0-9,A-Z:a-z]*' rep-workers.log | head -1))"; FAIL=1; }

echo "== 4. one keep-alive connection across two scopes, dependency DOWN =="
sudo -E env PATH="$PATH" "$REPLAY_BIN" mock replay -c "python3 keepalive.py $PORT" --name s --disable-tele 2>&1 | tee rep-keepalive.log
has "KA_t1=1,2,3" rep-keepalive.log || { echo "FAIL: t1 on the keep-alive connection was not served its own 1,2,3 (got: $(grep -o 'KA_t1=[0-9,A-Z:a-z]*' rep-keepalive.log | head -1))"; FAIL=1; }
has "KA_t2=4,5" rep-keepalive.log || { echo "FAIL: t2 on the connection t1 opened was not served its own 4,5 (got: $(grep -o 'KA_t2=[0-9,A-Z:a-z]*' rep-keepalive.log | head -1))"; FAIL=1; }
has "KA_sockets=1" rep-keepalive.log || { echo "FAIL: the client reconnected, so step 4 did not test one connection across two tests ($(grep -o 'KA_sockets=[0-9]*' rep-keepalive.log | head -1))"; FAIL=1; }

[ "$FAIL" = 0 ] && echo "MOCK STATEFUL E2E: PASSED" || echo "MOCK STATEFUL E2E: FAILED"
exit $FAIL
