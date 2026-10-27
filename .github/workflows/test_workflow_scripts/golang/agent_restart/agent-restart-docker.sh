#!/usr/bin/env bash
# Guard: a keploy agent restarted while the application it records runs on
# goes on recording the application.
#
# That is the Kubernetes sidecar's case. The pod's containers share its PID,
# network and IPC namespaces (shareProcessNamespace), and kubelet restarts a
# crashed or OOM-killed sidecar container in place while the application
# container runs on. Here docker plays the pod: a "pod" container owns the
# namespaces, and the agent (the PR's image, as the k8s sidecar runs it:
# `agent --is-docker`) and the application join them. The agent container is
# restarted twice; the application never is.
#
# The agent's eBPF filter excludes every process that started before the
# recording did (pre-existing sidecars, an Istio proxy). Each agent used to
# stamp its own load time as that start, so after a restart the application,
# older than the new agent, was excluded: from then on every call it made went
# straight to the dependency, not recorded, not mocked, and nothing said so.
#
# The application (probe -call) calls the dependency (probe -serve) over a new
# connection again and again. A recorder (probe -record) plays the keploy
# client: it starts the recording on each agent, the restarted ones too, and
# prints the mocks the agent records. Every call the application starts while
# an agent records must get its answer and come back as a mock: from its
# first call, under the first agent, and after each restart from the first
# call it starts once the recorder has started the recording on the restarted
# agent (whose health check answers only once its hooks are loaded).
#
# Between a restart and the recording, the restarted agent has no session.
# Started with --mode record, it passes the application's connections through
# to their destination, unrecorded: the application's calls there must get
# their answer, and the agent must say it passed them through. A pooled
# connection (probe -keepalive) opened then must be reset when the recording
# starts, so the pool reconnects, and its reconnection must be recorded.
#
# Every wait ends on what it waits for: a line from the probe, the recorder or
# the agent. HANG_GUARD only stops a wait that sees no progress at all, and
# then says what it was waiting for.
#
# Needs: the agent image loaded as $KEPLOY_IMAGE (default
# ghcr.io/keploy/keploy:v3-dev), go, docker, curl, python3.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IMAGE="${KEPLOY_IMAGE:-ghcr.io/keploy/keploy:v3-dev}"
PROBE_IMAGE=keploy-e2e-agent-restart-probe:local
POD=agent-restart-pod
AGENT=agent-restart-agent
APP=agent-restart-app
KA=agent-restart-keepalive
DEP=agent-restart-dependency
AGENT_PORT=16000
# How long a wait may see nothing change before it gives up, in seconds.
HANG_GUARD="${HANG_GUARD:-300}"
[[ "$HANG_GUARD" =~ ^[0-9]+$ ]] || { echo "HANG_GUARD must be a whole number of seconds, not $HANG_GUARD"; exit 2; }
WORK="$(mktemp -d)"
FAIL=0
REC_PID=

fail() {
  echo "::error::$*"
  FAIL=1
}

