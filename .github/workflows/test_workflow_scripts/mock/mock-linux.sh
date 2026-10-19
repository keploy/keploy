#!/usr/bin/env bash
# Self-contained e2e for `keploy mock record|replay` — Keploy as a mocking
# framework for a user's own test runner. Needs no external sample repo: it
# embeds a tiny HTTP dependency and a pytest suite, records the dependency
# calls, then replays them with the dependency STOPPED and asserts the runner
# still passes and the exit code is propagated.
#
# Both ways. Steps 1-5 run without the proven switch: the record and replay
# today's users run, on their default (the flip deletes them with the switch,
# as this build refuses every replay under it). Steps 6-7 run under
# KEPLOY_PROVEN=1, which the flip makes the default: the record keeps each
# test's mocks and begin under the test's ID, and this build, which does not
# judge each test (keploy enterprise does), refuses the replay before it runs
# the test command. The suite's harness speaks scope protocol v1 both ways
# (keploy_v1.py, conftest_v1.py); without the switch it sends the worker's
# pid, as today's workers do, and the record and the gated replay run again
# with no pid (KEPLOY_E2E_NO_PID), the optional single shared pool.
# MOCK_LANE_SWITCH (lane_switch.sh) picks the half a run runs; CI runs each in
# a job of its own.
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

# This lane's directory: the shared harness (keploy_v1.py, conftest_v1.py).
# shellcheck source-path=SCRIPTDIR
MOCK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
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
with socketserver.TCPServer(("127.0.0.1",PORT),H) as s:
    print("ready", flush=True)
    s.serve_forever()
PY

# The test harness: keploy's scope protocol v1 (claim the run, then begin each
# test with its file and canSkip, and end it with its outcome), failing a test
# on any answer it cannot trust. An agent without the proven switch has no
# claim route; the harness then speaks the scope API as it was (keploy_v1.py).
# A lane that could not load it would skip both halves and pass: fail instead.
# shellcheck source=lane_switch.sh
if ! source "$MOCK_DIR/lane_switch.sh" || ! declare -F half >/dev/null; then
  echo "FAIL: could not load lane_switch.sh from $MOCK_DIR"
  exit 1
fi
cp "$MOCK_DIR/keploy_v1.py" keploy_v1.py || { echo "FAIL: no keploy_v1.py beside this lane, in $MOCK_DIR"; exit 1; }
cp "$MOCK_DIR/conftest_v1.py" conftest.py || { echo "FAIL: no conftest_v1.py beside this lane, in $MOCK_DIR"; exit 1; }

# outcome_of NAME [SET]: the verdict the last replay of SET (e2e unless
# given) gives a test (per-test file).
outcome_of() {
  python3 - "$1" "${2:-e2e}" <<'PY'
import re, sys
try:
    t = open("keploy/%s/last-replay-tests.yaml" % sys.argv[2]).read()
except FileNotFoundError:
    t = ""
m = re.search(r"- name: " + re.escape(sys.argv[1]) + r"\n((?:\s+\w+: .*\n)*)", t)
o = re.search(r"outcome: (\w+)", m.group(1)) if m else None
print(o.group(1) if o else "none")
PY
}

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

# start_dep: starts the dependency and waits for its ready line (depserver.py
# prints it after binding). stop_dep stops it by its pid and waits for it to
# go, so the replays after it reach no dependency.
DEP_PID=""
trap '[ -z "$DEP_PID" ] || kill "$DEP_PID" 2>/dev/null' EXIT
start_dep() {
  : > dep.log
  python3 depserver.py "$PORT" >dep.log 2>&1 &
  DEP_PID=$!
  until grep -qx ready dep.log 2>/dev/null; do
    kill -0 "$DEP_PID" 2>/dev/null || { echo "FAIL: the dependency exited before it listened"; cat dep.log; exit 1; }
    sleep 0.1
  done
}
stop_dep() { kill "$DEP_PID" 2>/dev/null; wait "$DEP_PID" 2>/dev/null; DEP_PID=""; }

