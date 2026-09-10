#!/usr/bin/env bash
# Self-contained e2e for `keploy mock record|replay` — Keploy as a mocking
# framework for a user's own test runner. Needs no external sample repo: it
# embeds a tiny HTTP dependency and a pytest suite, records the dependency
# calls, then replays them with the dependency STOPPED and asserts the runner
# still passes and the exit code is propagated.
#
# Env: RECORD_BIN / REPLAY_BIN (the keploy binaries under test), set by the
# .github/actions/download-binary composite action, as in the other lang scripts.
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
# root also needs an installation-id so the sudo'd agent doesn't prompt
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true

# Ensure pytest is importable by the same interpreter `python3 -m pytest` uses.
# setup-python provides python3 but not pytest; install it once (idempotent).
if ! python3 -c "import pytest" 2>/dev/null; then
  # Keep everything on the SAME python3 the test command uses: ensurepip
  # guarantees that interpreter has pip, then install pytest into it. (No
  # cross-interpreter `pip3` fallback, which could land pytest where
  # `python3 -m pytest` can't import it.)
  python3 -m ensurepip --default-pip >/dev/null 2>&1 || true
  python3 -m pip install --quiet --disable-pip-version-check pytest
fi

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-keploy}"
WORK="$(mktemp -d)"
cd "$WORK"
PORT=18999
FAIL=0

cat > depserver.py <<'PY'
import http.server, json, socketserver, sys
PORT=int(sys.argv[1]); c={'n':0}
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        c['n']+=1
        body=json.dumps({"path":self.path,"counter":c['n']}).encode()
        self.send_response(200); self.send_header("Content-Type","application/json")
        self.send_header("Content-Length",str(len(body))); self.end_headers(); self.wfile.write(body)
    def log_message(self,*a): pass
socketserver.TCPServer.allow_reuse_address=True
with socketserver.TCPServer(("127.0.0.1",PORT),H) as s: s.serve_forever()
PY

cat > conftest.py <<'PY'
import os, json, urllib.request, pytest
AGENT=os.environ.get("KEPLOY_MOCK_AGENT")
def _post(p,b):
    if not AGENT: return
    try:
        r=urllib.request.Request(AGENT+p,data=json.dumps(b).encode(),
            headers={"Content-Type":"application/json"},method="POST")
        urllib.request.urlopen(r,timeout=3).read()
    except Exception: pass
@pytest.fixture(autouse=True)
def keploy_scope(request):
    _post("/agent/scope/begin",{"name":request.node.name}); yield
    _post("/agent/scope/end",{"name":request.node.name})
PY

cat > test_api.py <<PY
import os, json, urllib.request
BASE="http://127.0.0.1:$PORT"
def get(p):
    with urllib.request.urlopen(BASE+p, timeout=5) as r: return json.load(r)
def test_users():  assert get("/users?id=1")["path"]=="/users?id=1"
def test_items():  assert get("/items/42")["path"]=="/items/42"
def test_health(): assert get("/health")["path"]=="/health"
PY

start_dep() { python3 depserver.py "$PORT" >dep.log 2>&1 & echo $!; }
stop_dep()  { kill "$1" 2>/dev/null; sleep 1; }

echo "== 1. record dependency calls =="
DEP_PID=$(start_dep); sleep 1
sudo -E env PATH="$PATH" "$RECORD_BIN" mock record -c "python3 -m pytest -q -p no:cacheprovider test_api.py" --name e2e --disable-tele 2>&1 | tee rec.log
stop_dep "$DEP_PID"
MOCKS=$(grep -c '^kind:' keploy/e2e/mocks.yaml 2>/dev/null || echo 0)
echo "recorded mocks: $MOCKS"
[ "$MOCKS" -eq 3 ] || { echo "FAIL: expected 3 mocks, got $MOCKS"; FAIL=1; }
[ -f keploy/e2e/mappings.yaml ] || { echo "FAIL: per-test mappings.yaml not written"; FAIL=1; }

echo "== 2. replay with the dependency STOPPED (must serve from mocks) =="
sudo -E env PATH="$PATH" "$REPLAY_BIN" mock replay -c "python3 -m pytest -q -p no:cacheprovider test_api.py" --name e2e --disable-tele 2>&1 | tee rep.log
RC=${PIPESTATUS[0]}
echo "replay exit=$RC"
[ "$RC" -eq 0 ] || { echo "FAIL: replay should pass entirely from mocks (exit 0), got $RC"; FAIL=1; }
grep -q "3 passed" rep.log || { echo "FAIL: pytest did not report 3 passed under replay"; FAIL=1; }

echo "== 3. exit-code propagation: a failing runner must fail keploy =="
cat > test_fail.py <<PY
def test_boom(): assert False
PY
sudo -E env PATH="$PATH" "$REPLAY_BIN" mock replay -c "python3 -m pytest -q -p no:cacheprovider test_fail.py" --name e2e --disable-tele >/dev/null 2>&1
RC=$?
echo "failing-runner replay exit=$RC"
[ "$RC" -ne 0 ] || { echo "FAIL: a failing runner must make keploy exit non-zero"; FAIL=1; }

if [ "$FAIL" -eq 0 ]; then echo "MOCK E2E: PASSED"; else echo "MOCK E2E: FAILED"; fi
exit "$FAIL"