cleanup() {
  [ -z "$REC_PID" ] || kill "$REC_PID" 2>/dev/null || true
  cat "$WORK"/recorder-*.txt > "$HERE/recorder_logs.txt" 2>/dev/null || true
  { cat "$WORK"/keepalive-*.txt; docker logs "$KA"; } > "$HERE/keepalive_logs.txt" 2>/dev/null || true
  docker logs "$AGENT" > "$HERE/agent_logs.txt" 2>&1 || true
  docker logs "$APP" > "$HERE/app_logs.txt" 2>&1 || true
  docker logs "$DEP" > "$HERE/dependency_logs.txt" 2>&1 || true
  docker rm -f "$APP" "$KA" "$AGENT" "$POD" "$DEP" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT
docker rm -f "$APP" "$KA" "$AGENT" "$POD" "$DEP" >/dev/null 2>&1 || true

# await WHAT PROGRESS CONDITION [ABORT]: waits until the command CONDITION
# succeeds, and fails at once if the command ABORT does. PROGRESS is a command
# whose output changes while what is awaited may still come; only when it has
# printed the same thing for HANG_GUARD seconds does the wait give up, saying
# what it was waiting for and what it last saw.
await() {
  local what=$1 progress=$2 condition=$3 abort=${4:-false} seen='' now='' since
  seen=$(eval "$progress" 2>&1 | tail -c 400) || true
  since=$SECONDS
  until eval "$condition"; do
    if eval "$abort"; then
      now=$(eval "$progress" 2>&1 | tail -c 400) || true
      echo "::error::stopped waiting for $what: ${now:-nothing}"
      return 1
    fi
    now=$(eval "$progress" 2>&1 | tail -c 400) || true
    if [ "$now" != "$seen" ]; then
      seen=$now
      since=$SECONDS
    elif [ $((SECONDS - since)) -ge "$HANG_GUARD" ]; then
      echo "::error::gave up waiting for $what: nothing changed for ${HANG_GUARD}s; last seen: ${seen:-nothing}"
      return 1
    fi
    sleep 0.2
  done
}

(cd "$HERE/probe" && CGO_ENABLED=0 go build -o "$WORK/probe" .)
# The probe is static: no base image to pull.
cat > "$WORK/Dockerfile" <<'DOCKERFILE'
FROM scratch
COPY probe /probe
ENTRYPOINT ["/probe"]
DOCKERFILE
docker build -q -t "$PROBE_IMAGE" "$WORK" >/dev/null

state() { docker inspect -f '{{.State.Status}}' "$1" 2>&1; }

docker run -d --name "$DEP" "$PROBE_IMAGE" -serve 8000 >/dev/null
dep_ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$DEP")
await "the dependency to accept connections" "state $DEP" \
  "python3 -c 'import socket,sys; socket.create_connection((sys.argv[1], 8000), 1)' $dep_ip 2>/dev/null" \
  "[ \"\$(state $DEP)\" != running ]"

# The pod: owns the PID, network and IPC namespaces, as a pod's pause
# container does. The agent image's own sleep, so nothing else is pulled.
docker run -d --name "$POD" --ipc=shareable -p "127.0.0.1:$AGENT_PORT:$AGENT_PORT" \
  --entrypoint sleep "$IMAGE" infinity >/dev/null
docker run -d --name "$AGENT" --privileged \
  --pid="container:$POD" --network="container:$POD" --ipc="container:$POD" \
  -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf \
  "$IMAGE" --port "$AGENT_PORT" --proxy-port 16789 --dns-port 26789 --mode record >/dev/null

# healthy waits for the agent to answer its health check, which it serves
# only once its hooks are loaded. Nothing else it logs is progress toward
# that (the proxy also logs what it does with the application's
# connections), so this wait has only the guard, and stops at once if the
# agent's container is not running.
healthy() {
  await "the agent's health check" "state $AGENT" \
    "curl -fsS --max-time $HANG_GUARD -o /dev/null http://127.0.0.1:$AGENT_PORT/agent/health 2>/dev/null" \
    "[ \"\$(state $AGENT)\" != running ]"
}

# last_call is the number of the application's last finished call, 0 before
# its first.
last_call() {
  docker logs "$APP" 2>/dev/null | awk '$1 == "call" { n = $2 } END { print n + 0 }'
}

# wait_call N waits for the application to finish call N.
wait_call() {
  await "the application's call $1" "docker logs $APP 2>&1 | tail -n 1" "[ \"\$(last_call)\" -ge $1 ]" \
    "[ \"\$(state $APP)\" != running ]"
}

# record N starts recording on the agent that runs now, as a keploy client
# does, and waits until the agent says the recording is on.
record() {
  local out="$WORK/recorder-$1.txt"
  [ -z "$REC_PID" ] || kill "$REC_PID" 2>/dev/null || true
  "$WORK/probe" -record "http://127.0.0.1:$AGENT_PORT/agent" > "$out" 2>&1 &
  REC_PID=$!
  await "the agent to start recording for recorder $1" "cat $out" "grep -qx recording $out" \
    "! kill -0 $REC_PID 2>/dev/null"
}

# mocks_in REC PREFIX FROM TO counts the HTTP mocks recorder REC printed for
# PREFIX/FROM..PREFIX/TO.
mocks_in() {
  awk -v p="$2" -v from="$3" -v to="$4" '$1 == "mock" && $2 == "Http" && match($3, "/[0-9]+$") {
    n = substr($3, RSTART + 1) + 0; pre = substr($3, 1, RSTART - 1)
    if (substr(pre, length(pre) - length(p) + 1) == p && n >= from && n <= to) seen[n] = 1
  } END { c = 0; for (k in seen) c++; print c }' "$WORK/recorder-$1.txt"
}