if half off; then
echo "== 1. record dependency calls =="
start_dep
sudo -E env PATH="$PATH" "$RECORD_BIN" mock record -c "python3 -m pytest -q -p no:cacheprovider test_api.py" --name e2e --disable-tele 2>&1 | tee rec.log
stop_dep
MOCKS=$(grep -c '^kind:' keploy/e2e/mocks.yaml 2>/dev/null || echo 0)
echo "recorded mocks: $MOCKS"
[ "$MOCKS" -eq 3 ] || { echo "FAIL: expected 3 mocks, got $MOCKS"; FAIL=1; }
[ -f keploy/e2e/mappings.yaml ] || { echo "FAIL: per-test mappings.yaml not written"; FAIL=1; }
# Begins are kept only under the switch (step 6).
grep -qE '^\s+at: ' keploy/e2e/mappings.yaml && { echo "FAIL: a record without the proven switch wrote test begins"; FAIL=1; }

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

echo "== 2a. the replay records each test's own verdict =="
for t in test_users test_items test_health; do
  got=$(outcome_of "$t")
  [ "$got" = "passed" ] || { echo "FAIL: the replay gives $t the verdict '$got', want passed"; FAIL=1; }
done
grep -qx '    passed: 3' keploy/e2e/last-replay.yaml || { echo "FAIL: the receipt does not count 3 passed tests"; FAIL=1; }
grep -qxF '/*/last-replay-tests.yaml' keploy/.gitignore || { echo "FAIL: keploy/.gitignore does not ignore the per-test results"; FAIL=1; }

echo "== 2b'. a replay that runs only some tests skips the rest and says so =="
sudo -E env PATH="$PATH" "$REPLAY_BIN" mock replay -c "python3 -m pytest -q -p no:cacheprovider -rs test_api.py" --name e2e --run-only test_users --disable-tele 2>&1 | tee gated.log
RC=${PIPESTATUS[0]}
[ "$RC" -eq 0 ] || { echo "FAIL: a gated replay whose run tests pass should exit 0, got $RC"; FAIL=1; }
grep -q "1 passed, 2 skipped" gated.log || { echo "FAIL: pytest did not skip the two gated tests"; FAIL=1; }
# pytest's own skip line (the CLI also logs the reason, so match pytest's).
grep -qE 'SKIPPED \[[0-9]+\] [^ ]+: not in --run-only' gated.log || { echo "FAIL: pytest did not skip with the gate's reason"; FAIL=1; }
grep -q '^    gateNote:' keploy/e2e/last-replay.yaml && { echo "FAIL: the receipt says the gate did not hold, though the harness skipped"; FAIL=1; }
for t in test_items test_health; do
  got=$(outcome_of "$t")
  [ "$got" = "gated" ] || { echo "FAIL: the replay gives gated $t the verdict '$got', want gated"; FAIL=1; }
done
[ "$(outcome_of test_users)" = "passed" ] || { echo "FAIL: the test the gate ran is not recorded as passed"; FAIL=1; }
grep -qx '    gated: 2' keploy/e2e/last-replay.yaml || { echo "FAIL: the receipt does not count 2 gated tests"; FAIL=1; }
grep -qx '    gateReason: not in --run-only' keploy/e2e/last-replay.yaml || { echo "FAIL: the receipt does not say why tests were gated"; FAIL=1; }

echo "== 2b''. a harness that cannot skip runs every test, and the replay says so =="
sudo -E env PATH="$PATH" KEPLOY_E2E_NO_SKIP=1 "$REPLAY_BIN" mock replay -c "python3 -m pytest -q -p no:cacheprovider test_api.py" --name e2e --run-only test_users --disable-tele 2>&1 | tee ungated.log
grep -q "3 passed" ungated.log || { echo "FAIL: a harness that cannot skip should have run all 3 tests"; FAIL=1; }
grep -q -- "--run-only was ignored, so all tests ran" ungated.log || { echo "FAIL: keploy did not say the run was not the subset asked for"; FAIL=1; }
grep -qx '    ungated: 2' keploy/e2e/last-replay.yaml || { echo "FAIL: the receipt does not count the 2 tests that ran outside the run list"; FAIL=1; }
grep -q '^    gateNote: ' keploy/e2e/last-replay.yaml || { echo "FAIL: the receipt does not say the gate did not hold"; FAIL=1; }

