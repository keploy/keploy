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
#   1. starved, docker compose: the engine makes the agent's container with 2%
#      of a CPU (agent-cpu.yml, a compose override of the service keploy
#      writes its agent in), a quota the kernel holds its process to from its
#      first instruction (checked once its start is over: the container's
#      quota, its cgroup's cpu.max, that the quota throttled it, and that it
#      said its server listens past the budget), so its start takes far longer
#      than the budget while it makes progress. keploy must record the app's N
#      calls, and exit 0 on its own once the app exits after them. A keploy
#      bound by the budget gives up after it. (The quota used to be set on the
#      running container, on its start event: an agent the host ran quickly
#      said its server listens before the quota landed, and the leg starved no
#      start.)
#   2. paused after it said ready, docker run: keploy starts the agent's
#      container itself. From before keploy starts, the lane drops every
#      connection the host makes to the ports keploy picks the agent's control
#      port among (those above the ephemeral range; checked to hold its port
#      at the container's creation), so keploy's health check cannot answer.
#      Once the agent's report says its server listens (ready), and keploy has
#      read that report (the engine's archive-path event of keploy's next
#      docker cp of it), the lane pauses the container for HOLD_S, past
#      keploy's bound on an agent that says it listens and does not answer
#      (bootSilence, 60 s of keploy's runnable time), then unpauses it and
#      lets the ports go. While the engine does not run the agent nothing
#      bounds keploy's wait, and the recording must hold the app's N calls. A
#      keploy that counts a paused agent's time against that bound gives up
#      during the pause.
#
# Every wait is on an event (a line in a log, a container's create event, the
# agent's report, keploy's exit); the pause's length is the fixture's, not a
# bound. What a leg holds the agent's start to (its quota, the blocked ports)
# is in place before its container exists: none of it is a reaction to the
# agent's start, which a host that runs the agent quickly would win.
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
  rm -f held.ports
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

# The starved leg's agent: keploy writes its agent into the app's project as
# the service keploy-agent (a fixed key: keployAgentComposeService in
# pkg/client/app), and puts its own file in place of only the -f that names
# the app's file, so compose merges this file onto that service. The engine then
# makes the agent's container with 2% of a CPU, before its process runs.
cat > agent-cpu.yml <<'YML'
services:
  keploy-agent:
    cpus: 0.02
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
# EVENT (create: before its process runs), ACTION runs with its name, in the
# background, its output in ACTION.out. Stopped (stop_watch), it ends what
# ACTION runs in the background with it.
watch_agent() {
  local event=$1 action=$2 events="events.$2" since
  : > "$events"
  # The events since now, not since the stream connects: keploy starts at
  # once, and an event the engine sent before the stream connected would be
  # missed.
  since=$(date +%s.%N)
  sudo docker events --since "$since" --filter type=container --filter "event=$event" \
    --format '{{.Actor.Attributes.name}}' > "$events" 2>&1 &
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

# boot_report_time NAME: when (Unix seconds) the agent in container NAME wrote
# the start report that says its server listens (the agent writes none after
# it); nothing while its report does not say so.
boot_report_time() {
  sudo docker cp "$1:/tmp/keploy-agent-boot.json" - 2>/dev/null |
    python3 -c '
import json, sys, tarfile
t = tarfile.open(fileobj=sys.stdin.buffer, mode="r|")
m = t.next()
if json.load(t.extractfile(m)).get("ready"):
    print(int(m.mtime))' 2>/dev/null
}

# agent_port NAME: the control port of the agent in container NAME (its
# command's --port).
agent_port() {
  sudo docker inspect -f '{{json .Args}}' "$1" |
    python3 -c 'import json, sys; a = json.load(sys.stdin); print(a[a.index("--port") + 1])'
}

# agent_ports: the ports keploy picks its agent's control port among
# (utils.GetAvailablePort: those above the ephemeral range), as LO:HI. Fails
# when the ephemeral range reaches 65535: keploy then takes a port the kernel
# assigns, which no block set beforehand can name.
agent_ports() {
  local hi
  read -r _ hi < /proc/sys/net/ipv4/ip_local_port_range || return 1
  [ "$hi" -lt 65535 ] && echo "$((hi + 1)):65535"
}

# in_ports PORT LO:HI: PORT is one of LO to HI.
in_ports() {
  [ "$1" -ge "${2%%:*}" ] && [ "$1" -le "${2##*:}" ]
}

# block_port PORTS / unblock_port PORTS: drop, and stop dropping, every
# packet the host sends on a connection whose original destination port is
# one of PORTS (a port, or LO:HI): keploy's connections to the agent, through
# the published port or to the container, and the host's replies on any
# connection into such a port.
# unblock_port fails, keeping blocked.port for a later try, unless both rules
# are gone (-C says 1: no such rule). -w: an xtables lock dockerd holds is
# waited for, not taken for a failure.
block_port() {
  echo "$1" > blocked.port
  sudo iptables -w -I OUTPUT -p tcp -m conntrack --ctorigdstport "$1" -j DROP &&
    sudo ip6tables -w -I OUTPUT -p tcp -m conntrack --ctorigdstport "$1" -j DROP
}
unblock_port() {
  local t rc
  [ -n "$1" ] || return 1
  for t in iptables ip6tables; do
    sudo "$t" -w -D OUTPUT -p tcp -m conntrack --ctorigdstport "$1" -j DROP 2>/dev/null
    sudo "$t" -w -C OUTPUT -p tcp -m conntrack --ctorigdstport "$1" -j DROP 2>/dev/null
    rc=$?
    if [ "$rc" -ne 1 ]; then
      echo "the ports $1 may still be blocked ($t -C says $rc)"
      return 1
    fi
  done
  rm -f blocked.port
}

# up NAME: the agent's container is still being created or runs.
up() {
  case "$(sudo docker inspect -f '{{.State.Status}}' "$1" 2>/dev/null)" in
    created|running) return 0 ;;
  esac
  return 1
}

