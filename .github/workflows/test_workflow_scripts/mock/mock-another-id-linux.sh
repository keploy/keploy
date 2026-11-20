#!/usr/bin/env bash
# E2E: `keploy mock replay` says when a call was answered with the recording
# of another call, and never counts it as the call's own answer.
#
# The quickstart's shape: a test records GET /users/1 and GET /users/2, then
# a test calls GET /users/3, which the recording lacks. Without the proven
# switch the replay serves the call the recording of a path that differs only
# at a segment that looks like an id (user 1's or user 2's), so a test that
# only checks the status passes on another user's data. keploy reported every
# call answered from the recording, which made that pass look proven.
#
# One record, then four replays with the dependency stopped:
#   own        the recorded calls: served their own recordings, no call
#              answered with another's, the receipt isolated
#   another    GET /users/3: served another id's recording and passes, but
#              the replay names the call, the recording served and the
#              segment that differs (a WARN, the summary's
#              answered_as_another, the console and the receipt's
#              answeredAsAnother), and the receipt is not isolated
#   strict     GET /users/3 with --strict: the replay fails (exit 1)
#   noise      GET /users/3 with --strict and, in keploy.yml, the url noise
#              the next step names for an id the app makes on every run:
#              answered with that recording as its own, the receipt
#              isolated, and the replay passes
#
# Then a second record of GET /Users/<id> (a path with capitals) and two
# POST /orders whose JSON body holds the order id at data.id, and two
# replays of new ids:
#   names      GET /Users/3 and POST /orders of a new data.id: both served
#              another call's recording, and the next steps name a url
#              noise key with capitals and a request-body noise key with a
#              dot on the console, while the WARNs keep the calls' path
#              tokens out of the log
#   names-yml  the same with both keys in keploy.yml and --strict: keploy
#              loads them as written (the case kept, the dotted key one
#              field) and answers each call with that recording as its
#              own, and the replay passes
#
# Runs without the proven switch only: under it the matcher serves only a
# call's own recording, and a replay under it needs a build that judges each
# test (keploy enterprise); this build refuses it (mock-offline-linux.sh
# checks that refusal).
#
# Env: RECORD_BIN (the keploy binary under test), set by download-binary.
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
# shellcheck source=../sudo_env.sh
source "$(dirname "${BASH_SOURCE[0]}")/../sudo_env.sh" || exit 1
# root also needs an installation-id so the sudo'd agent doesn't prompt
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true

if ! python3 -c "import pytest" 2>/dev/null; then
  python3 -m ensurepip --default-pip >/dev/null 2>&1 || true
  python3 -m pip install --quiet --disable-pip-version-check pytest
fi

# This lane's directory: lane_switch.sh.
# shellcheck source-path=SCRIPTDIR
MOCK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# A lane that could not load it would skip its half and pass: fail instead.
# shellcheck source=lane_switch.sh
if ! source "$MOCK_DIR/lane_switch.sh" || ! declare -F half >/dev/null; then
  echo "FAIL: could not load lane_switch.sh from $MOCK_DIR"
  exit 1
fi
# http_mocks_are: what the record kept is the test's two calls, as Http
# mocks, and DNS mocks only where the runner's resolver answers a lookup.
# shellcheck source=../mock-kinds.sh
if ! source "$MOCK_DIR/../mock-kinds.sh" || ! declare -F http_mocks_are >/dev/null; then
  echo "FAIL: could not load mock-kinds.sh from $MOCK_DIR/.."
  exit 1
fi
if ! half off; then
  echo "FAIL: MOCK_LANE_SWITCH=$LANE_SWITCH: this lane has only a half without the switch; run it with off or both"
  exit 1
fi
RECORD_BIN="${RECORD_BIN:-keploy}"
ROOT="$(mktemp -d)"
cd "$ROOT" || exit 1
PORT=18993
FAIL=0
DEP_PID=""
trap '[ -z "$DEP_PID" ] || kill "$DEP_PID" 2>/dev/null' EXIT
CMD="python3 -m pytest -q -p no:cacheprovider test_users.py"
RECEIPT="keploy/users/last-replay.yaml"