echo "== 1 and 2b' with no pid: the pid-less scope (KEPLOY_E2E_NO_PID) =="
# The scope API without the switch also takes a begin and an end with no pid:
# the optional single shared pool, which each test narrows in turn, where a
# pid narrows only that worker's view. The record and the gated replay above,
# again with a harness that sends no pid.
start_dep
sudo -E env PATH="$PATH" KEPLOY_E2E_NO_PID=1 "$RECORD_BIN" mock record -c "python3 -m pytest -q -p no:cacheprovider test_api.py" --name e2e-nopid --disable-tele 2>&1 | tee rec-nopid.log
RC=${PIPESTATUS[0]}
stop_dep
[ "$RC" -eq 0 ] && grep -q "3 passed" rec-nopid.log || { echo "FAIL: (no pid) the record should pass (exit 0, 3 passed), got $RC"; FAIL=1; }
MOCKS=$(grep -c '^kind:' keploy/e2e-nopid/mocks.yaml 2>/dev/null)
[ "${MOCKS:-0}" -eq 3 ] || { echo "FAIL: (no pid) expected 3 mocks, got ${MOCKS:-0}"; FAIL=1; }
[ -f keploy/e2e-nopid/mappings.yaml ] || { echo "FAIL: (no pid) per-test mappings.yaml not written"; FAIL=1; }
sudo -E env PATH="$PATH" KEPLOY_E2E_NO_PID=1 "$REPLAY_BIN" mock replay -c "python3 -m pytest -q -p no:cacheprovider test_api.py" --name e2e-nopid --disable-tele 2>&1 | tee rep-nopid.log
RC=${PIPESTATUS[0]}
[ "$RC" -eq 0 ] && grep -q "3 passed" rep-nopid.log || { echo "FAIL: (no pid) the replay should pass entirely from mocks (exit 0, 3 passed), got $RC"; FAIL=1; }
sudo -E env PATH="$PATH" KEPLOY_E2E_NO_PID=1 "$REPLAY_BIN" mock replay -c "python3 -m pytest -q -p no:cacheprovider -rs test_api.py" --name e2e-nopid --run-only test_users --disable-tele 2>&1 | tee gated-nopid.log
RC=${PIPESTATUS[0]}
[ "$RC" -eq 0 ] || { echo "FAIL: (no pid) a gated replay whose run tests pass should exit 0, got $RC"; FAIL=1; }
grep -q "1 passed, 2 skipped" gated-nopid.log || { echo "FAIL: (no pid) pytest did not skip the two gated tests"; FAIL=1; }
grep -qE 'SKIPPED \[[0-9]+\] [^ ]+: not in --run-only' gated-nopid.log || { echo "FAIL: (no pid) pytest did not skip with the gate's reason"; FAIL=1; }
grep -q '^    gateNote:' keploy/e2e-nopid/last-replay.yaml && { echo "FAIL: (no pid) the receipt says the gate did not hold, though the harness skipped"; FAIL=1; }
for t in test_items test_health; do
  got=$(outcome_of "$t" e2e-nopid)
  [ "$got" = "gated" ] || { echo "FAIL: (no pid) the replay gives gated $t the verdict '$got', want gated"; FAIL=1; }
done
[ "$(outcome_of test_users e2e-nopid)" = "passed" ] || { echo "FAIL: (no pid) the test the gate ran is not recorded as passed"; FAIL=1; }
grep -qx '    gated: 2' keploy/e2e-nopid/last-replay.yaml || { echo "FAIL: (no pid) the receipt does not count 2 gated tests"; FAIL=1; }
grep -qx '    gateReason: not in --run-only' keploy/e2e-nopid/last-replay.yaml || { echo "FAIL: (no pid) the receipt does not say why tests were gated"; FAIL=1; }