# hold_after_ready NAME: met at its container's creation, the agent in NAME
# cannot answer keploy's health check when its report says its server
# listens, and keploy has read that report: its port is one of those the lane
# blocked before keploy started (held.ports), so no step of the lane races
# its start. Then it is paused for HOLD_S, unpaused, and answers. LATE when
# its port is not one of them: keploy's health check could answer before the
# pause.
hold_after_ready() {
  local name=$1 port since ev ports
  ports=$(cat held.ports 2>/dev/null)
  # Every return before the hold's end lets the ports go: keploy's wait then
  # ends on its own, and the leg fails without UNBLOCKED, which it asserts.
  port=$(agent_port "$name") || { echo "NOPORT: $name's command names no --port"; [ -z "$ports" ] || unblock_port "$ports"; return 1; }
  if [ -z "$ports" ] || ! in_ports "$port" "$ports"; then
    echo "LATE: $name's port $port is not one of the ports the lane blocked before keploy started (${ports:-none blocked})"
    [ -z "$ports" ] || unblock_port "$ports"
    return 1
  fi
  echo "BLOCKED $port, one of the ports $ports the lane blocked before keploy started"
  until boot_ready "$name"; do
    up "$name" || { echo "GONE: $name went before it said its server listens"; unblock_port "$ports"; return 1; }
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
    up "$name" || { echo "GONE: $name went before keploy read its report"; sudo kill "$ev"; unblock_port "$ports"; return 1; }
    sleep 0.2
  done
  sudo kill "$ev"
  # A failed pause or unpause must not leave the agent frozen: keploy waits
  # on an agent the engine does not run however long that takes (by design),
  # so the leg would hang. Unpaused, or else killed (the engine's kill of a
  # paused container unpauses it), the agent's run ends, and so the leg.
  if ! sudo docker pause "$name"; then
    echo "NOPAUSE: $name could not be paused"
    sudo docker unpause "$name" >/dev/null 2>&1 || sudo docker kill "$name" >/dev/null 2>&1
    unblock_port "$ports"
    return 1
  fi
  echo "PAUSED $name at $(date +%s), after it said its server listens and keploy read so"
  sleep "$HOLD_S" &
  wait $!
  if ! sudo docker unpause "$name"; then
    echo "NOUNPAUSE: $name could not be unpaused; killing it"
    sudo docker kill "$name" >/dev/null 2>&1
    unblock_port "$ports"
    return 1
  fi
  echo "UNPAUSED $name at $(date +%s)"
  unblock_port "$ports" && echo "UNBLOCKED $port (ports $ports)"
}

# cgroup_file NAME FILE: FILE of the cgroup (v2) the process of container
# NAME runs in.
cgroup_file() {
  local pid cg
  pid=$(sudo docker inspect -f '{{.State.Pid}}' "$1" 2>/dev/null) && [ "${pid:-0}" -gt 0 ] || return 1
  cg=$(sudo cat "/proc/$pid/cgroup" 2>/dev/null | awk -F: '$1 == "0" { print $3; exit }')
  [ -n "$cg" ] && sudo cat "/sys/fs/cgroup$cg/$2" 2>/dev/null
}