fail() { echo "FAIL: $*"; FAIL=1; }

# The dependency answers GET /users/<id> with that user.
cat > depserver.py <<'PY'
import http.server, json, socketserver, sys
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        uid = self.path.rsplit("/", 1)[-1]
        body = json.dumps({"id": uid, "name": "user " + uid}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def do_POST(self):
        sent = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        body = json.dumps({"created": sent["data"]["id"]}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a): pass
socketserver.TCPServer.allow_reuse_address = True
with socketserver.TCPServer(("127.0.0.1", int(sys.argv[1])), H) as s:
    print("ready", flush=True)
    s.serve_forever()
PY

# The test calls each user in USERS and checks only the status, as a test
# that would pass on another user's data does; it writes the ids it was
# answered with to GOT.
cat > test_users.py <<PY
import json, os, urllib.request
def test_users():
    got = []
    for uid in os.environ["USERS"].split():
        with urllib.request.urlopen("http://127.0.0.1:$PORT/users/" + uid, timeout=5) as r:
            assert r.status == 200
            got.append(str(json.load(r)["id"]))
    with open(os.environ["GOT"], "w") as f:
        f.write(" ".join(got))
PY

# The second test calls GET /Users/<id> for each id in USERS, then POSTs
# /orders {"data": {"id": <id>, "item": <item>}} for each id:item in
# ORDERS; it writes the ids it was answered with to GOT.
cat > test_orders.py <<PY
import json, os, urllib.request
def test_orders():
    got = []
    base = "http://127.0.0.1:$PORT"
    for uid in os.environ["USERS"].split():
        with urllib.request.urlopen(base + "/Users/" + uid, timeout=5) as r:
            assert r.status == 200
            got.append(str(json.load(r)["id"]))
    for oid, item in (o.split(":") for o in os.environ["ORDERS"].split()):
        sent = json.dumps({"data": {"id": oid, "item": item}}).encode()
        req = urllib.request.Request(base + "/orders", data=sent, headers={"Content-Type": "application/json"}, method="POST")
        with urllib.request.urlopen(req, timeout=5) as r:
            assert r.status == 200
            got.append(str(json.load(r)["created"]))
    with open(os.environ["GOT"], "w") as f:
        f.write(" ".join(got))
PY

echo "== record GET /users/1 and GET /users/2 =="
: > dep.log
python3 depserver.py "$PORT" >dep.log 2>&1 &
DEP_PID=$!
until grep -qx ready dep.log 2>/dev/null; do
  kill -0 "$DEP_PID" 2>/dev/null || { echo "FAIL: the dependency exited before it listened"; cat dep.log; exit 1; }
  sleep 0.1
done
sudo env PATH="$PATH" "${SUDO_ENV[@]}" USERS="1 2" GOT="$ROOT/got-record" "$RECORD_BIN" mock record -c "$CMD" --name users --disable-tele 2>&1 | tee rec.log
rc=${PIPESTATUS[0]}
kill "$DEP_PID" 2>/dev/null; wait "$DEP_PID" 2>/dev/null; DEP_PID=""
[ "$rc" -eq 0 ] || { echo "FAIL: record exited $rc, want 0"; exit 1; }
mocks=$(http_mocks_are keploy/users/mocks.yaml 2) || { echo "FAIL: expected 2 Http mocks and no other kind but DNS: $mocks"; exit 1; }
echo "recorded: $mocks"
for p in /users/1 /users/2; do
  grep -qE "^ +url: \"?[^ ]*$p\"?\$" keploy/users/mocks.yaml || { echo "FAIL: no recording of GET $p"; sed -n "s/^ *url: //p" keploy/users/mocks.yaml; exit 1; }
done

# replay <label> <users> [flag]: replays the mock set SET's test (CMD)
# calling users (and ORDERS), with the dependency stopped; sets RC and LOG.
SET=users
replay() {
  local label="$1" users="$2" flag="${3:-}"
  LOG="$ROOT/$label.log"
  rm -f "$ROOT/got-$label"
  echo "== $label: replay, users $users${flag:+, $flag}, the dependency stopped =="
  # shellcheck disable=SC2086 # flag is one word or none
  sudo env PATH="$PATH" "${SUDO_ENV[@]}" USERS="$users" ORDERS="${ORDERS:-}" GOT="$ROOT/got-$label" "$RECORD_BIN" mock replay -c "$CMD" --name "$SET" --disable-tele $flag 2>&1 | tee "$LOG"
  RC=${PIPESTATUS[0]}
  echo "$label: replay exit=$RC, answered with ids '$(cat "$ROOT/got-$label" 2>/dev/null)'"
}

# passed: the last replay exited 0 and its test passed.
passed() { [ "$RC" -eq 0 ] && grep -q "1 passed" "$LOG"; }

# noise_entry KIND FILE: the first test.globalNoise.global.KIND entry a
# console next step in FILE names, whole. Only the console's "  next step:"
# lines carry it whole: the WARN's next_step is in log form, its keys shaped.
noise_entry() {
  grep '^  next step: ' "$2" | grep -o "test\\.globalNoise\\.global\\.$1: {\"[^\"]*\": \\[\\]}" | head -1 | sed "s/^test\\.globalNoise\\.global\\.$1: //"
}

# 1. own: each call is answered with its own recording, and nothing says
# otherwise.
replay own "1 2"
passed || fail "own: the replay of the recorded calls failed (exit $RC), want it to pass"
[ "$(cat "$ROOT/got-own" 2>/dev/null)" = "1 2" ] || fail "own: the calls were answered with ids '$(cat "$ROOT/got-own" 2>/dev/null)', want '1 2'"
grep -q 'mock replay summary.*"missed": 0' "$LOG" || fail "own: the summary does not say missed 0"
if grep -q 'answered_as_another' "$LOG"; then fail "own: the summary counts a call answered with another call's recording"; fi
if grep -q "answered with the recording of another call" "$LOG"; then fail "own: a call answered with its own recording is said to be another's"; fi
grep -qx 'isolated: true' "$RECEIPT" || fail "own: the receipt does not prove the run isolated"
if grep -q '^answeredAsAnother:' "$RECEIPT"; then fail "own: the receipt lists a call answered with another call's recording"; fi

# 2. another: GET /users/3 is served another id's recording, and the test
# passes on that user's data; the replay says so.
replay another "3"
passed || fail "another: the replay failed (exit $RC); without --strict a call served another id's recording is served, want it to pass"
case "$(cat "$ROOT/got-another" 2>/dev/null)" in
  1 | 2) echo "ok: another: GET /users/3 was served user $(cat "$ROOT/got-another")'s recording" ;;
  *) fail "another: GET /users/3 was answered with id '$(cat "$ROOT/got-another" 2>/dev/null)', want 1 or 2 (another id's recording)" ;;