echo "== 2c. a replay that lets calls reach real services proves nothing =="
# --on-miss passthrough: any call the recording lacks goes to the real
# dependency, so whatever the tests say, the run did not prove isolation.
sudo -E env PATH="$PATH" "$REPLAY_BIN" mock replay -c "python3 -m pytest -q -p no:cacheprovider test_api.py" --name e2e --on-miss passthrough --disable-tele 2>&1 | tee passthrough.log
grep -qx 'isolated: false' keploy/e2e/last-replay.yaml || { echo "FAIL: a passthrough replay claims isolation"; FAIL=1; }
grep -q -- "--on-miss passthrough lets a call the recording does not have reach the real dependency" passthrough.log || { echo "FAIL: the passthrough run did not say what it failed to prove"; FAIL=1; }
# ...and the next proven replay puts the verdict back.
sudo -E env PATH="$PATH" "$REPLAY_BIN" mock replay -c "python3 -m pytest -q -p no:cacheprovider test_api.py" --name e2e --on-miss fail --disable-tele >/dev/null 2>&1
grep -qx 'isolated: true' keploy/e2e/last-replay.yaml || { echo "FAIL: a proven replay did not restore the verdict"; FAIL=1; }

echo "== 2d. a replay that serves NONE of the recorded calls verified nothing =="
# A passing runner that exercises none of the recording is the H2 false pass:
# nothing was served from the mocks, so a green exit would vouch for a run that
# proved nothing. test_noop makes no dependency call, so loaded=3 / consumed=0.
cat > test_noop.py <<PY
def test_noop(): assert True
PY
sudo -E env PATH="$PATH" "$REPLAY_BIN" mock replay -c "python3 -m pytest -q -p no:cacheprovider test_noop.py" --name e2e --disable-tele 2>&1 | tee nothing.log
RC=${PIPESTATUS[0]}
echo "nothing-served replay exit=$RC"
[ "$RC" -ne 0 ] || { echo "FAIL: a replay that served no recorded call must exit non-zero (nothing verified)"; FAIL=1; }
check_receipt_exit "$RC"
grep -qx 'failedBy: nothing-verified' keploy/e2e/last-replay.yaml || { echo "FAIL: the receipt does not say the run verified nothing"; FAIL=1; }
grep -qx 'consumed: 0' keploy/e2e/last-replay.yaml || { echo "FAIL: the receipt does not record zero consumed"; FAIL=1; }
grep -q "none of the 3 recorded calls was used by this run, so nothing was checked against the recording" nothing.log || { echo "FAIL: keploy did not say the run verified nothing"; FAIL=1; }
# A genuine proof (passed, no miss) must still exit 0: 'nothing verified' must
# not fire when a recorded call WAS served, or it would fail every real pass.
sudo -E env PATH="$PATH" "$REPLAY_BIN" mock replay -c "python3 -m pytest -q -p no:cacheprovider test_api.py" --name e2e --on-miss fail --disable-tele >/dev/null 2>&1
RC=$?
[ "$RC" -eq 0 ] || { echo "FAIL: a replay that served the recorded calls must still pass, got $RC"; FAIL=1; }

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
got=$(outcome_of test_boom)
[ "$got" = "failed" ] || { echo "FAIL: the receipt gives the failing test_boom the verdict '$got', want failed"; FAIL=1; }

echo "== 4. offline coverage and the --min-coverage gate =="
# The same suite, with pytest-cov writing coverage.xml. The dependency is still
# stopped, so this is the coverage of a run with every call replayed.
COV_CMD="python3 -m pytest -q -p no:cacheprovider --cov=client --cov-report=xml test_api.py"
rm -f coverage.xml
sudo -E env PATH="$PATH" "$REPLAY_BIN" mock replay -c "$COV_CMD" --name e2e --min-coverage 50 --disable-tele 2>&1 | tee cov-pass.log
RC=${PIPESTATUS[0]}
[ "$RC" -eq 0 ] || { echo "FAIL: replay above the coverage floor should pass, got $RC"; FAIL=1; }
grep -q "coverage with every outgoing call answered from the recording: " cov-pass.log || { echo "FAIL: keploy did not report offline coverage"; FAIL=1; }
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
grep -q "coverage with every outgoing call answered from the recording: .*report: reports/offline.xml" cov-named.log || { echo "FAIL: the named report was not reported as offline coverage"; FAIL=1; }
rm -f .coveragerc

