#!/usr/bin/env bash
# E2E: a test variable followed through dependency calls (the agent side of
# baked variables), under keploy record / keploy test, with KEPLOY_PROVEN=1.
#
# The app mints an order id on every create (a random hex word, no UUID: what
# is followed never depends on what a value looks like), sends it to an HTTP
# dependency (POST /orders), then reads the order back from the dependency
# twice by that id (GET /orders/<id>: "new", then "ready"), and answers the
# two states and whether the dependency named the same id each time. The
# answer holds no id, so the test case compares as recorded; the id lives only
# in the dependency calls. Replayed with the dependency down, the app mints
# another id, so the recorded calls do not match as recorded.
#
# The learn step (row L) writes, on each mock, where the variable is:
#   POST /orders   orderId: bound at body.id,    answered at body.id
#   GET  /orders/x orderId: used at url.path[1], answered at body.id
# This script writes that block itself, as L would (L is built in parallel).
#
#   1. record two creates (two test cases, each its own id).
#   2. KEPLOY_PROVEN=1 keploy test without the block: the set fails (the calls
#      carry a new id; a miss fails its test).
#   3. write the variables block on the six mocks; the recorded values stay as
#      recorded (mocks are byte-literal).
#   4. KEPLOY_PROVEN=1 keploy test: the set passes. Each test case binds
#      orderId where its create sends it, its two read-backs are asserted to
#      ask for that id and are answered in record order (new, then ready), and
#      every answer names it. keploy test leaves the mocks as they were.
#   5. the app reads back ANOTHER id than the one it made: the set fails, and
#      the report names the variable, the place, the value bound this run and
#      the one the call sent (match_phase variable_mismatch, kind variable).
#   6. the app reads back the RECORDED id (a stale constant): the set fails.
#   7. the app reads back without creating first: the set fails; the report
#      says the variable was used before the call that binds it.
#   8. the app sends the id as a number: the set fails; the report says the
#      value is of another JSON type.
#   9. switch off: the block is not read; the set gets the verdicts it gets
#      without the block, and no report mentions a variable.
#  10. no ERROR in the passing run's log, no data race in any log.
#
# Uses ports 18931 (app) and 18932 (dependency) on 127.0.0.1. Env: RECORD_BIN
# and REPLAY_BIN. The script stops only the processes it started (by PID).
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-$RECORD_BIN}"
WORK="$(mktemp -d)"
echo "work dir: $WORK"
cd "$WORK" || exit 1
APP=18931
DEP=18932
FAIL=0
fail() {
  echo "FAIL: $*"
  FAIL=$((FAIL + 1))
}

