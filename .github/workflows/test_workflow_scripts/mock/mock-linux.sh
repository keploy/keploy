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
# pytest-cov writes the coverage report step 4 reads back through keploy.
if ! python3 -c "import pytest_cov" 2>/dev/null; then
  python3 -m pip install --quiet --disable-pip-version-check pytest-cov
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

# The code under test, as its own module so step 4 can measure its coverage.
# never_called() is the part no test reaches.
cat > client.py <<PY
import json, urllib.request
BASE = "http://127.0.0.1:$PORT"
def get(p):
    with urllib.request.urlopen(BASE + p, timeout=5) as r:
        return json.load(r)
def never_called(x):
    if x:
        return "a"
    return "b"
PY

cat > test_api.py <<PY
from client import get
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

echo "== 2b. the replay left a receipt: local, git-ignored, and it says what was proven =="
# keploy mock status, the editor and an agent all read the verdict from here.
R=keploy/e2e/last-replay.yaml
if [ ! -f "$R" ]; then
  echo "FAIL: replay wrote no receipt at $R"; FAIL=1
else
  cat "$R"
  grep -qx 'exitCode: 0' "$R" || { echo "FAIL: receipt exitCode is not 0 for a passing replay"; FAIL=1; }
  grep -qx 'onMiss: fail' "$R" || { echo "FAIL: receipt does not record --on-miss fail"; FAIL=1; }
  grep -qx 'missed: 0' "$R"   || { echo "FAIL: receipt does not record zero misses"; FAIL=1; }
  grep -qE '^mocksDigest: [0-9a-f]{64}$' "$R" || { echo "FAIL: receipt has no digest of the recording it replayed"; FAIL=1; }
  grep -qx 'isolated: true' "$R" || { echo "FAIL: receipt does not record the proven isolation"; FAIL=1; }
  grep -qx 'runnerExitCode: 0' "$R" || { echo "FAIL: receipt does not record the test command's own exit"; FAIL=1; }
  # Ownership is deliberately NOT asserted here: main.go chowns keploy/ back to
  # $SUDO_USER after every mock command, so this would pass even if the
  # receipt's own fchown were deleted. The unit tests cover that write.
fi
grep -qxF '/*/last-replay.yaml' keploy/.gitignore || { echo "FAIL: keploy/.gitignore does not ignore the receipt"; FAIL=1; }

# The receipt must agree with the exit the shell saw, on EVERY run -- the green
# path is the one place where a flat exitCode happens to be right.
check_receipt_exit() {
  want="$1"
  got=$(sed -n 's/^exitCode: //p' keploy/e2e/last-replay.yaml)
  [ "$got" = "$want" ] || { echo "FAIL: receipt says exitCode $got, the shell saw $want"; FAIL=1; }
}
check_receipt_exit 0

echo "== 2c. a replay that lets calls reach real services proves nothing =="
# --on-miss passthrough: any call the recording lacks goes to the real
# dependency, so whatever the tests say, the run did not prove isolation.
sudo -E env PATH="$PATH" "$REPLAY_BIN" mock replay -c "python3 -m pytest -q -p no:cacheprovider test_api.py" --name e2e --on-miss passthrough --disable-tele 2>&1 | tee passthrough.log
grep -qx 'isolated: false' keploy/e2e/last-replay.yaml || { echo "FAIL: a passthrough replay claims isolation"; FAIL=1; }
grep -q "did not prove the tests run with the dependencies off" passthrough.log || { echo "FAIL: the passthrough run did not say what it failed to prove"; FAIL=1; }
# ...and the next proven replay puts the verdict back.
sudo -E env PATH="$PATH" "$REPLAY_BIN" mock replay -c "python3 -m pytest -q -p no:cacheprovider test_api.py" --name e2e --on-miss fail --disable-tele >/dev/null 2>&1
grep -qx 'isolated: true' keploy/e2e/last-replay.yaml || { echo "FAIL: a proven replay did not restore the verdict"; FAIL=1; }

echo "== 3. exit-code propagation: a failing runner must fail keploy =="
cat > test_fail.py <<PY
def test_boom(): assert False
PY
sudo -E env PATH="$PATH" "$REPLAY_BIN" mock replay -c "python3 -m pytest -q -p no:cacheprovider test_fail.py" --name e2e --disable-tele >/dev/null 2>&1
RC=$?
echo "failing-runner replay exit=$RC"
[ "$RC" -ne 0 ] || { echo "FAIL: a failing runner must make keploy exit non-zero"; FAIL=1; }
check_receipt_exit "$RC"
grep -qx 'failedBy: runner' keploy/e2e/last-replay.yaml || { echo "FAIL: the receipt does not say the tests were what failed"; FAIL=1; }