# No report is not a pass: a floor that cannot measure must not let the run through.
sudo -E env PATH="$PATH" "$REPLAY_BIN" mock replay -c "python3 -m pytest -q -p no:cacheprovider test_api.py" --name e2e --min-coverage 10 --disable-tele 2>&1 | tee cov-none.log
RC=${PIPESTATUS[0]}
[ "$RC" -ne 0 ] || { echo "FAIL: --min-coverage with no coverage report must fail"; FAIL=1; }
grep -q "no coverage report to check" cov-none.log || { echo "FAIL: the missing report was not explained"; FAIL=1; }

echo "== 5. a suite that calls its own in-process server: said once, then counted =="
# test_self.py's test makes 5 requests, each to a server it starts in-process
# on a new port, as supertest's request(app) does: self-calls, which keploy
# passes through to that real handler. The run's first is logged at INFO, the
# rest at Debug, and their count when the run ends; one INFO line per
# connection would flood the log.
cat > test_self.py <<'PY'
import http.server, json, socketserver, threading, urllib.request
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = json.dumps({"path": self.path}).encode()
        self.send_response(200); self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def log_message(self, *a): pass
def own_server(path):
    # TCPServer, not HTTPServer: no name lookup while keploy answers DNS.
    # The timeout and the daemon thread let a self-call keploy does not pass
    # through fail the test instead of hanging the run.
    s = socketserver.TCPServer(("127.0.0.1", 0), H)
    s.timeout = 5
    t = threading.Thread(target=s.handle_request, daemon=True); t.start()
    try:
        with urllib.request.urlopen(f"http://127.0.0.1:{s.server_address[1]}{path}", timeout=5) as r:
            return json.load(r)
    finally:
        t.join(6); s.server_close()
def test_self_calls():
    for i in range(5):
        assert own_server(f"/self/{i}")["path"] == f"/self/{i}"
PY
sudo -E env PATH="$PATH" "$REPLAY_BIN" mock replay -c "python3 -m pytest -q -p no:cacheprovider test_api.py test_self.py" --name e2e --disable-tele 2>&1 | tee self.log
RC=${PIPESTATUS[0]}
[ "$RC" -eq 0 ] && grep -q "4 passed" self.log || { echo "FAIL: the suite with self-calls should pass (exit 0, 4 passed), got $RC"; FAIL=1; }
SAID=$(grep -c "keploy: self-call passed through to the run's own listener" self.log)
[ "$SAID" -eq 1 ] || { echo "FAIL: 5 self-calls were said at INFO $SAID times, want once"; FAIL=1; }
grep -q "keploy: 5 self-calls passed through to the run's own listeners" self.log ||
  { echo "FAIL: the run's 5 self-calls were not counted when it ended"; FAIL=1; }

fi

if half on; then
echo "== 6. under the proven switch, a record keeps when each test began =="
# The agent moves the run's clock to each test's recorded begin while the test
# is open (design v14 §3.4); a build that freezes the clock does the moving.
# Under the switch a replay needs a build that judges each test (keploy
# enterprise), so here the begins are checked on disk; the enterprise UM lane
# (mock-local-loop-e2e) replays them on a frozen clock.
start_dep
sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$RECORD_BIN" mock record -c "python3 -m pytest -q -p no:cacheprovider test_api.py" --name e2e-proven --disable-tele 2>&1 | tee rec-proven.log
RC=${PIPESTATUS[0]}
stop_dep
[ "$RC" -eq 0 ] && grep -q "3 passed" rec-proven.log || { echo "FAIL: under the switch the record should pass (exit 0, 3 passed), got $RC"; FAIL=1; }
MOCKS=$(grep -c '^kind:' keploy/e2e-proven/mocks.yaml 2>/dev/null)
[ "${MOCKS:-0}" -eq 3 ] || { echo "FAIL: under the switch, expected 3 mocks, got ${MOCKS:-0}"; FAIL=1; }
# Each test's ID names its file, and its mapping its own call's mock.
python3 - <<'PY' || FAIL=1
import datetime, re, sys
t = open("keploy/e2e-proven/mappings.yaml").read()
begins, mocks = {}, {}
for block in re.split(r"\n(?=\s*- id: )", t):
    m = re.search(r"- id: (\S+)", block)
    a = re.search(r"\n\s+at: (\S+)", block)
    if m:
        mocks[m.group(1)] = re.findall(r"- name: (\S+)", block)
    if m and a:
        begins[m.group(1)] = datetime.datetime.fromisoformat(a.group(1).replace("Z", "+00:00"))