# wait_mocks REC PREFIX FROM TO waits for recorder REC to print a mock for
# every call PREFIX/FROM..TO. The calls have all ended, so only their own
# mocks are progress: once none has come for HANG_GUARD seconds, the ones
# missing are not coming. A wait that gives up ends the run (see where it is
# called), so no recorder is waited on again after a wait on it gave up.
wait_mocks() {
  await "recorder $1's mocks for $2/$3..$2/$4" "mocks_in $1 $2 $3 $4" \
    "[ \"\$(mocks_in $1 $2 $3 $4)\" -eq $(($4 - $3 + 1)) ]" || true
}

# check FROM TO N LABEL: calls FROM..TO all got their answer and were
# recorded, by recorder N, as mocks. The mock is the proof that the agent's
# proxy carried a call. The application's own verdict (the source port the
# dependency saw) is only a diagnostic: the proxy's port to the dependency
# can be the same number as the application's to the proxy by chance.
check() {
  local n how lost='' failed='' chance=''
  wait_mocks "$3" /call "$1" "$2"
  docker logs "$APP" > "$WORK/app.txt" 2>/dev/null
  for ((n = $1; n <= $2; n++)); do
    how=$(awk -v n="$n" '$1 == "call" && $2 == n { print $3; exit }' "$WORK/app.txt")
    if ! grep -q "^mock Http .*/call/$n\$" "$WORK/recorder-$3.txt"; then
      lost="$lost $n(${how:-not made})"
    elif [ "$how" = error ]; then
      failed="$failed $n"
    elif [ "$how" = direct ]; then
      chance="$chance $n"
    fi
  done
  if [ -n "$lost" ]; then
    fail "$4: calls $1..$2 must all be recorded; these were not (the application's verdict in brackets, direct = it reached the dependency without the agent):$lost"
  fi
  if [ -n "$failed" ]; then
    fail "$4: these calls were recorded but failed in the application:$failed"
  fi
  if [ -n "$chance" ]; then
    echo "$4: calls$chance were recorded though the dependency saw the application's own source port (a chance match of port numbers)"
  fi
  if [ -z "$lost$failed" ]; then
    echo "$4: calls $1..$2 all went through the agent's proxy and were recorded"
  fi
}

# The keep-alive client prints "ka C N proxied|direct|error ..." for call N on
# its connection C (probe -keepalive).
ka_log() { docker logs "$KA" 2>&1; }

# agent_said SINCE TEXT [ALSO] succeeds when the agent has logged a line with
# TEXT (and ALSO) since SINCE. Read whole, then searched: `docker logs |
# grep -q` fails under pipefail when grep stops reading at the match.
agent_said() {
  docker logs --since "$1" "$AGENT" > "$WORK/agent-since.txt" 2>&1 || true
  awk -v a="$2" -v b="${3:-$2}" 'index($0, a) && index($0, b) { f = 1 } END { exit !f }' "$WORK/agent-since.txt"
}

# ka_first_ok succeeds when the keep-alive client's first line is its first
# call on its first connection, answered.
ka_first_ok() {
  ka_log | awk '$1 == "ka" && !seen { seen = 1; ok = ($2 == 1 && $3 == 1 && $4 != "error") } END { exit !ok }'
}

# ka_toward_reconnect counts what moves the keep-alive client toward its
# reconnection: a failed call on its first connection (the reset), and any
# line from a later one. Calls still answered on the first connection are
# not progress: if the reset never comes, the wait sees nothing change.
ka_toward_reconnect() {
  ka_log | awk '$1 == "ka" && ($2 > 1 || $4 == "error") { n++ } END { print n + 0 }'
}

# ka_reconnected succeeds once a call on a later connection got its answer;
# ka_reconnect_failed once one did not, which is the failure to report.
ka_reconnected() {
  ka_log | awk '$1 == "ka" && $2 > 1 && $4 != "error" { f = 1 } END { exit !f }'
}
ka_reconnect_failed() {
  ka_log | awk '$1 == "ka" && $2 > 1 && $4 == "error" { f = 1 } END { exit !f }'
}

