#!/usr/bin/env bash
# e2e for a docker compose app that exits on its own during `keploy record`:
# compose then stops the whole project (keploy runs it with
# --abort-on-container-exit), keploy's agent with it. The agent must not go
# before the recording has saved everything it captured: every test case, mock
# and mapping. The app here answers N calls, each of which calls its
# dependency, and exits 0; the lane makes the N calls and waits for keploy to
# exit on its own. The recording must hold all N test cases and N dependency
# mocks, and each of the agent's streams must have been read to the agent's
# end marker, not ended by the agent going away (which keploy says at WARN).
# Run with the proven switch off and on.
#
# Every wait here is on an event: a line in the run's log, read in whole lines,
# or keploy's own exit.
#
# Env: RECORD_BIN (the keploy binary under test). The agent image the binary
# names must be loaded (.github/actions/download-image).
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true

RECORD_BIN="${RECORD_BIN:-keploy}"
IMAGE=python:3.11-slim
DEP_IMAGE=python:3.11-alpine
# How many calls the app answers before it exits.
N=20
WORK="$(mktemp -d)"
cd "$WORK" || exit 1
FAIL=0
# Names of this run's own: two runs on one runner never share a container.
ID=$$
# The app's port on this host.
source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/free-port.sh"
PORT=$(free_port) || exit 1

# The project leg 3 names with -p: a name that starts with "up", which keploy's
# command rewrite once took for the `up` subcommand.
PROJECT="upload$ID"

# Tear down only this run's projects, by name. Never a blanket prune.
cleanup() {
  [ -f docker-compose.yml ] && sudo docker compose down --remove-orphans >/dev/null 2>&1
  [ -f docker-compose.yml ] && sudo docker compose -p "$PROJECT" down --remove-orphans >/dev/null 2>&1
  return 0
}
finish() {
  cleanup
  cd / && sudo rm -rf "$WORK"
}
trap finish EXIT

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/docker-build-retry.sh"
docker_pull_retry "$IMAGE"
docker_pull_retry "$DEP_IMAGE"

cat > dep.py <<'PY'
import json, http.server
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        b = json.dumps({"path": self.path, "price": 42}).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    def log_message(self, *a): pass
http.server.HTTPServer(("0.0.0.0", 9411), H).serve_forever()
PY

# The app: answers each GET with its dependency's answer for the same path,
# and exits 0 at once after its answer to the Nth /price call (another call,
# such as keploy's readiness probe, is not counted). The dependency may not
# listen yet: /warmup is retried until it answers (the job's timeout bounds a
# dependency that never does). Only /price calls are checked.
cat > app.py <<'PY'
import http.server, os, time, urllib.request
DEP, N = os.environ["DEP_URL"], int(os.environ["N"])
answered = 0
while True:
    try:
        urllib.request.urlopen(DEP + "/warmup").read()
        break
    except OSError as e:
        print("the dependency does not answer yet:", type(e).__name__, e, flush=True)
        time.sleep(1)
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        global answered
        b = urllib.request.urlopen(DEP + self.path).read()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
        if self.path.startswith("/price/"):
            answered += 1
    def log_message(self, *a): pass
s = http.server.HTTPServer(("0.0.0.0", 8080), H)
print("APP LISTENING", flush=True)
while answered < N:
    s.handle_request()
print("APP ANSWERED ALL", N, flush=True)
PY

cat > docker-compose.yml <<YML
services:
  app:
    image: $IMAGE
    container_name: cae-app-$ID
    working_dir: /w
    volumes: [".:/w:z"] # :z relabels on SELinux hosts; ignored elsewhere
    command: python app.py
    ports: ["$PORT:8080"]
    environment:
      - DEP_URL=http://dep:9411
      - N=$N
    depends_on: [dep]
  dep:
    image: $DEP_IMAGE
    container_name: cae-dep-$ID
    working_dir: /w
    volumes: [".:/w:z"]
    command: python dep.py
YML

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

