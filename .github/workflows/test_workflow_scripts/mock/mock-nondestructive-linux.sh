#!/usr/bin/env bash
# Non-destructive record e2e for `keploy mock record`.
#
# `keploy mock record --name X` must NEVER destroy X's existing recording when a
# re-record fails, is interrupted, or captures nothing. Before the fix, record
# dropped the named set's mocks BEFORE capture even armed, so any such run left
# the user with an empty set where a good recording had been (gaps W1/W14;
# design §P0b "no delete-first"). The fix captures into a staging set and
# promotes it over X only on a complete, non-empty run.
#
# This mirrors mock-linux.sh's embedded HTTP dependency + pytest harness, then:
#   1. records a baseline recording and snapshots its bytes,
#   2. re-records with a command that captures nothing  -> bytes unchanged,
#   3. re-records with a command that FAILS             -> bytes unchanged,
#   4. re-records a different workload successfully       -> fully replaced.
# A leftover staging set is a failure in every case.
#
# Env: RECORD_BIN (the keploy binary under test), set by download-binary.
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
# root also needs an installation-id so the sudo'd agent doesn't prompt
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true

if ! python3 -c "import pytest" 2>/dev/null; then
  python3 -m ensurepip --default-pip >/dev/null 2>&1 || true
  python3 -m pip install --quiet --disable-pip-version-check pytest
fi

RECORD_BIN="${RECORD_BIN:-keploy}"
WORK="$(mktemp -d)"
cd "$WORK"
PORT=18997
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

cat > client.py <<PY
import json, urllib.request
BASE = "http://127.0.0.1:$PORT"
def get(p):
    with urllib.request.urlopen(BASE + p, timeout=5) as r:
        return json.load(r)
PY

cat > test_api.py <<PY
from client import get
def test_users():  assert get("/users?id=1")["path"]=="/users?id=1"
def test_items():  assert get("/items/42")["path"]=="/items/42"
def test_health(): assert get("/health")["path"]=="/health"
PY

start_dep() { python3 depserver.py "$PORT" >dep.log 2>&1 & echo $!; }
stop_dep()  { kill "$1" 2>/dev/null; sleep 1; }

SET=nd
MFILE="keploy/$SET/mocks.yaml"
STAGING="keploy/$SET.keploy-staging"

no_staging_left() {
  [ ! -e "$STAGING" ] || { echo "FAIL: $1 left a staging set behind: $STAGING"; FAIL=1; }
}

echo "== 1. record a baseline recording =="
DEP_PID=$(start_dep); sleep 1
sudo -E env PATH="$PATH" "$RECORD_BIN" mock record -c "python3 -m pytest -q -p no:cacheprovider test_api.py" --name "$SET" --disable-tele 2>&1 | tee rec.log
stop_dep "$DEP_PID"
[ -f "$MFILE" ] || { echo "FAIL: baseline recording not written"; FAIL=1; echo "MOCK NON-DESTRUCTIVE E2E: FAILED"; exit 1; }
# grep -c prints "0" and exits 1 on no match; capture the count, default to 0 if
# the file is absent (so the test never trips on a multiline value).
MOCKS=$(grep -c '^kind:' "$MFILE" 2>/dev/null); MOCKS=${MOCKS:-0}
echo "baseline mocks: $MOCKS"
[ "$MOCKS" -eq 3 ] || { echo "FAIL: expected 3 baseline mocks, got $MOCKS"; FAIL=1; }
BASE_HASH=$(sha256sum "$MFILE" | awk '{print $1}')
echo "baseline hash: $BASE_HASH"
no_staging_left "the baseline record"

echo "== 2. a re-record that captures NOTHING must leave the recording intact =="
# The command makes no external calls, so nothing is captured. Before the fix,
# record deleted the set first, so this left the user with an empty set.
sudo -E env PATH="$PATH" "$RECORD_BIN" mock record -c "python3 -c 'pass'" --name "$SET" --disable-tele 2>&1 | tee rec-empty.log
[ -f "$MFILE" ] || { echo "FAIL: a zero-capture re-record DESTROYED the existing recording"; FAIL=1; }
NOW_HASH=$(sha256sum "$MFILE" 2>/dev/null | awk '{print $1}')
[ "$NOW_HASH" = "$BASE_HASH" ] || { echo "FAIL: a zero-capture re-record changed the existing recording ($NOW_HASH != $BASE_HASH)"; FAIL=1; }
no_staging_left "a zero-capture re-record"

echo "== 3. a re-record whose command FAILS (capturing nothing) also preserves it =="
sudo -E env PATH="$PATH" "$RECORD_BIN" mock record -c "python3 -c 'import sys; sys.exit(7)'" --name "$SET" --disable-tele 2>&1 | tee rec-fail.log
[ -f "$MFILE" ] || { echo "FAIL: a failed re-record DESTROYED the existing recording"; FAIL=1; }
NOW_HASH=$(sha256sum "$MFILE" 2>/dev/null | awk '{print $1}')
[ "$NOW_HASH" = "$BASE_HASH" ] || { echo "FAIL: a failed re-record changed the existing recording"; FAIL=1; }
no_staging_left "a failed re-record"

echo "== 4. a successful re-record fully REPLACES the recording (clean rewrite) =="
# A different workload, so the new recording must differ from the baseline and
# must not keep any of the baseline's calls.
cat > test_api.py <<PY
from client import get
def test_a(): assert get("/alpha")["path"]=="/alpha"
def test_b(): assert get("/beta")["path"]=="/beta"
PY
DEP_PID=$(start_dep); sleep 1
sudo -E env PATH="$PATH" "$RECORD_BIN" mock record -c "python3 -m pytest -q -p no:cacheprovider test_api.py" --name "$SET" --disable-tele 2>&1 | tee rec2.log
stop_dep "$DEP_PID"
MOCKS=$(grep -c '^kind:' "$MFILE" 2>/dev/null); MOCKS=${MOCKS:-0}
echo "re-recorded mocks: $MOCKS"
[ "$MOCKS" -eq 2 ] || { echo "FAIL: expected 2 mocks after a clean re-record, got $MOCKS"; FAIL=1; }
NOW_HASH=$(sha256sum "$MFILE" 2>/dev/null | awk '{print $1}')
[ "$NOW_HASH" != "$BASE_HASH" ] || { echo "FAIL: a successful re-record did not replace the baseline"; FAIL=1; }
grep -q '/alpha' "$MFILE" || { echo "FAIL: the new recording is missing its calls"; FAIL=1; }
if grep -q '/users' "$MFILE"; then echo "FAIL: the baseline's calls survived a clean rewrite (appended, not replaced)"; FAIL=1; fi
no_staging_left "a successful re-record"

if [ "$FAIL" -eq 0 ]; then echo "MOCK NON-DESTRUCTIVE E2E: PASSED"; else echo "MOCK NON-DESTRUCTIVE E2E: FAILED"; fi
exit "$FAIL"