esac
grep -q 'an outgoing call was answered with the recording of another call, not its own' "$LOG" || fail "another: no WARN names the call answered with another call's recording"
grep -q '"differs": \["url.path.1"\]' "$LOG" || fail "another: the WARN does not name url.path.1 as the segment that differs"
grep -q 'mock replay summary.*"missed": 0.*"answered_as_another": 1' "$LOG" || fail "another: the summary does not say missed 0 and answered_as_another 1"
grep -qE '^answered with another call.s recording: GET /users/3 was answered with the recording of GET /users/[12]:' "$LOG" || fail "another: the console does not name the call and the recording's call"
grep -qx 'isolated: false' "$RECEIPT" || fail "another: the receipt counts the run as isolated"
grep -q "answered with the recording of another call" "$RECEIPT" || fail "another: the receipt's isolation note does not say why"
grep -qx 'answeredAsAnother:' "$RECEIPT" || fail "another: the receipt has no answeredAsAnother"
grep -qE '^ +- call: GET /users/3$' "$RECEIPT" || fail "another: the receipt's answeredAsAnother does not name GET /users/3"
grep -qE '^ +answer: other_id$' "$RECEIPT" || fail "another: the receipt's answeredAsAnother does not say other_id"
grep -qE '^ +- url\.path\.1$' "$RECEIPT" || fail "another: the receipt's answeredAsAnother does not name url.path.1"
grep -qx 'missed: 0' "$RECEIPT" || fail "another: the receipt counts the served call as missed"
if [ "$FAIL" -ne 0 ]; then echo "== receipt =="; cat "$RECEIPT"; fi

