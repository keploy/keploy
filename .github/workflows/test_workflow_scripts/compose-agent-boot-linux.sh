#!/usr/bin/env bash
# e2e for keploy's wait for the agent docker compose starts, under
# `keploy record -c "docker compose up"`. The agent is a service of the
# project keploy writes it into, so keploy does not start it: it waits for it
# to answer. That wait follows the agent's start, not a clock: it goes on while
# the start makes progress, and while the engine does not run the agent's
# container, and ends only on a start the agent reports failed or one that has
# stopped making progress (pkg.WatchAgentStart). A fixed budget of wall time
# (KEPLOY_AGENT_READY_TIMEOUT, 330 s by default, since removed) gave up on a
# start a loaded host ran slowly, and on a container the daemon held.
#
# Two legs, each with KEPLOY_AGENT_READY_TIMEOUT far below what the leg
# takes: this keploy reads no such variable, and a keploy that still did fails
# the leg. Each fails on a keploy whose waits a clock bounds:
#   1. starved, docker compose: the agent's container gets 2% of a CPU from its
#      start on, before it says its server listens (checked), so its start
#      takes far longer than the budget while it makes progress. keploy must
#      record the app's N calls, and exit 0 on its own once the app exits
#      after them. A keploy bound by the budget gives up after it.
#   2. paused after it said ready, docker run: keploy starts the agent's
#      container itself. From its creation on, before its process runs, the
#      lane drops every connection to its control port, so keploy's health
#      check cannot answer. Once the agent's report says its server listens
#      (ready), and keploy has read that report (the engine's archive-path
#      event of keploy's next docker cp of it), the lane pauses the container
#      for HOLD_S, past keploy's bound on an agent that says it listens and
#      does not answer (bootSilence, 60 s of keploy's runnable time), then
#      unpauses it and lets the port go. While the engine does not run the
#      agent nothing bounds keploy's wait, and the recording must hold the
#      app's N calls. A keploy that counts a paused agent's time against that
#      bound gives up during the pause.
#
# Every wait is on an event (a line in a log, a container's create or start
# event, the agent's report, keploy's exit); the pause's length is the
# fixture's, not a bound.
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
# and past bootSilence (60 s of keploy's runnable time, which is wall time for
# a keploy the host does not starve).
HOLD_S="${HOLD_S:-90}"
WORK="$(mktemp -d)"
cd "$WORK" || exit 1
FAIL=0
ID=$$
PORT=$(python3 -c 'import socket; s = socket.socket(); s.bind(("", 0)); print(s.getsockname()[1])')
EVENTS_PID=""
ACTOR_PID=""

# stop_watch ends the container watch and its action, and waits for the
# action to be gone: its trap ends what it runs in the background (a hold's
# sleep), so nothing of it outlives the leg.
stop_watch() {
  if [ -n "$ACTOR_PID" ]; then
    kill "$ACTOR_PID" 2>/dev/null
    wait "$ACTOR_PID" 2>/dev/null
  fi
  [ -n "$EVENTS_PID" ] && sudo kill "$EVENTS_PID" 2>/dev/null
  ACTOR_PID="" EVENTS_PID=""
  return 0
}
cleanup() {
  stop_watch
  [ -f blocked.port ] && unblock_port "$(cat blocked.port)"
  for c in $(sudo docker ps -aq --filter status=paused --filter name=keploy-v3-); do sudo docker unpause "$c" >/dev/null 2>&1; done
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

# watch_agent EVENT ACTION: once the agent's container (keploy-v3-*) has its
# EVENT (create: before its process runs; start), ACTION runs with its name,
# in the background. Stopped (stop_watch), it ends what ACTION runs in the
# background with it.
watch_agent() {
  local event=$1 action=$2 events="events.$2"
  : > "$events"
  sudo docker events --filter type=container --filter "event=$event" --format '{{.Actor.Attributes.name}}' > "$events" 2>&1 &
  EVENTS_PID=$!
  (
    trap 'sudo kill $(jobs -p) 2>/dev/null; exit 143' TERM
    name=""
    until name=$(grep -m1 '^keploy-v3-' "$events"); do sleep 0.1; done
    "$action" "$name"
  ) > "$action.out" 2>&1 &
  ACTOR_PID=$!
}

# boot_ready NAME: the start report of the agent in container NAME says its
# server listens (utils.BootProgress.Ready). Read through the engine's cp,
# which reads a paused container's files too.
boot_ready() {
  sudo docker cp "$1:/tmp/keploy-agent-boot.json" - 2>/dev/null | tar -xO 2>/dev/null |
    python3 -c 'import json, sys; sys.exit(0 if json.load(sys.stdin).get("ready") else 1)' 2>/dev/null
}

# agent_port NAME: the control port of the agent in container NAME (its
# command's --port).
agent_port() {
  sudo docker inspect -f '{{json .Args}}' "$1" |
    python3 -c 'import json, sys; a = json.load(sys.stdin); print(a[a.index("--port") + 1])'
}

# block_port PORT / unblock_port PORT: drop, and stop dropping, every
# connection to PORT the host makes (keploy's to the agent, through the
# published port or to the container), by its original destination port.
block_port() {
  echo "$1" > blocked.port
  sudo iptables -I OUTPUT -p tcp -m conntrack --ctorigdstport "$1" -j DROP &&
    sudo ip6tables -I OUTPUT -p tcp -m conntrack --ctorigdstport "$1" -j DROP
}
unblock_port() {
  sudo iptables -D OUTPUT -p tcp -m conntrack --ctorigdstport "$1" -j DROP 2>/dev/null
  sudo ip6tables -D OUTPUT -p tcp -m conntrack --ctorigdstport "$1" -j DROP 2>/dev/null
  rm -f blocked.port
  return 0
}

# up NAME: the agent's container is still being created or runs.
up() {
  case "$(sudo docker inspect -f '{{.State.Status}}' "$1" 2>/dev/null)" in
    created|running) return 0 ;;
  esac
  return 1
}

