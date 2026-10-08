#!/usr/bin/env bash
# End-to-end guard for VALUE REBINDING in `keploy mock replay`: a test that
# mints its own id (uuid4), creates an entity with it at a dependency and reads
# it back by that id must pass under replay, though every recording carries
# the id minted at record time.
#
# The driver mints a fresh uuid each run, POSTs it, GETs /items/<uuid>, and
# asserts the dependency answered with ITS id both times. Recorded with the
# dependency up, replayed with it DOWN:
#   - the POST is the recording's first carrier of the recorded id: keploy
#     binds recorded -> live there;
#   - the GET by the live id is matched against the recording of the GET by
#     the recorded one, and answered with the live id.
# Without rebinding the read-back is served the recorded id and the driver's
# assertion fails. Two creates with identical payloads must each bind their
# own recording, though each request also carries its own trace header.
# Uses the PR `build` binary for record and replay (mock_linux.yml).
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-keploy}"
WORK="$(mktemp -d)"
cd "$WORK"
PORT=18878
FAIL=0

cat > dep.py <<'PY'
import http.server, json, socketserver, sys
PORT = int(sys.argv[1]); items = {}; seq = [0]
class H(http.server.BaseHTTPRequestHandler):
    def _send(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def do_POST(self):
        item = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        seq[0] += 1
        item["seq"] = seq[0]  # the dependency's own number for the item
        items[item["id"]] = item
        self._send(201, item)
    def do_GET(self):
        iid = self.path.rsplit("/", 1)[-1]
        if iid in items: self._send(200, dict(items[iid], stock=5))
        else: self._send(404, {"error": "no such item"})
    def log_message(self, *a): pass
socketserver.TCPServer.allow_reuse_address = True
with socketserver.TCPServer(("127.0.0.1", PORT), H) as s: s.serve_forever()
PY

# The test under replay: mints its ids, creates, reads back. Every call carries
# its own W3C trace header, as an instrumented app's do: a per-request value no
# match can bind, which must not stop each create binding its own recording.
cat > flow.py <<'PY'
import json, os, sys, time, urllib.request, uuid
BASE = "http://127.0.0.1:%s" % sys.argv[1]
def call(method, path, obj=None):
    data = json.dumps(obj).encode() if obj is not None else None
    headers = {"traceparent": "00-%s-%s-01" % (os.urandom(16).hex(), os.urandom(8).hex())}
    if data:
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(BASE + path, data=data, method=method, headers=headers)
    return json.loads(urllib.request.urlopen(req, timeout=5).read())
time.sleep(3)  # let keploy warm up eBPF redirect so the FIRST call is intercepted
# Two creates with IDENTICAL payloads, differing only in the id the test mints:
# each must bind its own recording, so each read-back is its own item (the
# dependency's seq tells them apart).
made = []
for _ in range(2):
    iid = str(uuid.uuid4())
    created = call("POST", "/items", {"id": iid, "name": "widget"})
    assert created["id"] == iid, "create answered %r for %s" % (created["id"], iid)
    made.append((iid, created["seq"]))
for iid, seq in made:
    got = call("GET", "/items/" + iid)
    assert got["id"] == iid and got["seq"] == seq, "read-back answered %r for %s (seq %d)" % (got, iid, seq)
print("FLOW_OK", flush=True)
PY

start_dep() { python3 dep.py "$PORT" >dep.log 2>&1 & echo $!; }
DP=$(start_dep); sleep 1
trap 'kill "$DP" 2>/dev/null' EXIT  # however this script ends, no dependency is left behind

echo "== 1. record: two creates, each read back by its id =="
sudo -E env PATH="$PATH" "$RECORD_BIN" mock record -c "python3 flow.py $PORT" --name rb --disable-tele 2>&1 | tee rec.log
grep -q "FLOW_OK" rec.log || { echo "FAIL: the flow did not pass against the real dependency"; FAIL=1; }
kill "$DP" 2>/dev/null; sleep 1

echo "== 2. replay with the dependency DOWN: fresh ids must be answered with themselves =="
sudo -E env PATH="$PATH" "$REPLAY_BIN" mock replay -c "python3 flow.py $PORT" --name rb --disable-tele 2>&1 | tee rep.log
RC=${PIPESTATUS[0]}
grep -q "FLOW_OK" rep.log || { echo "FAIL: the create-then-read-back flow failed under replay (exit $RC): the read-back was not answered with the id the test minted"; FAIL=1; }
[ "$RC" -eq 0 ] || { echo "FAIL: replay exited $RC"; FAIL=1; }

echo "== 3. with rebinding off, the same replay serves the recorded ids (the guard is load-bearing) =="
cat > keploy.yml <<'YML'
test:
  disableMockRebinding: true
YML
sudo -E env PATH="$PATH" "$REPLAY_BIN" mock replay -c "python3 flow.py $PORT" --name rb --disable-tele 2>&1 | tee rep-off.log
grep -q "FLOW_OK" rep-off.log && { echo "FAIL: the flow passed with rebinding off, so step 2 proves nothing"; FAIL=1; }
grep -q "read-back answered" rep-off.log || { echo "FAIL: with rebinding off the flow must fail at the read-back, not elsewhere (see rep-off.log)"; FAIL=1; }
rm -f keploy.yml

# The PR's race-enabled build: a data race anywhere in a run fails the test.
if grep -l "WARNING: DATA RACE" ./*.log >/dev/null 2>&1; then
  echo "FAIL: a data race was reported in: $(grep -l "WARNING: DATA RACE" ./*.log | tr '\n' ' ')"; FAIL=1
fi

if [ "$FAIL" = 0 ]; then
  echo "MOCK REBIND E2E: PASSED"
  cd / && sudo rm -rf "$WORK"
else
  echo "MOCK REBIND E2E: FAILED (work directory kept: $WORK)"
fi
exit $FAIL