# record NAME PROVEN [FLAGS]: keploy records the compose project while the
# lane makes the app's N calls; the app then exits, and keploy must exit on
# its own, 0. The log is NAME.log. With FLAGS (compose's global -p NAME), the
# app container must run in that project.
record() {
  local name=$1 proven=$2 flags=${3:-} log="$1.log" pid rc i before=$FAIL
  sudo rm -rf keploy
  : > "$log"
  sudo -E env PATH="$PATH" KEPLOY_PROVEN="$proven" "$RECORD_BIN" record -c "docker compose ${flags:+$flags }up" --container-name "cae-app-$ID" \
    --disable-tele --debug > "$log" 2>&1 &
  pid=$!
  if await "$log" "$pid" "APP LISTENING" 1; then
    # Any state (-a): the app prints that it listens as its process starts,
    # and a loaded daemon lists the container as running only once its
    # start returns (compose's "Started"), which can come after.
    if [ -n "$flags" ] && [ -z "$(sudo docker ps -aq --filter "name=^/?cae-app-$ID\$" --filter "label=com.docker.compose.project=$PROJECT")" ]; then
      echo "FAIL: $name: the app container is not in project $PROJECT, which the command names (-p)"
      sudo docker ps -a --format '{{.Names}} project={{.Label "com.docker.compose.project"}}'
      FAIL=1
    fi
    for i in $(seq 1 "$N"); do
      curl -fsS -o /dev/null "http://127.0.0.1:$PORT/price/s$i" || { echo "FAIL: $name: GET /price/s$i through the app failed"; FAIL=1; }
    done
  else
    echo "FAIL: $name: keploy exited before the app listened"
    FAIL=1
  fi
  # The app exits after its N answers, and compose stops the project.
  wait "$pid"
  rc=$?
  [ "$rc" -eq 0 ] || { echo "FAIL: $name: keploy record exited $rc"; FAIL=1; }
  grep -q "APP ANSWERED ALL $N" "$log" || { echo "FAIL: $name: the app did not answer all $N calls"; FAIL=1; }
  [ "$FAIL" -eq "$before" ] || tail -80 "$log"
}

# saved_all NAME: the recording holds a test case for each of the N calls and
# a mock of each call the app made to its dependency for them.
saved_all() {
  local tests mocks
  tests=$(sudo sh -c 'ls keploy/test-set-*/tests/ 2>/dev/null' | grep -c '\.yaml$')
  mocks=$(sudo sh -c 'cat keploy/test-set-*/mocks.yaml' 2>/dev/null | grep -cE '^\s+url: \S*/price/s[0-9]+$')
  echo "$1: $tests test cases, $mocks /price mocks"
  [ "$tests" -eq "$N" ] || { echo "FAIL: $1: $tests test cases saved, want $N"; FAIL=1; }
  if [ "$mocks" -ne "$N" ]; then
    echo "FAIL: $1: $mocks /price mocks saved, want $N"
    FAIL=1
    sudo sh -c 'ls -l keploy/test-set-*; grep -h "url:" keploy/test-set-*/mocks.yaml | sort | uniq -c | head -40' 2>&1
    grep -E "mock" "$1.log" | grep -vi debug | tail -20
  fi
}

# ended_at_markers NAME: the recording read each of the agent's streams to the
# agent's end marker: keploy says so (at debug) for each drain. It never says
# the agent ended a stream first (the agent going away with the project, its
# tail lost), nor failed to ask for the markers, nor gave a drain up. keploy's
# own lines, not the agent's summary: compose forwards the agent's log, and
# can drop its last lines as the stack stops.
ended_at_markers() {
  local log="$1.log" frames
  for frames in "test cases" mocks mappings; do
    if ! grep "read the stream to the keploy agent's end marker" "$log" | grep -q "\"frames\": \"$frames\""; then
      echo "FAIL: $1: the $frames drain did not end at the agent's marker"
      grep -n "end marker\|ended its\|recording complete" "$log"
      FAIL=1
    fi
  done
  if grep -Eq "ended its stream before|ended its mapping stream before|could not ask the keploy agent to end its streams|teardown drain made no progress" "$log"; then
    echo "FAIL: $1: the agent's streams did not all end at its markers"
    grep -En "ended its stream before|ended its mapping stream before|could not ask the keploy agent to end its streams|teardown drain made no progress" "$log"
    FAIL=1
  fi
  # Drained, the agent goes on its own: keploy kills only a wedged one.
  if grep -q "Stopping the keploy agent by force" "$log"; then
    echo "FAIL: $1: keploy killed an agent that was not wedged"
    grep -n "Stopping the keploy agent by force" "$log"
    FAIL=1
  fi
}

echo "== 1. a compose app that exits on its own, the switch off =="
record exits-off ""
saved_all exits-off
ended_at_markers exits-off
cleanup

echo "== 2. a compose app that exits on its own, under the proven switch =="
record exits-on 1
saved_all exits-on
ended_at_markers exits-on
cleanup

echo "== 3. a compose command naming its project (-p), the switch off =="
record exits-project "" "-p $PROJECT"
saved_all exits-project
ended_at_markers exits-project
cleanup

# A race build reports a data race and carries on: fail on one in any leg.
if grep -l "WARNING: DATA RACE" ./*.log; then
  echo "FAIL: a data race, in the logs above"
  FAIL=1
fi

if [ "$FAIL" -ne 0 ]; then
  echo "compose app exits e2e FAILED"
  exit 1
fi
echo "compose app exits e2e passed"