# hold_after_ready NAME: met at its container's creation, the agent in NAME
# cannot answer keploy's health check (its port blocked) when its report says
# its server listens, and keploy has read that report; then it is paused for
# HOLD_S, unpaused, and answers.
hold_after_ready() {
  local name=$1 port since ev
  port=$(agent_port "$name") || { echo "NOPORT: $name's command names no --port"; return 1; }
  block_port "$port" && echo "BLOCKED $port"
  if boot_ready "$name"; then
    echo "LATE: $name said its server listens before its port was blocked"
    return 1
  fi
  until boot_ready "$name"; do
    up "$name" || { echo "GONE: $name went before it said its server listens"; return 1; }
    sleep 0.2
  done
  # keploy's own read of the report that says so: the engine's archive-path
  # event of the next docker cp out of the container, which, the lane's own
  # cps over, is keploy's look at the agent's start. A pause before keploy
  # had read it would hold an agent keploy still took for one whose server
  # does not listen yet, which no rule bounds before or after this change.
  since=$(date +%s.%N)
  sudo docker events --since "$since" --filter type=container --filter "container=$name" --filter event=archive-path \
    --format '{{.Action}}' > "read.$name" 2>&1 &
  ev=$!
  until grep -q archive-path "read.$name"; do
    up "$name" || { echo "GONE: $name went before keploy read its report"; sudo kill "$ev"; return 1; }
    sleep 0.2
  done
  sudo kill "$ev"
  sudo docker pause "$name" && echo "PAUSED $name at $(date +%s), after it said its server listens and keploy read so"
  sleep "$HOLD_S" &
  wait $!
  sudo docker unpause "$name" && echo "UNPAUSED $name at $(date +%s)"
  unblock_port "$port" && echo "UNBLOCKED $port"
}

# starve_agent NAME: 2% of a CPU, from its start on, before it said its
# server listens.
starve_agent() {
  sudo docker update --cpus 0.02 "$1" && echo "STARVED $1 at $(date +%s)"
  if boot_ready "$1"; then
    echo "LATE: $1 said its server listens before it was starved"
  fi
}

# unstarve_agent ACTION: gives the agent's container (the one ACTION met) the
# host's CPUs back, once its start is over: the leg starves its start, not
# the app's calls.
unstarve_agent() {
  local name
  name=$(grep -m1 '^keploy-v3-' "events.$1") || return 0
  sudo docker update --cpus "$(nproc)" "$name" >/dev/null && echo "UNSTARVED $name"
}

# record NAME EVENT ACTION CMD CONTAINER [AFTER]: keploy records CMD (the app,
# in CONTAINER), its agent's container met by ACTION at its EVENT (create or
# start), while the lane makes the app's N calls; the app then exits, and keploy must exit 0 on
# its own. The calls go once the app listens and keploy's agent forwards its
# port ("Started ingress forwarding": a call before that is reset, the
# record ingress race keploy knows of), after AFTER ACTION runs, when given.
# A call that fails stops keploy, so a failed leg ends.
record() {
  local name=$1 event=$2 action=$3 cmd=$4 container=$5 after=${6:-} log="$1.log" pid rc i before=$FAIL t0 failed=0
  sudo rm -rf keploy
  : > "$log"
  watch_agent "$event" "$action"
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
record starved start starve_agent "docker compose up" "cab-app-$ID" unstarve_agent
grep -q "^STARVED " starve_agent.out || { echo "FAIL: starved: the agent's container was never starved"; FAIL=1; }
if grep -q "^LATE" starve_agent.out; then
  echo "FAIL: starved: the agent said its server listens before it was starved; the leg starved no start"
  FAIL=1
fi
saved_all starved
cleanup

echo "== 2. docker run, the agent's container paused for ${HOLD_S}s once it says its server listens, its health check held off =="
record paused create hold_after_ready "docker run --rm --name cab-solo-$ID -v $WORK:/w:z -w /w -e N=$N -p $PORT:8080 $IMAGE python solo.py" "cab-solo-$ID"
if ! grep -q "^UNBLOCKED " hold_after_ready.out; then
  echo "FAIL: paused: the agent was not held: blocked, paused once it said its server listens, unpaused and let go"
  FAIL=1
fi
saved_all paused 0
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
