#!/usr/bin/env bash
# e2e for a compose app published on another host port. keploy records each
# test with the app's port it reached (app_port, 8080 here) and replay sends
# each test from the host to that port, over the URL's: an app serving on
# several ports gets each test on its own. A compose file publishing
# "18080:8080" leaves nothing at host port 8080, and every test failed. Now,
# in Docker mode, a test whose app port is published on exactly one host port
# (read from the live container's publishes at replay) goes there, which is
# where the recorded client called it, and keploy says so once:
#   "app port 8080 is published on host port N; sending these tests there".
# The legs, each recorded and then replayed with `keploy test`:
#   1. "N:8080": record warns of nothing for the port; replay sends there and
#      says so, every test passes; with the proven switch off and on.
#   2. "M:M", the app's port published as itself: every test passes, and
#      keploy says nothing of a redirect.
#   3. "N1:8080" and "N2:8080", two host ports: nothing to choose between, so
#      replay sends the tests to 8080 as before and fails them, saying the
#      port is published on both and how to send the tests to one.
#
# Every wait here is on an event: a line in the run's log, read in whole
# lines, or keploy's own exit.
#
# Env: RECORD_BIN (the keploy binary under test). The agent image the binary
# names must be loaded (.github/actions/download-image).
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true

RECORD_BIN="${RECORD_BIN:-keploy}"
IMAGE=python:3.11-slim
WORK="$(mktemp -d)"
cd "$WORK" || exit 1
FAIL=0
# Names of this run's own: two runs on one runner never share a container.
ID=$$
APP="pp-app-$ID"
free_port() { python3 -c 'import socket; s = socket.socket(); s.bind(("", 0)); print(s.getsockname()[1])'; }

# Tear down only this run's project and container, by name. Never a blanket
# prune.
cleanup() {
  [ -f docker-compose.yml ] && sudo docker compose down --remove-orphans >/dev/null 2>&1
  sudo docker rm -f "$APP" >/dev/null 2>&1
  return 0
}
finish() {
  cleanup
  cd / && sudo rm -rf "$WORK"
}
trap finish EXIT

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/docker-build-retry.sh"
docker_pull_retry "$IMAGE"

# The app: answers each GET with its path, on APP_PORT (8080 unless set).
cat > app.py <<'PY'
import http.server, json, os
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        b = json.dumps({"path": self.path}).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    def log_message(self, *a): pass
s = http.server.HTTPServer(("0.0.0.0", int(os.environ.get("APP_PORT", "8080"))), H)
print("APP LISTENING", flush=True)
s.serve_forever()
PY
# http.server stamps each response with the current Date.
cat > keploy.yml <<'YML'
test:
    globalNoise:
        global: {"header": {"Date":[]}}
YML

# compose APP_PORT PORTS...: the compose file of an app listening on APP_PORT,
# published as each of PORTS.
compose() {
  local app_port=$1 ports=""
  shift
  for p in "$@"; do ports="$ports\"$p\", "; done
  cat > docker-compose.yml <<YML
services:
  app:
    image: $IMAGE
    container_name: $APP
    working_dir: /w
    volumes: [".:/w:z"] # :z relabels on SELinux hosts; ignored elsewhere
    command: python app.py
    ports: [${ports%, }]
    environment:
      - APP_PORT=$app_port
YML
}

# await LOG PID PATTERN N: returns 0 once LOG has N whole lines that carry
# PATTERN, or 1 when keploy (PID) exited first.
await() {
  python3 - "$@" <<'PY'
import sys, time
log, pid, pattern, n = sys.argv[1], int(sys.argv[2]), sys.argv[3].encode(), int(sys.argv[4])
seen, buf = 0, b""
with open(log, "rb") as f:
    while True:
        chunk = f.read(65536)
        if chunk:
            *lines, buf = (buf + chunk).split(b"\n")
            seen += sum(pattern in l for l in lines)
            if seen >= n:
                sys.exit(0)
            continue
        # keploy's sudo is this shell's child, left a zombie when it exits
        # until the shell waits for it.
        try:
            with open("/proc/%d/stat" % pid) as st:
                if st.read().rsplit(")", 1)[1].split()[0] == "Z":
                    sys.exit(1)
        except FileNotFoundError:
            sys.exit(1)
        time.sleep(0.2)
PY
}

