#!/usr/bin/env bash
# e2e for keploy's wait for the agent docker compose starts, under
# `keploy record -c "docker compose up"`. The agent is a service of the
# project keploy writes it into, so keploy does not start it: it waits for it
# to answer. That wait follows the agent's start, not a clock: it goes on while
# the start makes progress, and while the engine does not run the agent's
# container, and ends only on a start the agent reports failed or one that has
# stopped making progress (pkg.WatchAgentStart). A fixed budget of wall time
# (KEPLOY_AGENT_READY_TIMEOUT, 330 s by default) gave up on a start a loaded
# host ran slowly, and on a container the daemon held.
#
# Three legs, each with KEPLOY_AGENT_READY_TIMEOUT far below what the leg
# takes, so a wait bounded by it fails the leg:
#   1. starved, docker compose: the agent's container gets 2% of a CPU from its
#      start on, so its start takes far longer than the budget while it makes
#      progress. keploy must record the app's N calls, and exit 0 on its own
#      once the app exits after them.
#   2. paused, docker run: keploy starts the agent's container itself, which
#      the lane pauses as it starts, for longer than the budget and than
#      keploy's bound on a running agent's silence (bootSilence, 60 s of
#      keploy's runnable time), then unpauses. While it does not run nothing
#      bounds keploy's wait, and the recording must hold the app's N calls.
#   3. paused, docker compose: compose's own wait for its agent service's
#      healthcheck fails on a paused container ("dependency failed to start:
#      ... is unhealthy") and the project aborts. keploy must say so at once,
#      as the project's exit, never as an agent start it gave up on.
#
# Every wait is on an event (a line in a log, a container's start event,
# keploy's exit); the pause's length is the fixture's, not a bound.
#
# Env: RECORD_BIN (the keploy binary under test). The agent image the binary
# names must be loaded (.github/actions/download-image).
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true

RECORD_BIN="${RECORD_BIN:-keploy}"
IMAGE=python:3.11-slim
DEP_IMAGE=python:3.11-alpine
N=5
# The budget a clock-bound wait would give up at.
BUDGET_S=10
# How long the paused leg holds the agent's container paused: past the budget,
# and past bootSilence.
HOLD_S="${HOLD_S:-90}"
WORK="$(mktemp -d)"
cd "$WORK" || exit 1
FAIL=0
ID=$$
PORT=$(python3 -c 'import socket; s = socket.socket(); s.bind(("", 0)); print(s.getsockname()[1])')
EVENTS_PID=""
ACTOR_PID=""