print("recorded begins:", begins)
print("recorded mappings:", mocks)
want = ["test_api.py::test_users", "test_api.py::test_items", "test_api.py::test_health"]
if set(begins) != set(want):
    sys.exit(f"FAIL: want a recorded begin for each of {sorted(want)}, got {sorted(begins)}")
order = [n for n, _ in sorted(begins.items(), key=lambda kv: kv[1])]
if order != want:
    sys.exit(f"FAIL: the begins are not in the order the tests ran: {order}")
if sorted(len(mocks.get(n, [])) for n in want) != [1, 1, 1] or len({m for n in want for m in mocks[n]}) != 3:
    sys.exit(f"FAIL: want each test mapped to its own one mock, got {mocks}")
PY
# The begins change nothing without the switch: the recording replays as
# before, and nothing is sent.
sudo -E env PATH="$PATH" "$REPLAY_BIN" mock replay -c "python3 -m pytest -q -p no:cacheprovider test_api.py" --name e2e-proven --disable-tele 2>&1 | tee rep-proven-off.log
RC=${PIPESTATUS[0]}
[ "$RC" -eq 0 ] && grep -q "3 passed" rep-proven-off.log || { echo "FAIL: without the switch the recording with begins should replay as before (exit 0, 3 passed), got $RC"; FAIL=1; }
grep -q 'sent the agent when each test began' rep-proven-off.log && { echo "FAIL: a replay without the proven switch sent the agent the tests' begins"; FAIL=1; }

echo "== 7. under the proven switch, this build refuses a replay before it runs the test command =="
# Its exit code would be only the test command's, so a test keploy failed after
# it ended, or a call made outside every test with no recorded answer, would
# pass: the replay needs a build that judges each test (keploy enterprise,
# whose UM lane replays under the switch). Refused at once, the test command
# never started, and the receipt says so instead of standing green.
rm -f ran.txt
sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$REPLAY_BIN" mock replay -c "sh -c 'echo ran > ran.txt; python3 -m pytest -q -p no:cacheprovider test_api.py'" --name e2e-proven --disable-tele 2>&1 | tee rep-proven.log
RC=${PIPESTATUS[0]}
[ "$RC" -eq 1 ] || { echo "FAIL: under the switch this build should refuse the replay with exit 1, got $RC"; FAIL=1; }
grep -q "keploy mock replay under KEPLOY_PROVEN needs a keploy build that judges each test (keploy enterprise)" rep-proven.log ||
  { echo "FAIL: the refused replay does not say it needs a build that judges each test"; FAIL=1; }
[ -e ran.txt ] && { echo "FAIL: under the switch the refused replay ran the test command"; FAIL=1; }
R=keploy/e2e-proven/last-replay.yaml
grep -qx 'exitCode: 1' "$R" || { echo "FAIL: the refused replay's receipt does not say exitCode 1"; FAIL=1; }
grep -qx 'failedBy: setup' "$R" || { echo "FAIL: the refused replay's receipt does not blame its setup"; FAIL=1; }
grep -qx 'isolationNote: the test command never ran' "$R" || { echo "FAIL: the refused replay's receipt does not say the test command never ran"; FAIL=1; }
grep -qx 'isolated: false' "$R" || { echo "FAIL: the refused replay's receipt claims isolation"; FAIL=1; }
fi

if [ "$FAIL" -eq 0 ]; then echo "MOCK E2E: PASSED"; else echo "MOCK E2E: FAILED"; fi
exit "$FAIL"
