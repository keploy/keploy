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
# Needs: the agent image loaded as $KEPLOY_IMAGE (default
# ghcr.io/keploy/keploy:v3-dev), go, docker, curl, python3.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IMAGE="${KEPLOY_IMAGE:-ghcr.io/keploy/keploy:v3-dev}"
PROBE_IMAGE=keploy-e2e-agent-restart-probe:local
POD=agent-restart-pod
AGENT=agent-restart-agent
APP=agent-restart-app
DEP=agent-restart-dependency
AGENT_PORT=16000
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
  docker logs "$AGENT" > "$HERE/agent_logs.txt" 2>&1 || true
  docker logs "$APP" > "$HERE/app_logs.txt" 2>&1 || true
  docker logs "$DEP" > "$HERE/dependency_logs.txt" 2>&1 || true
  docker rm -f "$APP" "$AGENT" "$POD" "$DEP" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT
docker rm -f "$APP" "$AGENT" "$POD" "$DEP" >/dev/null 2>&1 || true

(cd "$HERE/probe" && CGO_ENABLED=0 go build -o "$WORK/probe" .)
# The probe is static: no base image to pull.
cat > "$WORK/Dockerfile" <<'DOCKERFILE'
FROM scratch
COPY probe /probe
ENTRYPOINT ["/probe"]
DOCKERFILE
docker build -q -t "$PROBE_IMAGE" "$WORK" >/dev/null

docker run -d --name "$DEP" "$PROBE_IMAGE" -serve 8000 >/dev/null
dep_ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$DEP")
for _ in $(seq 100); do
  python3 -c "import socket,sys; socket.create_connection((sys.argv[1], 8000), 1)" "$dep_ip" 2>/dev/null && break
  sleep 0.2
done

# The pod: owns the PID, network and IPC namespaces, as a pod's pause
# container does. The agent image's own sleep, so nothing else is pulled.
docker run -d --name "$POD" --ipc=shareable -p "127.0.0.1:$AGENT_PORT:$AGENT_PORT" \
  --entrypoint sleep "$IMAGE" infinity >/dev/null
docker run -d --name "$AGENT" --privileged \
  --pid="container:$POD" --network="container:$POD" --ipc="container:$POD" \
  -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf \
  "$IMAGE" --port "$AGENT_PORT" --proxy-port 16789 --dns-port 26789 --mode record >/dev/null

# healthy waits for the agent to answer its health check, which it serves
# only once its hooks are loaded.
healthy() {
  for _ in $(seq 240); do
    if curl -fsS -o /dev/null "http://127.0.0.1:$AGENT_PORT/agent/health" 2>/dev/null; then
      return 0
    fi
    sleep 0.5
  done
  return 1
}

# last_call is the number of the application's last finished call, 0 before
# its first.
last_call() {
  docker logs "$APP" 2>/dev/null | awk '$1 == "call" { n = $2 } END { print n + 0 }'
}

# wait_call N waits for the application to finish call N.
wait_call() {
  for _ in $(seq 600); do
    [ "$(last_call)" -ge "$1" ] && return 0
    sleep 0.2
  done
  return 1
}

# record N starts recording on the agent that runs now, as a keploy client
# does, and waits until the agent says the recording is on.
record() {
  local out="$WORK/recorder-$1.txt"
  [ -z "$REC_PID" ] || kill "$REC_PID" 2>/dev/null || true
  "$WORK/probe" -record "http://127.0.0.1:$AGENT_PORT/agent" > "$out" 2>&1 &
  REC_PID=$!
  for _ in $(seq 300); do
    grep -qx recording "$out" && return 0
    kill -0 "$REC_PID" 2>/dev/null || break
    sleep 0.2
  done
  cat "$out"
  return 1
}

# check FROM TO N LABEL: calls FROM..TO all got their answer and were
# recorded, by recorder N, as mocks. The mock is the proof that the agent's
# proxy carried a call. The application's own verdict (the source port the
# dependency saw) is only a diagnostic: the proxy's port to the dependency
# can be the same number as the application's to the proxy by chance.
check() {
  local n how lost='' failed='' chance=''
  # A mock is sent once its exchange is over, which is about when the
  # application prints the call: wait for the last one.
  for _ in $(seq 150); do
    grep -q "^mock Http .*/call/$2\$" "$WORK/recorder-$3.txt" && break
    sleep 0.2
  done
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

for restart in 1 2; do
  docker restart -t 10 "$AGENT" >/dev/null
  if ! healthy; then
    fail "restart $restart: the agent did not answer its health check"
    break
  fi
  if ! record "$restart"; then
    fail "restart $restart: the recorder could not start the recording on the restarted agent"
    break
  fi
  # The call after the last finished one may have started before the
  # recording was on; every call after that started after.
  from=$(($(last_call) + 2))
  if ! wait_call $((from + 9)); then
    fail "restart $restart: the application did not make 10 more calls"
    break
  fi
  check "$from" $((from + 9)) "$restart" "restart $restart"
done

docker logs "$AGENT" 2>&1 | grep -i "recording start boottime" || true
if [ "$FAIL" -ne 0 ]; then
  echo "--- application ---"
  docker logs "$APP" 2>&1 | tail -n 60
  echo "--- agent ---"
  docker logs "$AGENT" 2>&1 | tail -n 60
  echo "AGENT RESTART E2E: FAILED"
  exit 1
fi
echo "AGENT RESTART E2E: PASSED"