# 3. strict: the same replay under --strict fails.
replay strict "3" --strict
[ "$RC" -eq 1 ] || fail "strict: the replay exited $RC, want 1"
grep -q 'replay failed under --strict: calls were answered with the recording of another call, not their own' "$LOG" || fail "strict: the failure does not say a call was answered with another call's recording"

# 4. noise: the next step's other cause, an id the app makes on every run,
# names the keploy.yml url noise that makes the recording the call's own.
# With it, GET /users/3 is answered with that recording as its own: nothing
# says otherwise, the receipt is isolated, and --strict passes.
ENTRY=$(noise_entry url "$ROOT/another.log")
if [ -z "$ENTRY" ]; then
  fail "noise: the console's next step for GET /users/3 names no test.globalNoise.global.url entry"
else
  echo "== noise: keploy.yml test.globalNoise.global.url: $ENTRY =="
  printf 'test:\n  globalNoise:\n    global:\n      url: %s\n' "$ENTRY" > keploy.yml
  replay noise "3" --strict
  rm -f keploy.yml
  passed || fail "noise: with the url noise the next step names, the replay under --strict failed (exit $RC), want it to pass"
  case "$(cat "$ROOT/got-noise" 2>/dev/null)" in
    1 | 2) echo "ok: noise: GET /users/3 was answered with user $(cat "$ROOT/got-noise")'s recording as its own" ;;
    *) fail "noise: GET /users/3 was answered with id '$(cat "$ROOT/got-noise" 2>/dev/null)', want 1 or 2" ;;
  esac
  if grep -q "answered with the recording of another call" "$LOG"; then fail "noise: the call is still said to be answered with another call's recording"; fi
  grep -q 'mock replay summary.*"missed": 0' "$LOG" || fail "noise: the summary does not say missed 0"
  grep -qx 'isolated: true' "$RECEIPT" || fail "noise: the receipt does not prove the run isolated"
  if grep -q '^answeredAsAnother:' "$RECEIPT"; then fail "noise: the receipt lists a call answered with another call's recording"; fi
fi

# 5. names: keploy.yml noise keys as the next steps write them, with capitals
# and a dot, load as written and make each recording the call's own.
SET=orders
CMD="python3 -m pytest -q -p no:cacheprovider test_orders.py"
RECEIPT="keploy/orders/last-replay.yaml"
echo "== record GET /Users/1 and POST /orders of data.id a1 and a2 =="
: > dep.log
python3 depserver.py "$PORT" >dep.log 2>&1 &
DEP_PID=$!
until grep -qx ready dep.log 2>/dev/null; do
  kill -0 "$DEP_PID" 2>/dev/null || { echo "FAIL: the dependency exited before it listened"; cat dep.log; exit 1; }
  sleep 0.1