echo "== 4. offline coverage and the --min-coverage gate =="
# The same suite, with pytest-cov writing coverage.xml. The dependency is still
# stopped, so this is the coverage of a run with every call replayed.
COV_CMD="python3 -m pytest -q -p no:cacheprovider --cov=client --cov-report=xml test_api.py"
rm -f coverage.xml
sudo -E env PATH="$PATH" "$REPLAY_BIN" mock replay -c "$COV_CMD" --name e2e --min-coverage 50 --disable-tele 2>&1 | tee cov-pass.log
RC=${PIPESTATUS[0]}
[ "$RC" -eq 0 ] || { echo "FAIL: replay above the coverage floor should pass, got $RC"; FAIL=1; }
grep -q "offline coverage" cov-pass.log || { echo "FAIL: keploy did not report offline coverage"; FAIL=1; }
# The receipt must carry the runner's own numbers, not keploy's guess.
WANT_COV=$(python3 -c "import xml.etree.ElementTree as E; r=E.parse('coverage.xml').getroot(); print(r.get('lines-covered'), r.get('lines-valid'))")
GOT_COV=$(python3 - <<'PY'
import re
t = open("keploy/e2e/last-replay.yaml").read()
c = re.search(r"^    covered: (\d+)$", t, re.M); n = re.search(r"^    total: (\d+)$", t, re.M)
print(c.group(1) if c else "?", n.group(1) if n else "?")
PY
)
echo "coverage.xml: $WANT_COV, receipt: $GOT_COV"
[ "$WANT_COV" = "$GOT_COV" ] || { echo "FAIL: receipt coverage ($GOT_COV) is not the runner's ($WANT_COV)"; FAIL=1; }

sudo -E env PATH="$PATH" "$REPLAY_BIN" mock replay -c "$COV_CMD" --name e2e --min-coverage 95 --disable-tele 2>&1 | tee cov-fail.log
RC=${PIPESTATUS[0]}
[ "$RC" -ne 0 ] || { echo "FAIL: replay below the coverage floor must fail"; FAIL=1; }
grep -q "less of the code than the floor" cov-fail.log || { echo "FAIL: the floor failure was not explained"; FAIL=1; }
check_receipt_exit "$RC"
# Below the floor is not a failing test, and not a lost proof.
grep -qx 'failedBy: min-coverage' keploy/e2e/last-replay.yaml || { echo "FAIL: the receipt does not say the FLOOR failed the run"; FAIL=1; }
grep -qx 'isolated: true' keploy/e2e/last-replay.yaml || { echo "FAIL: a coverage failure threw away the proven isolation"; FAIL=1; }
grep -qx 'runnerExitCode: 0' keploy/e2e/last-replay.yaml || { echo "FAIL: a coverage failure was recorded as the tests failing"; FAIL=1; }

# A report the command does not name and keploy does not guess: only
# --coverage-report finds it. coverage.py writes it where .coveragerc says.
mkdir -p reports
cat > .coveragerc <<'CFG'
[xml]
output = reports/offline.xml
CFG
OUT_CMD="python3 -m pytest -q -p no:cacheprovider --cov=client --cov-report=xml test_api.py"
rm -f coverage.xml reports/offline.xml
sudo -E env PATH="$PATH" "$REPLAY_BIN" mock replay -c "$OUT_CMD" --name e2e --min-coverage 50 --disable-tele 2>&1 | tee cov-elsewhere.log
RC=${PIPESTATUS[0]}
[ "$RC" -ne 0 ] || { echo "FAIL: a report keploy cannot find must fail --min-coverage, not pass unmeasured"; FAIL=1; }
sudo -E env PATH="$PATH" "$REPLAY_BIN" mock replay -c "$OUT_CMD" --name e2e --coverage-report reports/offline.xml --min-coverage 50 --disable-tele 2>&1 | tee cov-named.log
RC=${PIPESTATUS[0]}
[ "$RC" -eq 0 ] || { echo "FAIL: --coverage-report did not find the report it was given, got $RC"; FAIL=1; }
grep -q "offline coverage" cov-named.log || { echo "FAIL: the named report was not reported as offline coverage"; FAIL=1; }
rm -f .coveragerc

# No report is not a pass: a floor that cannot measure must not let the run through.
sudo -E env PATH="$PATH" "$REPLAY_BIN" mock replay -c "python3 -m pytest -q -p no:cacheprovider test_api.py" --name e2e --min-coverage 10 --disable-tele 2>&1 | tee cov-none.log
RC=${PIPESTATUS[0]}
[ "$RC" -ne 0 ] || { echo "FAIL: --min-coverage with no coverage report must fail"; FAIL=1; }
grep -q "no coverage report to check" cov-none.log || { echo "FAIL: the missing report was not explained"; FAIL=1; }

if [ "$FAIL" -eq 0 ]; then echo "MOCK E2E: PASSED"; else echo "MOCK E2E: FAILED"; fi
exit "$FAIL"