cat > dep.py <<'PY'
import http.server, json, socketserver, sys
PORT = int(sys.argv[1]); orders = {}
class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def _send(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def do_POST(self):
        o = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        orders[str(o["id"])] = o
        self._send(201, {"id": o["id"], "item": o["item"]})
    def do_GET(self):
        oid = self.path.rsplit("/", 1)[-1]
        if oid not in orders: return self._send(404, {"error": "no such order"})
        o = orders[oid]; o["reads"] = o.get("reads", 0) + 1
        self._send(200, {"id": oid, "item": o["item"], "status": "new" if o["reads"] == 1 else "ready"})
    def log_message(self, *a): pass
socketserver.ThreadingTCPServer.allow_reuse_address = True
with socketserver.ThreadingTCPServer(("127.0.0.1", PORT), H) as s: s.serve_forever()
PY

cat > app.py <<'PY'
import http.server, json, secrets, socketserver, sys, urllib.error, urllib.request
PORT = int(sys.argv[1]); DEP = "http://127.0.0.1:%s" % sys.argv[2]
# A regression to replay with. "wrong-read": the read-backs ask the dependency
# for another id than the one the create sent. "stale-read:<id>": they ask for
# <id> (the recorded one). "no-create": the create is never sent.
# "number-id": the create sends the id as a JSON number.
MODE = sys.argv[3] if len(sys.argv) > 3 else ""
def dep(method, path, obj=None):
    data = json.dumps(obj).encode() if obj is not None else None
    hdrs = {"Content-Type": "application/json"}
    r = urllib.request.Request(DEP + path, data=data, method=method, headers=hdrs)
    with urllib.request.urlopen(r, timeout=10) as resp: return json.loads(resp.read())
class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def _send(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def do_POST(self):
        item = json.loads(self.rfile.read(int(self.headers.get("Content-Length") or 0)) or b"{}").get("item", "pen")
        oid = "o" + secrets.token_hex(6)  # a new id on every run
        sent = int(oid[1:], 16) if MODE == "number-id" else oid
        asked = oid
        if MODE == "wrong-read": asked = "o" + secrets.token_hex(6)
        if MODE.startswith("stale-read:"): asked = MODE.split(":", 1)[1]
        try:
            made = {"id": oid} if MODE == "no-create" else dep("POST", "/orders", {"id": sent, "item": item})
            reads = [dep("GET", "/orders/" + asked) for _ in range(2)]
        except urllib.error.HTTPError as e:
            return self._send(502, {"error": "the dependency answered %d" % e.code})
        except Exception as e:
            return self._send(502, {"error": str(e)})
        self._send(201, {"item": item, "statuses": [r.get("status") for r in reads],
                         "created_same_id": made.get("id") == sent,
                         "read_same_id": all(r.get("id") == oid for r in reads)})
    def log_message(self, *a): pass
socketserver.ThreadingTCPServer.allow_reuse_address = True
with socketserver.ThreadingTCPServer(("127.0.0.1", PORT), H) as s: s.serve_forever()
PY

# declare.py MOCKS: writes the variables block on the recorded calls, as the
# learn step would: the create binds orderId where it sends it, each read-back
# uses it in its URL, and every answer names it. The recorded bytes are kept.
cat > declare.py <<'PY'
import re, sys
path = sys.argv[1]
docs = open(path).read().split("\n---\n")
out, binds, uses, recorded = [], 0, 0, []
for d in docs:
    if not re.search(r"^kind: Http$", d, re.M):
        out.append(d); continue
    m = re.search(r"\n\s+method: (\w+)\n(?:.*\n)*?\s+url: (\S+)", d)
    if not m:
        out.append(d); continue
    method, url = m.group(1), m.group(2)
    if method == "POST" and url.endswith("/orders"):
        rid = re.search(r'\\"id\\": ?\\"(o[0-9a-f]+)\\"|"id": ?"(o[0-9a-f]+)"', d)
        rid = rid.group(1) or rid.group(2)
        d = d.rstrip("\n") + "\nvariables:\n    orderId:\n        recorded: %s\n        bind: body.id\n        answer:\n            - body.id\n" % rid
        binds += 1; recorded.append(rid)
    elif method == "GET" and "/orders/" in url:
        rid = url.rsplit("/", 1)[-1]
        d = d.rstrip("\n") + "\nvariables:\n    orderId:\n        recorded: %s\n        use:\n            - url.path[1]\n        answer:\n            - body.id\n" % rid
        uses += 1
    out.append(d)
if binds != 2 or uses != 4:
    sys.exit("found %d creates and %d read-backs, want 2 and 4" % (binds, uses))
open(path, "w").write("\n---\n".join(out))
print(recorded[0])
PY

listening() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
# wait_for_port PORT SECONDS: until something listens there; the bound is a
# safety net for a run that hangs.
wait_for_port() {
  local i
  for ((i = 0; i < $2 * 5; i++)); do
    listening "$1" && return 0
    sleep 0.2
  done
  echo "nothing listened on 127.0.0.1:$1 within $2s"
  return 1
}
test_files() { find keploy/test-set-0/tests -maxdepth 1 -name '*.yaml' -size +0c 2>/dev/null | wc -l; }
wait_for_tests() {
  local i
  for ((i = 0; i < 360 * 5; i++)); do
    [ "$(test_files)" -ge "$1" ] && return 0
    sleep 0.2
  done
  return 1
}
own() { sudo chown -R "$(id -u):$(id -g)" . 2>/dev/null || true; }
# stop_pid PID: stop a keploy this script started under sudo (PID is sudo's)
# by interrupting keploy itself, sudo's child, then wait for it to exit.
# Nothing is looked up by name: other keploy processes on the host are left
# alone.
stop_pid() {
  local sudo_pid="$1" child
  child="$(pgrep -P "$sudo_pid" 2>/dev/null | head -n1)"
  [ -n "$child" ] && sudo kill -INT "$child" 2>/dev/null
  wait "$sudo_pid" 2>/dev/null
}
newest_run() { ls -1d keploy/reports/test-run-* 2>/dev/null | sort -t- -k3 -n | tail -n1; }
REPORT() { echo "$(newest_run)/test-set-0-report.yaml"; }
set_status() { grep -m1 '^status:' "$(REPORT)" 2>/dev/null | awk '{print $2}'; }
# case_statuses: each test case of the newest report and its status, by test
# case (a test case's own status is the first status line of its block).
case_statuses() {
  awk '/^ *- kind:/{st=""} /^ *status:/{ if (st=="") st=$2 } /test_case_id:/{ print $2, st }' "$(REPORT)" 2>/dev/null | sort
}
# replay LOG MODE [VAR=VALUE...]: keploy test with the dependency down; RC is
# its exit code. A run that wrote no report of its own fails the script: every
# check after it would otherwise read the previous run's report.
replay() {
  local log="$1" mode="$2" before; shift 2
  before="$(newest_run)"
  sudo -E env PATH="$PATH" "$@" "$REPLAY_BIN" test -c "python3 app.py $APP $DEP${mode:+ $mode}" --delay 5 \
    --generateGithubActions=false --disable-tele >"$log.log" 2>&1 </dev/null
  RC=$?
  own
  if [ "$(newest_run)" = "$before" ] || [ ! -f "$(REPORT)" ]; then
    fail "$log: keploy test wrote no report (exit $RC)"; tail -30 "$log.log"
  fi
}
expect_rc() { [ "$RC" = "$1" ] || fail "$2: keploy exited $RC, want $1"; }
expect_failed_with() { # STEP WHAT: the newest set failed, its report holding WHAT
  [ "$(set_status)" = FAILED ] || fail "$1: the set must fail (status: $(set_status))"
  grep -q -- "$2" "$(REPORT)" || { fail "$1: the report does not say: $2"; grep -nE 'match_phase|diff:|kind:' "$(REPORT)" | head -20; }
}

for p in "$APP" "$DEP"; do
  if listening "$p"; then echo "port $p is in use"; exit 1; fi
done
python3 dep.py "$DEP" >dep.log 2>&1 &
DEP_PID=$!
REC_PID=""
cleanup() {
  [ -n "$REC_PID" ] && stop_pid "$REC_PID"
  kill "$DEP_PID" 2>/dev/null
}
trap cleanup EXIT
wait_for_port "$DEP" 30 || { echo "FAIL: the dependency did not start"; exit 1; }

echo "== 1. record two creates =="
sudo -E env PATH="$PATH" "$RECORD_BIN" record -c "python3 app.py $APP $DEP" --generateGithubActions=false --disable-tele >record.log 2>&1 </dev/null &
REC_PID=$!
wait_for_port "$APP" 360 || { echo "FAIL: the app did not start under keploy record"; tail -30 record.log; exit 1; }
for item in pen book; do
  out="$(curl -s -XPOST "http://127.0.0.1:$APP/orders" -H 'Content-Type: application/json' -d "{\"item\":\"$item\"}")"
  echo "$out" | grep -q '"statuses": \["new", "ready"\]' && echo "$out" | grep -q '"read_same_id": true' ||
    fail "the create of $item answered '$out'"
done
wait_for_tests 2 || fail "keploy record wrote $(test_files) test cases, want 2"
stop_pid "$REC_PID"; REC_PID=""; own
kill "$DEP_PID" 2>/dev/null; wait "$DEP_PID" 2>/dev/null
[ "$(test_files)" -eq 2 ] || { fail "recorded $(test_files) test cases, want 2"; tail -30 record.log; exit 1; }
MOCKS=keploy/test-set-0/mocks.yaml
[ "$(grep -c '^kind: Http$' "$MOCKS" 2>/dev/null)" -ge 6 ] || { fail "the six dependency calls were not recorded as mocks"; tail -30 record.log; exit 1; }
cp -a keploy keploy.plain

echo "== 2. KEPLOY_PROVEN=1 keploy test without the variables: the set fails =="
replay plain "" KEPLOY_PROVEN=1
expect_rc 1 "step 2"
[ "$(set_status)" = FAILED ] || fail "without the variables the set must fail: the creates send new ids (status: $(set_status))"
grep -q 'variable_mismatch' "$(REPORT)" && fail "step 2: a variable was reported in a set that declares none"

echo "== 3. write the variables on the mocks, as the learn step would =="
REC_ID="$(python3 declare.py "$MOCKS")" || fail "could not write the variables block: $REC_ID"
cp "$MOCKS" mocks.declared.yaml

echo "== 4. KEPLOY_PROVEN=1 keploy test: the set passes =="
replay follow "" KEPLOY_PROVEN=1
expect_rc 0 "step 4"
[ "$(set_status)" = PASSED ] || { fail "with the variables the set must pass (status: $(set_status))"; grep -nE 'status:|match_phase|diff:|next_steps' "$(REPORT)" | head -40; tail -40 follow.log; }
cmp -s "$MOCKS" mocks.declared.yaml || fail "keploy test rewrote the mocks"
grep -q ' ERROR ' follow.log && { fail "the passing run logged an ERROR"; grep ' ERROR ' follow.log | head; }

echo "== 5. the app reads back another id: the set fails with the variable as evidence =="
replay wrong wrong-read KEPLOY_PROVEN=1
expect_rc 1 "step 5"
expect_failed_with "step 5" 'match_phase: variable_mismatch'
expect_failed_with "step 5" 'kind: variable'
expect_failed_with "step 5" 'orderId'
expect_failed_with "step 5" 'url.path\[1\]'

echo "== 6. the app reads back the recorded id: the set fails =="
replay stale "stale-read:$REC_ID" KEPLOY_PROVEN=1
expect_rc 1 "step 6"
expect_failed_with "step 6" "actual: $REC_ID"

echo "== 7. the app reads back without creating: the set fails, used before bound =="
replay nocreate no-create KEPLOY_PROVEN=1
expect_rc 1 "step 7"
expect_failed_with "step 7" 'before the call that binds it'

echo "== 8. the app sends the id as a number: the set fails, another JSON type =="
replay numberid number-id KEPLOY_PROVEN=1
expect_rc 1 "step 8"
expect_failed_with "step 8" 'another JSON type'

echo "== 9. switch off: the block is not read =="
replay off ""
OFF="$(case_statuses)"
[ "$(echo "$OFF" | grep -c .)" -eq 2 ] || fail "switch off: the report holds $(echo "$OFF" | grep -c .) test cases, want 2"
grep -q 'variable_mismatch\|kind: variable' "$(REPORT)" && fail "switch off: a report mentions a variable"
cp keploy.plain/test-set-0/mocks.yaml "$MOCKS"
replay offplain ""
OFFPLAIN="$(case_statuses)"
[ "$OFF" = "$OFFPLAIN" ] || fail "switch off: the verdicts with the block ($OFF) differ from those without it ($OFFPLAIN)"

echo "== 10. no data race =="
for f in record.log plain.log follow.log wrong.log stale.log nocreate.log numberid.log off.log offplain.log; do
  grep -q 'WARNING: DATA RACE' "$f" 2>/dev/null && fail "data race in $f"
done

if [ "$FAIL" -ne 0 ]; then
  echo "VARIABLES E2E: FAILED ($FAIL)"
  exit 1
fi
echo "VARIABLES E2E: PASSED"