done
sudo env PATH="$PATH" "${SUDO_ENV[@]}" USERS="1" ORDERS="a1:pen a2:book" GOT="$ROOT/got-record2" "$RECORD_BIN" mock record -c "$CMD" --name orders --disable-tele 2>&1 | tee rec2.log
rc=${PIPESTATUS[0]}
kill "$DEP_PID" 2>/dev/null; wait "$DEP_PID" 2>/dev/null; DEP_PID=""
[ "$rc" -eq 0 ] || { echo "FAIL: the second record exited $rc, want 0"; exit 1; }
mocks=$(http_mocks_are keploy/orders/mocks.yaml 3) || { echo "FAIL: expected 3 Http mocks and no other kind but DNS: $mocks"; exit 1; }
echo "recorded: $mocks"
grep -qE '^ +url: "?[^ ]*/Users/1"?$' keploy/orders/mocks.yaml || { echo "FAIL: no recording of GET /Users/1"; sed -n "s/^ *url: //p" keploy/orders/mocks.yaml; exit 1; }

ORDERS="n7:book"
replay names "3"
passed || fail "names: the replay failed (exit $RC); without --strict both calls are served another call's recording, want it to pass"
[ "$(cat "$ROOT/got-names" 2>/dev/null)" = "1 a2" ] || fail "names: the calls were answered with ids '$(cat "$ROOT/got-names" 2>/dev/null)', want '1 a2' (user 1's and the book order's recordings)"
grep -q 'mock replay summary.*"missed": 0.*"answered_as_another": 2' "$LOG" || fail "names: the summary does not say missed 0 and answered_as_another 2"
grep -qE '^ +answer: closest_body$' "$RECEIPT" || fail "names: the receipt's answeredAsAnother does not say closest_body"
grep -qE '^ +- body\.data\.id$' "$RECEIPT" || fail "names: the receipt's answeredAsAnother does not name body.data.id"
URLKEY=$(noise_entry url "$LOG")
BODYKEY=$(noise_entry requestbody "$LOG")
[ "$URLKEY" = '{"^/Users/[0-9]+$": []}' ] || fail "names: the next step for GET /Users/3 names url noise '$URLKEY', want '{\"^/Users/[0-9]+\$\": []}'"
[ "$BODYKEY" = '{"data.id": []}' ] || fail "names: the next step for POST /orders names request-body noise '$BODYKEY', want '{\"data.id\": []}'"
# The WARNs name each call in log form: no path token of the call reaches the log.
if grep 'an outgoing call was answered with the recording of another call' "$LOG" | grep -q 'Users'; then
  fail "names: a WARN for a call answered with another call's recording carries the path token Users, want the call in log form"
fi
if [ -n "$URLKEY" ] && [ -n "$BODYKEY" ]; then
  echo "== names-yml: keploy.yml url $URLKEY, requestbody $BODYKEY =="
  printf 'test:\n  globalNoise:\n    global:\n      url: %s\n      requestbody: %s\n' "$URLKEY" "$BODYKEY" > keploy.yml
  replay names-yml "3" --strict
  rm -f keploy.yml
  passed || fail "names-yml: with the noise keys the next steps name, the replay under --strict failed (exit $RC), want it to pass"
  [ "$(cat "$ROOT/got-names-yml" 2>/dev/null)" = "1 a2" ] || fail "names-yml: the calls were answered with ids '$(cat "$ROOT/got-names-yml" 2>/dev/null)', want '1 a2'"
  if grep -q "answered with the recording of another call" "$LOG"; then fail "names-yml: a call is still said to be answered with another call's recording"; fi
  grep -q 'mock replay summary.*"missed": 0' "$LOG" || fail "names-yml: the summary does not say missed 0"
  grep -qx 'isolated: true' "$RECEIPT" || fail "names-yml: the receipt does not prove the run isolated"
  if grep -q '^answeredAsAnother:' "$RECEIPT"; then fail "names-yml: the receipt lists a call answered with another call's recording"; fi
fi
if [ "$FAIL" -ne 0 ]; then echo "== receipt =="; cat "$RECEIPT"; fi

if [ "$FAIL" -eq 0 ]; then echo "MOCK ANOTHER-ID E2E: PASSED"; else echo "MOCK ANOTHER-ID E2E: FAILED"; fi
exit "$FAIL"