# unstarve_agent: once the agent's start is over (the app listens, and it
# starts only once the agent's server listens), says whether that start ran
# starved from its first instruction, then gives the agent's container the
# host's CPUs back: the leg starves its start, not the app's calls. Starved
# from its first instruction: the engine made the container with 2% of a CPU
# (agent-cpu.yml; NanoCpus, which only `docker update` changes, and only this
# does, after the look), its cgroup holds its process to that (cpu.max, which
# the runtime writes before the process runs), and the quota held its start
# back (cpu.stat's nr_throttled: the periods it was throttled in by the time
# the lane looked, once the app listened).
# STARVED if so, LATE otherwise. And whether the start, so starved, outlasted
# the budget, from the engine's and the kernel's clocks: its container's start
# to the agent's report that its server listens (the report's mtime). HELD if
# so, SHORT otherwise: a start over within the budget would pass a keploy the
# budget bounds too. One look, by the one actor that unstarves the agent,
# after its start: nothing races it.
unstarve_agent() {
  local name nano max throttled project started ready held
  # The agent's service in the app's project (keploy runs both in one).
  project=$(sudo docker inspect -f '{{index .Config.Labels "com.docker.compose.project"}}' "cab-app-$ID" 2>/dev/null)
  if [ -z "$project" ]; then
    echo "LATE: the app's container cab-app-$ID names no compose project: the leg cannot find the agent's"
    return 1
  fi
  name=$(sudo docker ps --filter "label=com.docker.compose.project=$project" \
    --filter label=com.docker.compose.service=keploy-agent --format '{{.Names}}')
  case "$name" in
    *$'\n'* | '') echo "LATE: not one container of keploy's agent service runs (${name:-none}): the leg saw no start starved"; return 1 ;;
  esac
  nano=$(sudo docker inspect -f '{{.HostConfig.NanoCpus}}' "$name" 2>/dev/null)
  max=$(cgroup_file "$name" cpu.max)
  throttled=$(cgroup_file "$name" cpu.stat | awk '$1 == "nr_throttled" { print $2 }')
  if [ -z "$nano" ] || [ -z "$max" ] || [ -z "$throttled" ]; then
    echo "LATE: $name: could not read its quota (NanoCpus ${nano:-unread}, cpu.max ${max:-unread}, nr_throttled ${throttled:-unread}): the leg saw no start starved"
  # 2% of a CPU: a quota of a fiftieth of the period.
  elif [ "$nano" != 20000000 ] || ! awk -v m="$max" 'BEGIN { split(m, f, " "); exit !(f[1] ~ /^[0-9]+$/ && f[1] * 50 == f[2]) }'; then
    echo "LATE: $name was not made with 2% of a CPU (NanoCpus $nano, cpu.max $max): its start ran on more"
  elif ! [ "$throttled" -gt 0 ] 2>/dev/null; then
    echo "LATE: $name's quota held its start back in no period (nr_throttled $throttled): the leg starved no start"
  else
    echo "STARVED $name from its creation: NanoCpus $nano, cpu.max $max; throttled in $throttled periods by the time the lane looked"
  fi
  # (date -d "" reads as today's midnight: an unread or zero time stays empty.)
  started=$(sudo docker inspect -f '{{.State.StartedAt}}' "$name" 2>/dev/null)
  case "$started" in
    '' | 0001-*) started= ;;
    *) started=$(date -d "$started" +%s 2>/dev/null) ;;
  esac
  ready=$(boot_report_time "$name")
  if [ -z "$started" ] || [ -z "$ready" ]; then
    echo "SHORT: $name: could not read when its container started (${started:-unread}) or when its report said its server listens (${ready:-unread})"
  else
    held=$((ready - started))
    # Whole seconds both: past the budget by more than their rounding.
    if [ "$held" -gt $((BUDGET_S + 1)) ]; then
      echo "HELD $name: said its server listens ${held}s after its container started, past the budget (${BUDGET_S}s)"
    else
      echo "SHORT: $name said its server listens ${held}s after its container started, not past the budget (${BUDGET_S}s) by more than the clocks' rounding"
    fi
  fi
  sudo docker update --cpus "$(nproc)" "$name" >/dev/null && echo "UNSTARVED $name"
}

