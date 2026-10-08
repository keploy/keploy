#!/usr/bin/env bash
# End-to-end guard for ids an app mints, under `keploy record` / `keploy test`:
# on the test-case side (the requests keploy replays and the answers it
# expects) and the mock side (the app's dependency calls) together.
#
# The app mints an order id on POST /orders and stores the order at an HTTP
# dependency; GET /orders/<id> and POST /orders/lookup (a form: order=<id>)
# read it back from there. On every run the app mints a NEW id, so under
# replay, with the dependency down:
#   - the create's expected response names the recorded id, and the app
#     answers with the one it just minted;
#   - the two read-backs ask for the recorded id, which the app now knows
#     under the new one, and the dependency mocks were recorded with the old
#     one.
# `keploy templatize` finds that a response produced the id and a later
# request reuses it, and puts it in the set's template map. keploy then binds
# the recorded id to the new one where the app first sends it to the
# dependency, and from then on it is the new id everywhere: in the mocks'
# answers, in the next test case's request, and in what each test case
# expects — also where templatize left the id written out (the form body, the
# expected responses), which keploy's own placeholders do not reach.
#
#   1. record the three test cases with the dependency up.
#   2. `keploy test` before templatize: nothing is followed, and the create
#      fails on its id — as it always has. A value is never followed just
#      because it differs.
#   3. `keploy templatize`, then `keploy test`: every test case passes, the
#      report says which ids were swapped, and both read-backs were sent with
#      the id minted this run.
#   4. `keploy test --update-template` passes and leaves the RECORDED id in
#      the set's templates, so the next run still follows it and passes.
#   5. the app answers the read-backs with ANOTHER id than the one it made:
#      the set fails. A followed id is still asserted — in a templated set
#      keploy otherwise takes whatever the app answers in a template's place.
#   6. the app asks its dependency for an id nobody made: the set fails. The
#      closest recording answers such a call as recorded, never renamed to
#      this run's id, and the agent says so.
#   7. with rebinding off the templatized set fails (step 3 proves something).
#   8. the app's answers change in a field that is no id — the create's too,
#      the test case that produces the id — and `keploy normalize` accepts
#      them: the test cases get the RECORDED id back, not the one made by the
#      run they were taken from, so the next run still follows it and passes.
#   9. the app stops storing the order at its dependency, so there is nothing
#      to tie its new id to: the set fails as it would with rebinding off, and
#      keploy says which ids the failed test cases name that it did not
#      follow.
# Uses the PR `build` binary for record and replay (mock_linux.yml).
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-keploy}"
WORK="$(mktemp -d)"
cd "$WORK"
APP=18911
DEP=18912
FAIL=0

