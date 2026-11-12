#!/usr/bin/env bash
# E2E: `keploy mock replay` honours keploy.yml's request noise.
#
# A browser-driven test's calls carry a Referer, the page the test was on. A
# user who declares it noise in keploy.yml (test.globalNoise.global.header:
# {Referer: []}) expects a call that differs from its recording only in that
# header to be served its mock. keploy mock did not hand the agent that
# noise, so a call that carried no Referer at replay (a page whose
# Referrer-Policy sends none, or a direct call) missed its mock and failed
# the run: without the proven switch the matcher does not compare a header's
# value, but it still requires each recorded header to be present unless the
# header was handed to it as noise. This also shows that keploy mock loads
# test.globalNoise from keploy.yml at all.
#
# Each case records a pytest test whose three calls carry a Referer, then
# replays it with the dependency stopped and no Referer sent:
#   declared  keploy.yml declares Referer noise: the replay is served from the
#             mocks and passes
#   control   keploy.yml declares none: the replay misses and fails, so the
#             lane can tell a served call from a missed one, and the miss's
#             hint names the header noise that serves it
#
# Runs without the proven switch only: a replay under the switch needs a
# build that judges each test (keploy enterprise), and this build refuses it
# (mock-offline-linux.sh checks that refusal).
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
# A lane that could not load it would skip both halves and pass: fail instead.
# shellcheck source=lane_switch.sh
if ! source "$MOCK_DIR/lane_switch.sh" || ! declare -F half >/dev/null; then
  echo "FAIL: could not load lane_switch.sh from $MOCK_DIR"
  exit 1
fi
# http_mocks_are: what a record kept is the app's three calls, as Http mocks,
# and DNS mocks only where the runner's resolver answers a lookup.
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
PORT=18996
FAIL=0
DEP_PID=""
trap '[ -z "$DEP_PID" ] || kill "$DEP_PID" 2>/dev/null' EXIT
CMD="python3 -m pytest -q -p no:cacheprovider test_api.py"
PAGE="http://app.local/desk/list"

cat > depserver.py <<'PY'
import http.server, json, socketserver, sys
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = json.dumps({"path": self.path}).encode()
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

# The test sends REFERER as its Referer when it is set, and no Referer when
# it is not.
cat > test_api.py <<PY
import json, os, urllib.request
def test_referer():
    for i in range(3):
        h = {"Referer": os.environ["REFERER"]} if os.environ.get("REFERER") else {}
        req = urllib.request.Request("http://127.0.0.1:$PORT/ping/%d" % i, headers=h)
        with urllib.request.urlopen(req, timeout=5) as r:
            assert json.load(r)["path"] == "/ping/%d" % i
PY

# start_dep: starts the dependency and waits for its ready line (DEP_PID).
start_dep() {
  : > dep.log
  python3 depserver.py "$PORT" >dep.log 2>&1 &
  DEP_PID=$!
  until grep -qx ready dep.log 2>/dev/null; do
    kill -0 "$DEP_PID" 2>/dev/null || { echo "FAIL: $LABEL: the dependency exited before it listened"; cat dep.log; FAIL=1; return 1; }
    sleep 0.1
  done
}
stop_dep() { kill "$DEP_PID" 2>/dev/null; wait "$DEP_PID" 2>/dev/null; DEP_PID=""; }

# run_case <label> <declared|none> <want pass|fail>: records with the Referer
# in a directory of its own, then replays with no Referer.
run_case() {
  local LABEL="$1" NOISE="$2" WANT="$3" rc got mocks referers missed
  local WORK="$ROOT/$LABEL"
  mkdir -p "$WORK" && cp "$ROOT/depserver.py" "$ROOT/test_api.py" "$WORK/" && cd "$WORK" || exit 1
  echo "######## $LABEL: keploy.yml noise $NOISE, replay want $WANT ########"
  : > keploy.yml
  if [ "$NOISE" = declared ]; then
    printf 'test:\n  globalNoise:\n    global:\n      header:\n        Referer: []\n' > keploy.yml
  fi

  echo "== record, every call with Referer $PAGE =="
  start_dep || return
  sudo env PATH="$PATH" "${SUDO_ENV[@]}" REFERER="$PAGE" "$RECORD_BIN" mock record -c "$CMD" --name hn --disable-tele 2>&1 | tee rec.log
  rc=${PIPESTATUS[0]}
  stop_dep
  [ "$rc" -eq 0 ] || { echo "FAIL: $LABEL: record exited $rc, want 0"; FAIL=1; return; }
  mocks=$(http_mocks_are keploy/hn/mocks.yaml 3) || { echo "FAIL: $LABEL: expected 3 Http mocks and no other kind but DNS: $mocks"; FAIL=1; return; }
  referers=$(grep -ciF "referer: $PAGE" keploy/hn/mocks.yaml 2>/dev/null); referers=${referers:-0}
  echo "recorded: $mocks; $referers with the Referer"
  [ "$referers" -eq 3 ] || { echo "FAIL: $LABEL: expected the Referer in 3 mocks, got $referers"; FAIL=1; return; }

  echo "== replay, the dependency stopped, no Referer sent =="
  sudo env PATH="$PATH" "${SUDO_ENV[@]}" "$RECORD_BIN" mock replay -c "$CMD" --name hn --disable-tele 2>&1 | tee rep.log
  rc=${PIPESTATUS[0]}
  got=fail
  [ "$rc" -eq 0 ] && grep -q "1 passed" rep.log && got=pass
  echo "replay exit=$rc: $got"
  if [ "$got" != "$WANT" ]; then
    echo "FAIL: $LABEL: the replay ${got}ed (exit $rc), want $WANT"
    FAIL=1
  else
    echo "ok: $LABEL: the replay ${got}ed"
  fi
  # The receipt says whether a call missed its mock: none when the replay
  # passes, and the control's replay fails by its misses, not by something
  # else.
  missed=$(sed -n 's/^missed: //p' keploy/hn/last-replay.yaml 2>/dev/null)
  echo "receipt: missed ${missed:-(none written)}"
  case "$WANT:$missed" in
    pass:0 | fail:[1-9]*) ;;
    *)
      echo "FAIL: $LABEL: the receipt says missed '${missed}', want $([ "$WANT" = pass ] && echo 0 || echo 'at least 1')"
      FAIL=1
      ;;
  esac
  # A miss names the remedy: the control's call lacks only the recorded
  # Referer, so its hint names keploy.yml's header bucket for it.
  if [ "$WANT" = fail ]; then
    if grep -qF "add the headers under test.globalNoise.global.header (e.g. header: {referer: []})" rep.log; then
      echo "ok: $LABEL: the miss's hint names test.globalNoise.global.header for the Referer"
    else
      echo "FAIL: $LABEL: the miss's hint does not name test.globalNoise.global.header for the Referer"
      FAIL=1
    fi
  fi
  cd "$ROOT" || exit 1
}

run_case declared declared pass
run_case control none fail

if [ "$FAIL" -eq 0 ]; then echo "MOCK HEADER-NOISE E2E: PASSED"; else echo "MOCK HEADER-NOISE E2E: FAILED"; fi
exit "$FAIL"
