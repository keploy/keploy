#!/usr/bin/env bash
# End-to-end guard for PER-PID (worker-keyed) mock scoping — Keploy serving
# parallel test workers, each isolated to its own test's mocks (Design A).
#
# Records two endpoints under two named scopes (t1→/a, t2→/b), then in replay
# runs TWO CONCURRENT workers that BOTH call /b: the worker scoped to t2 must be
# SERVED /b, while the worker scoped to t1 must MISS it (its scope only allows
# /a's mock). Without per-PID scoping both would be served — so a MISS for t1 is
# the proof of isolation. Uses the PR `build` binary for record and replay.
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-keploy}"
WORK="$(mktemp -d)"
cd "$WORK"
PORT=18876
FAIL=0

cat > dep.py <<'PY'
import http.server, socketserver, sys
PORT = int(sys.argv[1])
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = {"/a": b"AAA", "/b": b"BBB"}.get(self.path, b"??")
        self.send_response(200); self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def log_message(self, *a): pass
socketserver.TCPServer.allow_reuse_address = True
with socketserver.TCPServer(("127.0.0.1", PORT), H) as s: s.serve_forever()
PY

# Record driver: one process, two SEQUENTIAL scopes each reporting its PID, so
# the mapping ends up t1->[/a mock], t2->[/b mock].
cat > rec.py <<'PY'
import os, json, urllib.request, urllib.error, sys, time
AG = os.environ["KEPLOY_MOCK_AGENT"]; PORT = sys.argv[1]; pid = os.getpid()
def _auth():
    # The agent guards its control plane with a bearer token and exports it
    # here for exactly this caller. Required, not optional: this lane runs the
    # PR build for record and replay (mock_linux.yml), so a missing token is
    # the regression, not an older binary.
    t = os.environ.get("KEPLOY_MOCK_AGENT_TOKEN")
    assert t, "keploy exported KEPLOY_MOCK_AGENT but no KEPLOY_MOCK_AGENT_TOKEN into the wrapped command"
    return {"Authorization": "Bearer " + t}
def scope(path, name):
    d = json.dumps({"name": name, "pid": pid}).encode()
    h = {"Content-Type": "application/json"}; h.update(_auth())
    urllib.request.urlopen(urllib.request.Request(AG + path, data=d, headers=h, method="POST"), timeout=5).read()
def call(p): return urllib.request.urlopen("http://127.0.0.1:%s%s" % (PORT, p), timeout=5).read()
time.sleep(3)  # let keploy warm up eBPF redirect so the FIRST call is intercepted (not raced)

# The control plane must REFUSE a caller with no credential. Without this the
# whole feature can regress to a no-op with a green pipeline: an agent that was
# handed no token serves everything, and every other assertion here still
# passes. Unconditional: record and replay both run the PR build here, and an
# agent that came up with no token is exactly what this must catch — skipping
# the check whenever the token is missing would skip it in the one case it
# exists for.
_auth()
try:
    urllib.request.urlopen(urllib.request.Request(
        AG + "/agent/scope/begin",
        data=json.dumps({"name": "unauthenticated-probe", "pid": pid}).encode(),
        headers={"Content-Type": "application/json"}, method="POST"), timeout=5).read()
except urllib.error.HTTPError as e:
    assert e.code == 401, "expected 401 for an unauthenticated scope call, got %d" % e.code
else:
    raise AssertionError("the agent served /agent/scope/begin to a caller with no token")
# And health stays reachable without one, so callers can still find the agent.
assert urllib.request.urlopen(AG + "/agent/health", timeout=5).status == 200
scope("/agent/scope/begin", "t1"); assert call("/a") == b"AAA"; scope("/agent/scope/end", "t1")
scope("/agent/scope/begin", "t2"); assert call("/b") == b"BBB"; scope("/agent/scope/end", "t2")
print("RECORD_OK", flush=True)
PY

# One worker: register its OWN pid under a scope, then call /b. Prints RESULT.
cat > worker.py <<'PY'
import os, json, urllib.request, sys, time
AG = os.environ["KEPLOY_MOCK_AGENT"]; PORT = sys.argv[1]; name = sys.argv[2]
def _auth():
    # The agent guards its control plane with a bearer token and exports it
    # here for exactly this caller. Required, not optional: this lane runs the
    # PR build for record and replay (mock_linux.yml), so a missing token is
    # the regression, not an older binary.
    t = os.environ.get("KEPLOY_MOCK_AGENT_TOKEN")
    assert t, "keploy exported KEPLOY_MOCK_AGENT but no KEPLOY_MOCK_AGENT_TOKEN into the wrapped command"
    return {"Authorization": "Bearer " + t}
def scope(path):
    d = json.dumps({"name": name, "pid": os.getpid()}).encode()
    h = {"Content-Type": "application/json"}; h.update(_auth())
    urllib.request.urlopen(urllib.request.Request(AG + path, data=d, headers=h, method="POST"), timeout=5).read()
scope("/agent/scope/begin")
time.sleep(2)  # overlap the two workers' scopes AND let the replay proxy warm up
try:
    body = urllib.request.urlopen("http://127.0.0.1:%s/b" % PORT, timeout=5).read().decode()
    print("%s=SERVED:%s" % (name, body), flush=True)
except Exception:
    print("%s=MISS" % name, flush=True)
scope("/agent/scope/end")
PY

# Replay driver: run the two workers CONCURRENTLY under one replay.
cat > run.py <<'PY'
import subprocess, sys
PORT = sys.argv[1]
ps = [subprocess.Popen(["python3", "worker.py", PORT, n], stdout=subprocess.PIPE) for n in ("t1", "t2")]
for p in ps:
    print(p.communicate()[0].decode().strip(), flush=True)
PY

start_dep() { python3 dep.py "$PORT" >dep.log 2>&1 & echo $!; }
DP=$(start_dep); sleep 1

echo "== 1. record two endpoints under two scopes =="
sudo -E env PATH="$PATH" "$RECORD_BIN" mock record -c "python3 rec.py $PORT" --name p --disable-tele 2>&1 | tee rec.log
grep -q "RECORD_OK" rec.log || { echo "FAIL: record driver did not complete"; FAIL=1; }
MOCKS=$(sed 's/\x1b\[[0-9;]*m//g' rec.log | grep -oE '"mocks": *[0-9]+' | grep -oE '[0-9]+' | tail -1)
[ "${MOCKS:-0}" = "2" ] || { echo "FAIL: expected 2 recorded mocks, got ${MOCKS:-0}"; FAIL=1; }
kill "$DP" 2>/dev/null; sleep 1

echo "== 2. replay: two concurrent workers, both hitting /b, dependency DOWN =="
sudo -E env PATH="$PATH" "$REPLAY_BIN" mock replay -c "python3 run.py $PORT" --name p --disable-tele 2>&1 | tee rep.log

echo "== 3. assertions (per-PID isolation) =="
grep -q "t2=SERVED:BBB" rep.log || { echo "FAIL: worker scoped to t2 was NOT served /b"; FAIL=1; }
grep -q "t1=MISS"       rep.log || { echo "FAIL: worker scoped to t1 was NOT isolated (it saw another test's mock)"; FAIL=1; }

[ "$FAIL" = 0 ] && echo "MOCK PARALLEL E2E: PASSED" || echo "MOCK PARALLEL E2E: FAILED"
exit $FAIL