# no_session_window RESTART SINCE: the window between the restart and the
# recording, in which the restarted agent has no session yet.
no_session_window() {
  local restart=$1 since=$2 nosess bad conn ka
  # The call after the last finished one may have started before the hooks
  # were loaded; every call after that started after.
  nosess=$(($(last_call) + 2))
  if ! wait_call $((nosess + 2)); then
    fail "restart $restart: the application did not make 3 calls before the recording started"
    return 1
  fi
  bad=$(docker logs "$APP" 2>/dev/null | awk -v from="$nosess" -v to="$((nosess + 2))" \
    '$1 == "call" && $2 >= from && $2 <= to && $3 == "error" { printf "%s; ", $0 }')
  [ -z "$bad" ] || fail "restart $restart: before the recording started, calls $nosess..$((nosess + 2)) must get their answer, passed through by the agent; these failed: $bad"
  # The application is the only process here calling the dependency, and
  # the agent decides per process whether it redirects: this line says it
  # redirected the application, and passed its connections through.
  agent_said "$since" "keploy has no session yet, so this connection is passed through" "\"$dep_ip:8000\"" ||
    fail "restart $restart: the restarted agent did not pass the application's connections through before the recording started (it did not redirect them at all)"

  # A pooled connection opened in the window, still in use when the
  # recording starts: the agent must reset it then, so the pool reconnects
  # and what it sends from then on is recorded.
  docker rm -f "$KA" >/dev/null 2>&1 || true
  if ! docker run -d --name "$KA" --pid="container:$POD" --network="container:$POD" --ipc="container:$POD" \
    "$PROBE_IMAGE" -keepalive "$dep_ip:8000" >/dev/null; then
    fail "restart $restart: the keep-alive client did not start"
    return 1
  fi
  if ! await "the keep-alive client's first call" "ka_log | tail -n 1" \
    "ka_log | awk '\$1 == \"ka\" { f = 1 } END { exit !f }'" "[ \"\$(state $KA)\" != running ]"; then
    fail "restart $restart: the keep-alive client made no call"
    return 1
  fi
  if ! ka_first_ok; then
    fail "restart $restart: the keep-alive client's first call, before the recording started, did not get its answer: $(ka_log | awk '$1 == "ka" { print; exit }')"
    return 1
  fi
  # Its first connection must still be whole when the recording starts, or
  # its next one would be a passthrough too, and never recorded.
  if [ "$(ka_toward_reconnect)" -ne 0 ]; then
    fail "restart $restart: the keep-alive connection broke before the recording started: $(ka_log | tail -n 3 | tr '\n' ';')"
    return 1
  fi

  if ! record "$restart"; then
    fail "restart $restart: the recorder could not start the recording on the restarted agent"
    return 1
  fi
  # The recording resets the keep-alive connection: its next call fails, and
  # the client reconnects. With no deadline on the client's calls, nothing
  # else ends that connection.
  if ! await "the keep-alive connection to be reset when the recording started, and the client to reconnect" \
    ka_toward_reconnect ka_reconnected "ka_reconnect_failed || [ \"\$(state $KA)\" != running ]"; then
    fail "restart $restart: the keep-alive connection opened before the recording was not reset when it started, or its reconnection failed: $(ka_log | tail -n 3 | tr '\n' ';')"
    return 1
  fi
  read -r conn ka < <(ka_log | awk '$1 == "ka" && $2 > 1 && $4 != "error" { print $2, $3 }' | head -n 1) || true
  if [ -z "${conn:-}" ] || [ -z "${ka:-}" ]; then
    fail "restart $restart: could not read the keep-alive client's reconnection back from its log"
    return 1
  fi
  wait_mocks "$restart" "/ka/$conn" "$ka" "$ka"
  if ! grep -q "^mock Http .*/ka/$conn/$ka\$" "$WORK/recorder-$restart.txt"; then
    fail "restart $restart: the keep-alive client's reconnection (call /ka/$conn/$ka) was not recorded"
    return 1
  fi
  agent_said "$since" "the session started: reset the connections keploy had passed through unrecorded" ||
    fail "restart $restart: the restarted agent did not report resetting the passed-through connections when the recording started"
  ka_log > "$WORK/keepalive-$restart.txt"
  docker rm -f "$KA" >/dev/null 2>&1 || true
}

if ! healthy; then
  fail "the agent did not answer its health check"
  docker logs "$AGENT" 2>&1 | tail -n 40
  exit 1
fi
if ! record 0; then
  fail "the recorder could not start the recording on the agent"
  exit 1
fi
# Started once the agent records, as kubelet starts the application container
# once the sidecar's postStart hook says the agent is ready.
docker run -d --name "$APP" --pid="container:$POD" --network="container:$POD" --ipc="container:$POD" \
  "$PROBE_IMAGE" -call "$dep_ip:8000" >/dev/null