# record NAME HOSTPORT: keploy records the compose app while the lane makes
# two calls through host port HOSTPORT; it is stopped once both calls' test
# cases are written. The log is NAME-record.log.
record() {
  local name=$1 hp=$2 log="$1-record.log" pid rc made=0
  sudo rm -rf keploy
  : > "$log"
  sudo -E env PATH="$PATH" "$RECORD_BIN" record -c "docker compose up" --container-name "$APP" \
    --disable-tele --disable-ansi > "$log" 2>&1 &
  pid=$!
  # Once the app listens, keploy's agent moves its socket and listens on its
  # port itself (ingress forwarding); a call before that is reset, so the
  # calls wait for it.
  if await "$log" "$pid" "APP LISTENING" 1 && await "$log" "$pid" "Started ingress forwarding" 1; then
    for p in /hello/one /hello/two; do
      if curl -fsS -o /dev/null --max-time 10 "http://127.0.0.1:$hp$p"; then
        made=$((made + 1))
      else
        echo "FAIL: $name: GET $p through host port $hp failed"; FAIL=1
      fi
    done
    [ "$made" -eq 0 ] || await "$log" "$pid" "Keploy has captured test cases" "$made" ||
      { echo "FAIL: $name: keploy did not write the test cases of the $made calls made"; FAIL=1; }
    sudo kill -INT "$pid" 2>/dev/null
  else
    echo "FAIL: $name: keploy exited before the app listened"; FAIL=1
  fi
  wait "$pid"
  rc=$?
  [ "$rc" -eq 0 ] || { echo "FAIL: $name: keploy record exited $rc"; FAIL=1; }
  [ "$made" -eq 2 ] || tail -60 "$log"
  cleanup
}

# replay NAME [ENV...]: keploy test of the recording, ENV set; the log is
# NAME-test.log, and the report's status and keploy's exit are echoed.
replay() {
  local name=$1 log="$1-test.log" rc
  shift
  # Only this replay's report is read: an earlier one's is no answer.
  sudo rm -rf keploy/reports
  sudo -E env PATH="$PATH" "$@" "$RECORD_BIN" test -c "docker compose up" --container-name "$APP" \
    --delay 5 --disable-tele --disable-ansi > "$log" 2>&1
  rc=$?
  cleanup
  REPORT_STATUS=$(sudo sh -c 'cat keploy/reports/test-run-*/test-set-0-report.yaml 2>/dev/null' | sed -n 's/^status: //p' | tail -n 1)
  REPLAY_RC=$rc
  echo "$name: keploy test exited $rc, report ${REPORT_STATUS:-missing}"
}

redirect_line() { echo "app port 8080 is published on host port $1; sending these tests there"; }

echo "== 1. published on another host port =="
HP=$(free_port)
compose 8080 "$HP:8080"
record remapped "$HP"
if grep -q "cannot be replayed at that port from the host" remapped-record.log; then
  echo "FAIL: remapped: record warned that the tests cannot be replayed, while replay sends them to host port $HP"
  grep "cannot be replayed" remapped-record.log; FAIL=1
fi
for leg in "remapped-off" "remapped-on KEPLOY_PROVEN=1"; do
  set -- $leg
  replay "$@"
  [ "$REPORT_STATUS" = PASSED ] && [ "$REPLAY_RC" -eq 0 ] ||
    { echo "FAIL: $1: want the set PASSED and exit 0"; tail -60 "$1-test.log"; FAIL=1; }
  n=$(grep -cF "$(redirect_line "$HP")" "$1-test.log")
  [ "$n" -eq 1 ] || { echo "FAIL: $1: said the redirect $n times, want once"; FAIL=1; }
  if grep -q "cannot be reached from the host" "$1-test.log"; then
    echo "FAIL: $1: a test was said to be unreachable"; grep "cannot be reached" "$1-test.log" | head -3; FAIL=1
  fi
done

echo "== 2. published as itself =="
P=$(free_port)
compose "$P" "$P:$P"
record same "$P"
replay same
[ "$REPORT_STATUS" = PASSED ] && [ "$REPLAY_RC" -eq 0 ] ||
  { echo "FAIL: same: want the set PASSED and exit 0"; tail -60 same-test.log; FAIL=1; }
if grep -q "sending these tests there" same-test.log; then
  echo "FAIL: same: said a redirect for a port published as itself"; FAIL=1
fi

echo "== 3. published on two host ports =="
HP1=$(free_port)
HP2=$(free_port)
compose 8080 "$HP1:8080" "$HP2:8080"
record two "$HP1"
replay two
[ "$REPORT_STATUS" = FAILED ] && [ "$REPLAY_RC" -ne 0 ] ||
  { echo "FAIL: two: want the set FAILED and a non-zero exit"; tail -60 two-test.log; FAIL=1; }
low=$(printf '%s\n%s\n' "$HP1" "$HP2" | sort | head -n 1)
high=$(printf '%s\n%s\n' "$HP1" "$HP2" | sort | tail -n 1)
want="the app's port 8080 cannot be reached from the host: it is published on host port $low, $high instead"
grep -qF "$want" two-test.log ||
  { echo "FAIL: two: the failure does not say: $want"; grep "cannot be reached" two-test.log | head -3; FAIL=1; }
if grep -q "sending these tests there" two-test.log; then
  echo "FAIL: two: said a redirect with two host ports to choose from"; FAIL=1
fi

if [ "$FAIL" -ne 0 ]; then
  echo "compose-published-port: FAILED"
  exit 1
fi
echo "compose-published-port: all legs passed"