# record NAME EVENT ACTION CMD CONTAINER [AFTER]: keploy records CMD (the app,
# in CONTAINER), its agent's container met by ACTION at its EVENT (watch_agent;
# none when both are empty), while the lane makes the app's N calls; the app
# then exits, and keploy must exit 0 on its own. The calls go as soon as the
# app says it listens (keploy holds the app's port from its bind until its own
# forwarder listens there, so a call in between is answered and recorded),
# after AFTER runs, when given, its output in AFTER.out. A call that fails
# stops keploy, so a failed leg ends.
record() {
  local name=$1 event=$2 action=$3 cmd=$4 container=$5 after=${6:-} log="$1.log" pid rc i before=$FAIL t0 took failed=0
  sudo rm -rf keploy
  : > "$log"
  [ -z "$after" ] || : > "$after.out"
  [ -z "$event" ] || watch_agent "$event" "$action"
  t0=$(date +%s)
  sudo -E env PATH="$PATH" KEPLOY_AGENT_READY_TIMEOUT="$BUDGET_S" "$RECORD_BIN" record -c "$cmd" --container-name "$container" \
    --disable-tele --debug > "$log" 2>&1 &
  pid=$!
  if await "$log" "$pid" "APP LISTENING"; then
    took=$(( $(date +%s) - t0 ))
    echo "$name: the app listened ${took}s after keploy started (a wait bounded by the budget gives up at ${BUDGET_S}s)"
    # The leg's premise: keploy's wait for the agent held past the budget
    # (this counts from keploy's own start, so it bounds that wait from
    # above). A wait over within it would pass a keploy the budget bounds as
    # well. Load only lengthens it.
    if [ "$took" -le "$BUDGET_S" ]; then
      echo "FAIL: $name: the app listened within the budget (${took}s); the leg cannot tell a keploy the budget bounds from one it does not"
      FAIL=1
    fi
    [ -z "$after" ] || { "$after" > "$after.out" 2>&1; cat "$after.out"; }
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
  [ -z "$action" ] || cat "$action.out"
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

echo "== 1. docker compose, the agent's container made with 2% of a CPU, starved from its first instruction =="
record starved "" "" "docker compose -f docker-compose.yml -f agent-cpu.yml up" "cab-app-$ID" unstarve_agent
grep -q "^STARVED " unstarve_agent.out || { echo "FAIL: starved: the agent's container was never seen starved"; FAIL=1; }
if grep -q "^LATE" unstarve_agent.out; then
  echo "FAIL: starved: the lane did not see the agent's start run starved from its first instruction; the leg starved no start"
  FAIL=1
fi
grep -q "^HELD " unstarve_agent.out || { echo "FAIL: starved: the lane did not see the starved start outlast the budget; the leg cannot tell a keploy the budget bounds from one it does not"; FAIL=1; }
grep -q "^UNSTARVED " unstarve_agent.out || { echo "FAIL: starved: the agent's container was not given the host's CPUs back for the app's calls"; FAIL=1; }
saved_all starved
cleanup

echo "== 2. docker run, the agent's container paused for ${HOLD_S}s once it says its server listens, its health check held off =="
# Blocked before keploy picks the agent's port, so before the agent's
# container exists; a block set on the engine's create event raced the
# agent's start, as a quota set on its start event did in the starved leg.
# held.ports names them only once both rules are in. The rules cover the
# whole network namespace, so the lane needs one of its own (a CI job's VM, a
# lane container): every keploy's agent port is in that range. With no block
# in place the leg is not run: its failures would only echo that one.
if ! PORTS=$(agent_ports); then
  echo "FAIL: paused: no ports above the ephemeral range ($(cat /proc/sys/net/ipv4/ip_local_port_range 2>/dev/null)), so keploy's agent port is not one the lane can block before keploy picks it"
  FAIL=1
elif block_port "$PORTS"; then
  echo "$PORTS" > held.ports
else
  echo "FAIL: paused: could not block the ports $PORTS"
  FAIL=1
fi
if [ -f held.ports ]; then
  record paused create hold_after_ready "docker run --rm --name cab-solo-$ID -v $WORK:/w:z -w /w -e N=$N -p $PORT:8080 $IMAGE python solo.py" "cab-solo-$ID"
  if ! grep -q "^UNBLOCKED " hold_after_ready.out; then
    echo "FAIL: paused: the agent was not held: blocked, paused once it said its server listens, unpaused and let go"
    FAIL=1
  fi
  saved_all paused 0
fi
cleanup
# A block left in place would break every later agent in this network
# namespace.
if [ -f blocked.port ]; then
  echo "FAIL: the ports $(cat blocked.port) are still blocked after the leg"
  FAIL=1
fi

if grep -l "WARNING: DATA RACE" ./*.log; then
  echo "FAIL: a data race, in the logs above"
  FAIL=1
fi

if [ "$FAIL" -ne 0 ]; then
  echo "compose agent boot e2e FAILED"
  exit 1
fi
echo "compose agent boot e2e passed"