stop_watch() {
  [ -n "$ACTOR_PID" ] && kill "$ACTOR_PID" 2>/dev/null
  [ -n "$EVENTS_PID" ] && sudo kill "$EVENTS_PID" 2>/dev/null
  ACTOR_PID="" EVENTS_PID=""
  return 0
}
cleanup() {
  stop_watch
  [ -f docker-compose.yml ] && sudo docker compose down --remove-orphans >/dev/null 2>&1
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
# and exits 0 after its answer to the Nth /price call. The dependency may not
# listen yet: /warmup is retried until it answers.
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

# The docker run leg's app: answers each GET itself, and exits 0 after its
# answer to the Nth /price call.
cat > solo.py <<'PY'
import http.server, json, os
N = int(os.environ["N"])
answered = 0
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        global answered
        b = json.dumps({"path": self.path, "price": 42}).encode()
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
    container_name: cab-app-$ID
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
    container_name: cab-dep-$ID
    working_dir: /w
    volumes: [".:/w:z"]
    command: python dep.py
YML

# await LOG PID PATTERN: returns 0 once LOG has a whole line that carries
# PATTERN, or 1 when PID exited first.
await() {
  python3 - "$@" <<'PY'
import sys, time
log, pid, pattern = sys.argv[1], int(sys.argv[2]), sys.argv[3].encode()
buf = b""
with open(log, "rb") as f:
    while True:
        chunk = f.read(65536)
        if chunk:
            *lines, buf = (buf + chunk).split(b"\n")
            if any(pattern in l for l in lines):
                sys.exit(0)
            continue
        try:
            with open("/proc/%d/stat" % pid) as st:
                if st.read().rsplit(")", 1)[1].split()[0] == "Z":
                    sys.exit(1)
        except FileNotFoundError:
            sys.exit(1)
        time.sleep(0.2)
PY
}

# watch_agent_start ACTION: once the agent's container (keploy-v3-*) starts,
# its start event, ACTION runs with its name, in the background.
watch_agent_start() {
  local action=$1 events="events.$1"
  : > "$events"
  sudo docker events --filter type=container --filter event=start --format '{{.Actor.Attributes.name}}' > "$events" 2>&1 &
  EVENTS_PID=$!
  (
    name=""
    until name=$(grep -m1 '^keploy-v3-' "$events"); do sleep 0.1; done
    "$action" "$name"
  ) > "$action.out" 2>&1 &
  ACTOR_PID=$!
}

pause_agent() {
  sudo docker pause "$1" && echo "PAUSED $1 at $(date +%s)"
  sleep "$HOLD_S"
  sudo docker unpause "$1" && echo "UNPAUSED $1 at $(date +%s)"
}

starve_agent() {
  sudo docker update --cpus 0.02 "$1" && echo "STARVED $1 at $(date +%s)"
}

# unstarve_agent ACTION: gives the agent's container (the one ACTION met) the
# host's CPUs back, once its start is over: the leg starves its start, not
# the app's calls.
unstarve_agent() {
  local name
  name=$(grep -m1 '^keploy-v3-' "events.$1") || return 0
  sudo docker update --cpus "$(nproc)" "$name" >/dev/null && echo "UNSTARVED $name"
}

# record NAME ACTION CMD CONTAINER [AFTER]: keploy records CMD (the app, in
# CONTAINER), its agent's container met by ACTION as it starts, while the
# lane makes the app's N calls; the app then exits, and keploy must exit 0 on
# its own. The calls go once the app listens and keploy's agent forwards its
# port ("Started ingress forwarding": a call before that is reset, the
# record ingress race keploy knows of), after AFTER ACTION runs, when given.
# A call that fails stops keploy, so a failed leg ends.
record() {
  local name=$1 action=$2 cmd=$3 container=$4 after=${5:-} log="$1.log" pid rc i before=$FAIL t0 failed=0
  sudo rm -rf keploy
  : > "$log"
  watch_agent_start "$action"
  t0=$(date +%s)
  sudo -E env PATH="$PATH" KEPLOY_AGENT_READY_TIMEOUT="$BUDGET_S" "$RECORD_BIN" record -c "$cmd" --container-name "$container" \
    --disable-tele --debug > "$log" 2>&1 &
  pid=$!
  if await "$log" "$pid" "APP LISTENING" && await "$log" "$pid" "Started ingress forwarding"; then
    echo "$name: the app listened, its port forwarded, $(( $(date +%s) - t0 ))s after keploy started (a wait bounded by the budget gives up at ${BUDGET_S}s)"
    [ -z "$after" ] || "$after" "$action"
    for i in $(seq 1 "$N"); do
      curl -fsS -o /dev/null "http://127.0.0.1:$PORT/price/s$i" || { echo "FAIL: $name: GET /price/s$i through the app failed"; FAIL=1; failed=1; }
    done
    # The app exits only after its N answers: a call that failed leaves it
    # up, and keploy with it.
    [ "$failed" -eq 0 ] || sudo kill -INT "$pid" 2>/dev/null
  else
    echo "FAIL: $name: keploy exited before the app listened"
    FAIL=1
  fi
  wait "$pid"
  rc=$?
  stop_watch
  cat "$action.out"
  [ "$rc" -eq 0 ] || { echo "FAIL: $name: keploy record exited $rc"; FAIL=1; }
  grep -q "APP ANSWERED ALL $N" "$log" || { echo "FAIL: $name: the app did not answer all $N calls"; FAIL=1; }
  if grep -Eq "did not come up|did not become ready" "$log"; then
    echo "FAIL: $name: keploy gave up on the agent's start"
    grep -En "did not come up|did not become ready" "$log"
    FAIL=1
  fi
  [ "$FAIL" -eq "$before" ] || tail -80 "$log"
}

# saved_all NAME [MOCKS]: the recording holds a test case for each of the N
# calls and, unless MOCKS is 0, a mock of each call the app made to its
# dependency for them.
saved_all() {
  local tests mocks want_mocks=${2:-$N}
  tests=$(sudo sh -c 'ls keploy/test-set-*/tests/ 2>/dev/null' | grep -c '\.yaml$')
  mocks=$(sudo sh -c 'cat keploy/test-set-*/mocks.yaml' 2>/dev/null | grep -cE '^\s+url: \S*/price/s[0-9]+$')
  echo "$1: $tests test cases, $mocks /price mocks"
  [ "$tests" -eq "$N" ] || { echo "FAIL: $1: $tests test cases saved, want $N"; FAIL=1; }
  [ "$mocks" -eq "$want_mocks" ] || { echo "FAIL: $1: $mocks /price mocks saved, want $want_mocks"; FAIL=1; }
}

echo "== 1. docker compose, the agent's container starved (2% of a CPU) from its start =="
record starved starve_agent "docker compose up" "cab-app-$ID" unstarve_agent
grep -q "^STARVED " starve_agent.out || { echo "FAIL: starved: the agent's container was never starved"; FAIL=1; }
saved_all starved
cleanup

echo "== 2. docker run, the agent's container paused as it starts, for ${HOLD_S}s =="
record paused pause_agent "docker run --rm --name cab-solo-$ID -v $WORK:/w:z -w /w -e N=$N -p $PORT:8080 $IMAGE python solo.py" "cab-solo-$ID"
grep -q "^UNPAUSED " pause_agent.out || { echo "FAIL: paused: the agent's container was never paused and unpaused"; FAIL=1; }
saved_all paused 0
cleanup

echo "== 3. docker compose, the agent's container paused as it starts =="
# compose's own dependency wait fails on the paused agent and aborts the
# project: keploy says so at once, as the project's exit.
sudo rm -rf keploy
: > compose-paused.log
HOLD_S=600 watch_agent_start pause_agent
sudo -E env PATH="$PATH" KEPLOY_AGENT_READY_TIMEOUT="$BUDGET_S" "$RECORD_BIN" record -c "docker compose up" --container-name "cab-app-$ID" \
  --disable-tele --debug > compose-paused.log 2>&1
rc=$?
stop_watch
for c in $(sudo docker ps -aq --filter status=paused --filter name=keploy-v3-); do sudo docker unpause "$c" >/dev/null 2>&1; done
[ "$rc" -ne 0 ] || { echo "FAIL: compose-paused: keploy record exited 0 with its project aborted"; FAIL=1; }
if ! grep -q "the compose project stopped while keploy was waiting for the keploy-agent to come up" compose-paused.log; then
  echo "FAIL: compose-paused: keploy did not say the project stopped while it waited for the agent"
  tail -40 compose-paused.log
  FAIL=1
fi
if grep -Eq "did not come up|did not become ready" compose-paused.log; then
  echo "FAIL: compose-paused: keploy took the project's abort for an agent start it gave up on"
  grep -En "did not come up|did not become ready" compose-paused.log
  FAIL=1
fi
cleanup

if grep -l "WARNING: DATA RACE" ./*.log; then
  echo "FAIL: a data race, in the logs above"
  FAIL=1
fi

if [ "$FAIL" -ne 0 ]; then
  echo "compose agent boot e2e FAILED"
  exit 1
fi
echo "compose agent boot e2e passed"