if ! wait_call 5; then
  fail "the application did not make 5 calls"
  exit 1
fi
check 1 5 0 "first agent"
# What the restarts are measured against: without it, nothing after can be
# judged.
[ "$FAIL" -eq 0 ] || { echo "AGENT RESTART E2E: FAILED"; exit 1; }

for restart in 1 2; do
  docker restart -t 10 "$AGENT" >/dev/null
  # Only this agent's own lines count: the log of a restarted container
  # holds every earlier run's too. When it started, on the daemon's clock.
  since=$(docker inspect -f '{{.State.StartedAt}}' "$AGENT")
  if ! healthy; then
    fail "restart $restart: the agent did not answer its health check"
    break
  fi
  no_session_window "$restart" "$since" || break
  # The call after the last finished one may have started before the
  # recording was on; every call after that started after.
  from=$(($(last_call) + 2))
  if ! wait_call $((from + 9)); then
    fail "restart $restart: the application did not make 10 more calls"
    break
  fi
  check "$from" $((from + 9)) "$restart" "restart $restart"
  # A restart that failed has said why; another would only take as long.
  [ "$FAIL" -eq 0 ] || break
done

docker logs "$AGENT" 2>&1 | grep -i "recording start boottime" || true

# The agent process that answers names itself in every answer under the proven
# switch (X-Keploy-Agent-Instance), so that keploy can tell an agent started
# again in its place: what a test set is judged on (the calls no mock
# answered, what its tests consumed, what keploy could not capture) is held in
# that process's memory, and a restarted agent answers at the same address
# holding none of it. Here a restarted container is a new process, as a
# sidecar kubelet restarts is: it must name another instance, and one process
# the same one in every answer. With the switch off no answer names one.
#
# answered_as prints the instance the agent's answer to its health check
# names, "" when it names none.
answered_as() {
  curl -fsS --max-time "$HANG_GUARD" -D - -o /dev/null "http://127.0.0.1:$AGENT_PORT/agent/health" |
    tr -d '\r' | awk 'tolower($1) == "x-keploy-agent-instance:" { print $2 }'
}
instances() {
  local first again restarted
  if [ -n "$(answered_as)" ]; then
    fail "with the proven switch off the agent's answers name its process"
    return 1
  fi
  docker rm -f "$AGENT" >/dev/null
  # The agent's switch is its --proven flag (keploy passes it to the agents
  # it starts), never its environment.
  docker run -d --name "$AGENT" --privileged \
    --pid="container:$POD" --network="container:$POD" --ipc="container:$POD" \
    -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf \
    "$IMAGE" --port "$AGENT_PORT" --proxy-port 16789 --dns-port 26789 --mode record --proven >/dev/null
  if ! healthy; then
    fail "with the proven switch on the agent did not answer its health check"
    return 1
  fi
  if ! curl -fsS --max-time "$HANG_GUARD" -D - -o /dev/null "http://127.0.0.1:$AGENT_PORT/agent/health" |
    tr -d '\r' | grep -ix 'x-keploy-proven: on' >/dev/null; then
    fail "the agent started with --proven does not say its proven switch is on"
    return 1
  fi
  first=$(answered_as)
  again=$(answered_as)
  if [ -z "$first" ] || [ "$first" != "$again" ]; then
    fail "with the proven switch on one agent process must name the same instance in every answer; it named '$first', then '$again'"
    return 1
  fi
  docker restart -t 10 "$AGENT" >/dev/null
  if ! healthy; then
    fail "with the proven switch on the restarted agent did not answer its health check"
    return 1
  fi
  restarted=$(answered_as)
  if [ -z "$restarted" ] || [ "$restarted" = "$first" ]; then
    fail "the agent started again must name another instance than the one before ('$first'); it named '$restarted'"
    return 1
  fi
  echo "agent instances: the agent named $first, and started again $restarted; with the switch off, none"
}
[ "$FAIL" -ne 0 ] || instances || true

if [ "$FAIL" -ne 0 ]; then
  echo "--- application ---"
  docker logs "$APP" 2>&1 | tail -n 60
  echo "--- agent ---"
  docker logs "$AGENT" 2>&1 | tail -n 60
  echo "AGENT RESTART E2E: FAILED"
  exit 1
fi
echo "AGENT RESTART E2E: PASSED"