cat > dep.py <<'PY'
import http.server, json, socketserver, sys
PORT = int(sys.argv[1]); items = {}; seq = [0]
class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def _send(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def do_POST(self):
        item = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        seq[0] += 1; item["seq"] = seq[0]; items[item["id"]] = item
        self._send(201, item)
    def do_GET(self):
        iid = self.path.rsplit("/", 1)[-1]
        if iid in items: self._send(200, dict(items[iid], stock=5))
        else: self._send(404, {"error": "no such item"})
    def log_message(self, *a): pass
socketserver.ThreadingTCPServer.allow_reuse_address = True
with socketserver.ThreadingTCPServer(("127.0.0.1", PORT), H) as s: s.serve_forever()
PY

# The app under test.
cat > app.py <<'PY'
import http.server, json, socketserver, sys, urllib.parse, urllib.request, urllib.error, uuid
PORT = int(sys.argv[1]); DEP = "http://127.0.0.1:%s" % sys.argv[2]
# A regression to replay with: "wrong-id" answers read-backs with an id the app
# did not make; "wrong-dep-id" asks the dependency for one; "no-store" creates
# an order without storing it at the dependency. "shape-v2" is a change to
# accept: every answer has another value in a field that is no id.
BUG = sys.argv[3] if len(sys.argv) > 3 else ""
SHAPE = "v2" if BUG == "shape-v2" else "v1"
def dep(method, path, obj=None):
    data = json.dumps(obj).encode() if obj is not None else None
    req = urllib.request.Request(DEP + path, data=data, method=method, headers={"Content-Type": "application/json"} if data else {})
    with urllib.request.urlopen(req, timeout=5) as r: return json.loads(r.read())
class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def _send(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n)
        if self.path.endswith("/lookup"):  # a form: order=<id>
            return self._read(urllib.parse.parse_qs(raw.decode()).get("order", [""])[0])
        body = json.loads(raw or b"{}")
        oid = str(uuid.uuid4())  # a new id on every run
        if BUG == "no-store": return self._send(201, {"id": oid, "seq": 0, "stored_id": oid, "kind": SHAPE})
        try: item = dep("POST", "/items", {"id": oid, "name": body.get("name", "widget")})
        except urllib.error.HTTPError as e: return self._send(502, {"error": "dependency said %d" % e.code})
        self._send(201, {"id": oid, "seq": item["seq"], "stored_id": item["id"], "kind": SHAPE})
    def do_GET(self):
        self._read(self.path.rsplit("/", 1)[-1])
    def _read(self, oid):
        try: item = dep("GET", "/items/" + (str(uuid.uuid4()) if BUG == "wrong-dep-id" else oid))
        except urllib.error.HTTPError as e: return self._send(502, {"error": "dependency said %d" % e.code})
        self._send(200, {"asked": str(uuid.uuid4()) if BUG == "wrong-id" else oid, "item": item, "shape": SHAPE})
    def log_message(self, *a): pass
socketserver.ThreadingTCPServer.allow_reuse_address = True
with socketserver.ThreadingTCPServer(("127.0.0.1", PORT), H) as s: s.serve_forever()
PY

listening() { ss -ltn 2>/dev/null | grep -q ":$1 "; }
wait_listening() { for _ in $(seq 1 60); do listening "$1" && return 0; sleep 1; done; return 1; }
stop_keploy() {
  local name; name="$(basename "$1")"
  sudo pkill -INT -x "$name" 2>/dev/null
  for _ in $(seq 1 30); do pgrep -x "$name" >/dev/null || break; sleep 1; done
  sudo pkill -9 -x "$name" 2>/dev/null
  sudo pkill -f "app.py $APP" 2>/dev/null
  for _ in $(seq 1 10); do listening "$APP" || break; sleep 1; done
}
own() { sudo chown -R "$(id -u):$(id -g)" . 2>/dev/null || true; }
# newest_run is the newest test run's report directory; run_passed, whether
# every test-set report in it says PASSED.
newest_run() { ls -1d keploy/reports/test-run-* 2>/dev/null | sort -t- -k3 -n | tail -n1; }
run_passed() {
  local run; run="$(newest_run)"
  [ -n "$run" ] || return 1
  local reports; reports=$(ls "$run"/test-set-*-report.yaml 2>/dev/null) || return 1
  [ -n "$reports" ] || return 1
  for f in $reports; do [ "$(grep -m1 '^status:' "$f" | awk '{print $2}')" = PASSED ] || return 1; done
}
replay() { # $1: log file, $2: extra argument for the app, $3: extra keploy flag (both optional)
  sudo -E env PATH="$PATH" "$REPLAY_BIN" test -c "python3 app.py $APP $DEP${2:+ $2}" --delay 8 --generateGithubActions=false ${3:-} 2>&1 | tee "$1" >/dev/null
  stop_keploy "$REPLAY_BIN"; own
}

python3 dep.py "$DEP" >dep.log 2>&1 &
DP=$!
# However this script ends, it leaves no dependency, keploy or app behind.
trap 'kill "$DP" 2>/dev/null; stop_keploy "$RECORD_BIN" >/dev/null 2>&1' EXIT
wait_listening "$DEP" || { echo "FAIL: the dependency did not start"; exit 1; }

echo "== 1. record: create an order, read it back =="
sudo -E env PATH="$PATH" "$RECORD_BIN" record -c "python3 app.py $APP $DEP" --generateGithubActions=false >rec.log 2>&1 &
wait_listening "$APP" || { echo "FAIL: the app did not start under keploy record"; cat rec.log; exit 1; }
sleep 8  # let keploy's hooks settle so the first call is captured
CREATED="$(curl -s -XPOST "http://127.0.0.1:$APP/orders" -H 'Content-Type: application/json' -d '{"name":"widget"}')"
RECORDED_ID="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["id"])' "$CREATED" 2>/dev/null)"
[ -n "$RECORDED_ID" ] || { echo "FAIL: the create answered '$CREATED'"; FAIL=1; }
READ="$(curl -s "http://127.0.0.1:$APP/orders/$RECORDED_ID")"
echo "$READ" | grep -q "\"id\": \"$RECORDED_ID\"" || { echo "FAIL: the read-back answered '$READ'"; FAIL=1; }
LOOKUP="$(curl -s -XPOST "http://127.0.0.1:$APP/orders/lookup" -d "order=$RECORDED_ID")"
echo "$LOOKUP" | grep -q "\"id\": \"$RECORDED_ID\"" || { echo "FAIL: the lookup answered '$LOOKUP'"; FAIL=1; }
sleep 6
stop_keploy "$RECORD_BIN"; own
kill "$DP" 2>/dev/null; sleep 1
TESTS=$(ls keploy/test-set-0/tests 2>/dev/null | wc -l)
[ "$TESTS" -eq 3 ] || { echo "FAIL: recorded $TESTS test cases, want 3 (see rec.log)"; tail -20 rec.log; FAIL=1; }
grep -q "$RECORDED_ID" keploy/test-set-0/mocks.yaml 2>/dev/null || { echo "FAIL: the dependency calls were not recorded as mocks"; FAIL=1; }

echo "== 2. keploy test before templatize, dependency DOWN: nothing is followed =="
replay test-plain.log
RUN2="$(newest_run)"
[ -n "$RUN2" ] || { echo "FAIL: the plain run wrote no report"; tail -30 test-plain.log; FAIL=1; }
grep -q '^status: FAILED' "$RUN2"/test-set-*-report.yaml 2>/dev/null || { echo "FAIL: without templates the create must fail on the id the app minted, as it always has"; FAIL=1; }
grep -q "run_ids:" "$RUN2"/*-report.yaml 2>/dev/null && { echo "FAIL: an id was followed in a set that has no templates"; FAIL=1; }

echo "== 3. keploy templatize, then keploy test: the id is followed through every test case =="
sudo -E env PATH="$PATH" "$REPLAY_BIN" templatize >templatize.log 2>&1; own
grep -rq '{{' keploy/test-set-0/tests/ || { echo "FAIL: keploy templatize made no template of the id the read-back reuses"; tail -20 templatize.log; FAIL=1; }
# The lookup's form body is the place this script relies on templatize leaving
# the id written out: only keploy rewriting the request can send the new id
# there. If templatize now puts a placeholder there too, give the lookup
# another carrier it does not reach, or this step stops proving that.
grep -q "body: order=$RECORDED_ID" keploy/test-set-0/tests/*lookup*.yaml 2>/dev/null || { echo "FAIL: the lookup's form body no longer holds the recorded id as written (see the note above this check)"; FAIL=1; }
replay test-templ.log
RUN3="$(newest_run)"
if [ "$RUN3" = "$RUN2" ] || ! run_passed; then
  echo "FAIL: the templatized create-then-read-backs set did not pass under replay"
  grep -hE "^status:|expected:|actual:" "$RUN3"/*-report.yaml 2>/dev/null | head -20; tail -30 test-templ.log; FAIL=1
fi
grep -A1 "run_ids:" "$RUN3"/*-report.yaml 2>/dev/null | grep -q "$RECORDED_ID: " || { echo "FAIL: the report does not say the recorded id $RECORDED_ID was swapped"; FAIL=1; }
# The read-back was sent with the id minted this run, not the recorded one.
# (Its URL holds a placeholder, which the create's answer teaches with or
# without following: this check alone proves little. The lookup's is the one
# that proves the request was rewritten.)
grep -h "url: .*/orders/[0-9a-f]" "$RUN3"/*-report.yaml | grep -vq "$RECORDED_ID" || { echo "FAIL: the read-back was sent with the recorded id"; FAIL=1; }
# So was the lookup, whose id no placeholder stands for.
grep -h "body: order=" "$RUN3"/*-report.yaml | grep -vq "$RECORDED_ID" || { echo "FAIL: the lookup was sent with the recorded id"; FAIL=1; }

# must_fail runs the templatized set against a regression of the app and
# checks the set failed. $1: the app's bug, $2: what the failure message says.
LAST_RUN="$RUN3"
must_fail() {
  replay "test-$1.log" "$1"
  local run; run="$(newest_run)"
  [ -n "$run" ] && [ "$run" != "$LAST_RUN" ] || { echo "FAIL: the run with $1 wrote no report"; FAIL=1; }
  grep -q '^status: FAILED' "$run"/test-set-*-report.yaml 2>/dev/null || { echo "FAIL: $2"; FAIL=1; }
  LAST_RUN="$run"
}

echo "== 4. --update-template keeps the recorded id, so the next run still follows it =="
replay test-update.log "" --update-template
run_passed || { echo "FAIL: the run with --update-template did not pass"; tail -30 test-update.log; FAIL=1; }
grep -q "$RECORDED_ID" keploy/test-set-0/config.yaml 2>/dev/null || { echo "FAIL: --update-template wrote this run's id over the recorded one in the set's templates:"; grep -A3 '^template:' keploy/test-set-0/config.yaml; FAIL=1; }
replay test-after-update.log
LAST_RUN="$(newest_run)"
run_passed || { echo "FAIL: the run after --update-template did not pass: the id is no longer followed"; grep -h "did not follow" test-after-update.log | head -5; FAIL=1; }

echo "== 5. the app answers the read-backs with another id: the set fails =="
must_fail wrong-id "the app answered with an id it did not make and the set still passed"

echo "== 6. the app asks its dependency for an id nobody made: the set fails =="
must_fail wrong-dep-id "the app asked its dependency for an id nobody made and the set still passed"
grep -q "names another id" test-wrong-dep-id.log || { echo "FAIL: the agent did not say that a call for another id was answered as recorded"; FAIL=1; }
RUN3="$LAST_RUN"

echo "== 7. with rebinding off the templatized set fails (the guard is load-bearing) =="
[ -f keploy.yml ] && cp keploy.yml keploy.yml.bak
cat > keploy.yml <<'YML'
test:
  disableMockRebinding: true
YML
replay test-off.log
RUN4="$(newest_run)"
[ -n "$RUN4" ] && [ "$RUN4" != "$RUN3" ] || { echo "FAIL: the run with rebinding off wrote no report"; FAIL=1; }
grep -q '^status: FAILED' "$RUN4"/test-set-*-report.yaml 2>/dev/null || { echo "FAIL: the set did not fail with rebinding off, so step 3 proves nothing"; FAIL=1; }
rm -f keploy.yml; [ -f keploy.yml.bak ] && mv keploy.yml.bak keploy.yml

echo "== 8. keploy normalize accepts a changed answer with the recorded id, so the next run still follows it =="
LAST_RUN="$RUN4"
must_fail shape-v2 "the app answered with another shape and the set still passed"
# The id that run made: what its report says the recorded one was swapped for.
V2_ID="$(grep -h -A1 "run_ids:" "$LAST_RUN"/*-report.yaml 2>/dev/null | grep -m1 "$RECORDED_ID: " | awk '{print $2}')"
[ -n "$V2_ID" ] && [ "$V2_ID" != "$RECORDED_ID" ] || { echo "FAIL: the run with shape-v2 does not report the id it made in place of $RECORDED_ID"; FAIL=1; }
sudo -E env PATH="$PATH" "$REPLAY_BIN" normalize --allow-high-risk >normalize.log 2>&1; own
grep -rq 'shape.*v2' keploy/test-set-0/tests/ || { echo "FAIL: keploy normalize did not accept the changed read-backs"; tail -20 normalize.log; FAIL=1; }
# The create is the test case that produces the id. Its accepted answer holds
# the id written out where the recorded one held a placeholder, so the next run
# passes only if the read-back's placeholder is still given the id of that run.
grep -rq 'kind.*v2' keploy/test-set-0/tests/ || { echo "FAIL: keploy normalize did not accept the changed create"; tail -20 normalize.log; FAIL=1; }
# Written with the id of the run they were taken from, the accepted answers
# would match no later run: the app makes another id every time.
[ -n "$V2_ID" ] && grep -rq "$V2_ID" keploy/test-set-0/tests/ && { echo "FAIL: keploy normalize wrote the id made by the run it accepted ($V2_ID) into the test cases, where the recorded one ($RECORDED_ID) belongs"; FAIL=1; }
replay test-after-normalize.log shape-v2
run_passed || { echo "FAIL: the run after keploy normalize did not pass: the accepted answers no longer name an id the replay can follow"; grep -hE "^status:|expected:|actual:" "$(newest_run)"/*-report.yaml 2>/dev/null | head -20; FAIL=1; }
# The read-back's placeholder was still given the id of this run, though no
# answer holds a placeholder to learn it from any more. (Sent with the recorded
# id this app would answer all the same — it keeps its orders at the
# dependency — so the request itself is what is checked.)
grep -h "url: .*/orders/[0-9a-f]" "$(newest_run)"/*-report.yaml | grep -vq "$RECORDED_ID" || { echo "FAIL: after keploy normalize the read-back was sent with the recorded id: its placeholder was not given the id made this run"; FAIL=1; }

echo "== 9. the app no longer sends its id to the dependency: nothing is followed, and keploy says so =="
LAST_RUN="$(newest_run)"
must_fail no-store "the app stopped storing its orders and the set still passed"
grep -q "run_ids:" "$LAST_RUN"/*-report.yaml 2>/dev/null && { echo "FAIL: an id was followed though the app never sent one to its dependency"; FAIL=1; }
grep "did not follow" test-no-store.log | grep -q "$RECORDED_ID" || { echo "FAIL: keploy did not say that the failed test cases name an id it did not follow"; FAIL=1; }

# The PR's race-enabled build: a data race anywhere in a run fails the test.
if grep -l "WARNING: DATA RACE" ./*.log >/dev/null 2>&1; then
  echo "FAIL: a data race was reported in: $(grep -l "WARNING: DATA RACE" ./*.log | tr '\n' ' ')"; FAIL=1
fi

if [ "$FAIL" = 0 ]; then
  echo "REBIND TEST E2E: PASSED"
  cd / && rm -rf "$WORK"
else
  echo "REBIND TEST E2E: FAILED (work directory kept: $WORK)"
fi
exit $FAIL
