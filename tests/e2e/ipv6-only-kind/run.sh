#!/usr/bin/env bash
#
# A keploy agent records and replays an application on a single-stack IPv6
# Kubernetes cluster: kind with ipFamily ipv6, where a Pod has no IPv4 address
# but loopback's.
#
# WHAT BROKE. A Kubernetes sidecar agent runs `keploy agent --is-docker` (the
# image's entrypoint). As its hooks load, it picks the address the kernel
# sends the application's connections to: its container's own. keploy looked
# only for an IPv4 one (utils.GetContainerIPv4), found none in an IPv6-only
# Pod, and exited "could not find a non-loopback IP for the container". The
# sidecar crash-looped and the application never started, so nothing was
# recorded or replayed there. keploy now picks the Pod's IPv6 address when it
# has no IPv4 one, and the eBPF hooks send the application's IPv6 connections
# (and its DNS) there.
#
# THE WORKLOAD. In namespace v6: a dependency (harness dep) behind Service
# dep, and the application (harness app), whose GET /greet?n=N calls the
# dependency by name (dep.v6.svc.cluster.local.) and answers "hello" and what
# it said. The application's Pod runs keploy's agent as a native sidecar
# (an init container that keeps running), with a startup probe on the agent's
# health, which answers only once its hooks are loaded: the application starts
# after that, as the agent records only processes started after its load. A
# driver Pod (harness record / replay) plays keploy's client as Kubernetes
# tooling drives a sidecar agent: over the agent's API at the Pod's IPv6
# address, with keploy's own client (pkg/platform/http).
#
# WHAT IS ASSERTED
#   C0  the cluster is IPv6-only: a Pod has one IP, IPv6, and inside one no
#       up interface has an IPv4 address; the Services are IPv6 only;
#   A0  the recording agent starts (its hooks load) in such a Pod;
#   A1  it runs with the driver's proven switch (KEPLOY_PROVEN, below);
#   R1  each /greet the driver sends the application over IPv6 gets the
#       dependency's answer;
#   R2  the agent records a test case for each, and a mock of the
#       application's call to the dependency for each: the call went to the
#       agent's proxy, over the IPv6 redirect, and was captured;
#   P0  with the dependency and its Service deleted, a replaying agent (in a
#       new Pod of the application) starts;
#   P1  it runs with the driver's proven switch;
#   P2  every recorded test case, sent to the application again, gets the
#       recorded answer: its call to the dependency was answered from the
#       mocks, its name too (no Service is left to answer it).
#   Against an agent image built before the fix, A0 fails: the sidecar exits
#   at start with "could not find a non-loopback IP for the container", which
#   the failure quotes.
#
# THE DOCKER HOST. kind makes an IPv6 cluster only where IPv6 is on in the
# network namespace of the Docker daemon it drives. This script never changes
# that setting: it reads it (with a throwaway container on the daemon's host
# network) and stops, saying so, when it is off. The cluster gets a Docker
# network of its own (KIND_EXPERIMENTAL_DOCKER_NETWORK): kind reuses an
# existing network as it is, and one an IPv4 cluster made earlier on the same
# daemon may have no IPv6.
#
# THE PROVEN SWITCH. With KEPLOY_PROVEN=1 the run is under the proven
# model's staging switch, as keploy runs it: the agents get --proven (and
# --exact-view-verify with KEPLOY_EXACT_VIEW_VERIFY=1, as
# provenswitch.AgentArgs passes them), and the driver, keploy's client,
# both variables. The proven model tracks each connection between the
# application and the agent's proxy, and on an IPv6-only Pod every one is
# native IPv6 (on an IPv4 one each is IPv4), so CI runs this script with the
# switch unset and with it on.
#
# Env:
#   KEPLOY_IMAGE (ghcr.io/keploy/keploy:v3-dev)  the agent image under test
#   KEPLOY_PROVEN (unset)  1: the proven switch on; unset, empty or 0: off
#   KEPLOY_EXACT_VIEW_VERIFY (unset)  1: under the switch, the agents' exact
#                 view verify mode
#   CLUSTER_NAME (keploy-ipv6only)  KEEP_CLUSTER (0)  KIND_NODE_IMAGE
#   HARNESS_BIN   a prebuilt linux harness (else built here with go)
#   ARTIFACTS     where the Pods' logs are kept (default: ./artifacts here)
#   N_CALLS (3)   the /greet calls recorded
#   AGENT_WAIT (180)  seconds an agent has to load its hooks
#   AGENT_DEBUG (0)   1: the agents log at debug level

set -euo pipefail

for bin in kind kubectl docker; do
  command -v "$bin" >/dev/null 2>&1 || { echo "FATAL: required tool '$bin' not found" >&2; exit 1; }
done

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
AGENT_IMAGE="${KEPLOY_IMAGE:-ghcr.io/keploy/keploy:v3-dev}"
CLUSTER_NAME="${CLUSTER_NAME:-keploy-ipv6only}"
KEEP_CLUSTER="${KEEP_CLUSTER:-0}"
N_CALLS="${N_CALLS:-3}"
AGENT_WAIT="${AGENT_WAIT:-180}"
DEBUG_ARG=""
[ "${AGENT_DEBUG:-0}" != 1 ] || DEBUG_ARG=', "--debug"'
case "${KEPLOY_PROVEN:-}" in
  "" | 0) PROVEN=off ;;
  1) PROVEN=on ;;
  *) echo "FATAL: KEPLOY_PROVEN=$KEPLOY_PROVEN: set it to 1, or leave it unset" >&2; exit 1 ;;
esac
PROVEN_ARGS=""
if [ "$PROVEN" = on ]; then
  PROVEN_ARGS=', "--proven"'
  [ "${KEPLOY_EXACT_VIEW_VERIFY:-}" != 1 ] || PROVEN_ARGS+=', "--exact-view-verify"'
fi
ARTIFACTS="${ARTIFACTS:-$SCRIPT_DIR/artifacts}"
HARNESS_IMAGE=keploy-e2e-ipv6only-harness:local
NS=v6
DEP_HOST=dep.$NS.svc.cluster.local.
APP_URL=http://greeter.$NS.svc.cluster.local.:8080
TOKEN="dep$RANDOM$RANDOM"
rm -rf "$ARTIFACTS"; mkdir -p "$ARTIFACTS"
WORK="$(mktemp -d)"

PASS=0; FAIL=0; FAIL_REASONS=()
say()  { echo "$(date -u +%FT%TZ) [ipv6only] $*"; }
pass() { PASS=$((PASS + 1)); echo "[ipv6only] PASS: $*"; }
fail() { FAIL=$((FAIL + 1)); FAIL_REASONS+=("$*"); echo "[ipv6only] FAIL: $*" >&2; }
finish() {
  echo
  echo "[ipv6only] PASS=$PASS FAIL=$FAIL  (Pod logs: $ARTIFACTS)"
  if [ "$FAIL" -ne 0 ]; then
    printf '[ipv6only] FAILED: %s\n' "${FAIL_REASONS[@]}" >&2
    exit 1
  fi
  exit 0
}
abort() { fail "$*"; finish; }

export KUBECONFIG="$WORK/kubeconfig"
export KIND_EXPERIMENTAL_DOCKER_NETWORK="${KIND_EXPERIMENTAL_DOCKER_NETWORK:-kind-$CLUSTER_NAME}"
K="kubectl --context kind-$CLUSTER_NAME"
CREATED=0

# collect keeps every Pod's description and its containers' logs, the
# previous run's of a restarted container too.
collect() {
  [ "$CREATED" = 1 ] || return 0
  $K get pods -A -o wide > "$ARTIFACTS/pods.txt" 2>&1 || true
  $K -n "$NS" get events --sort-by=.lastTimestamp > "$ARTIFACTS/events.txt" 2>&1 || true
  local p c
  for p in $($K -n "$NS" get pods -o name 2>/dev/null); do
    p=${p#pod/}
    $K -n "$NS" describe pod "$p" > "$ARTIFACTS/describe-$p.txt" 2>&1 || true
    for c in $($K -n "$NS" get pod "$p" -o jsonpath='{.spec.initContainers[*].name} {.spec.containers[*].name}' 2>/dev/null); do
      $K -n "$NS" logs "$p" -c "$c" > "$ARTIFACTS/$p-$c.log" 2>&1 || true
      $K -n "$NS" logs "$p" -c "$c" --previous > "$ARTIFACTS/$p-$c-previous.log" 2>/dev/null || rm -f "$ARTIFACTS/$p-$c-previous.log"
    done
  done
}

# shellcheck disable=SC2317,SC2329 # run by the EXIT trap
cleanup() {
  collect
  if [ "$KEEP_CLUSTER" != "1" ] && [ "$CREATED" = 1 ]; then
    kind delete cluster --name "$CLUSTER_NAME" >/dev/null 2>&1 || true
    docker network rm "$KIND_EXPERIMENTAL_DOCKER_NETWORK" >/dev/null 2>&1 || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

say "agent image $AGENT_IMAGE; proven switch $PROVEN${PROVEN_ARGS:+ (agent args${PROVEN_ARGS#,})}"

# ---- 1. the harness image ----------------------------------------------------
mkdir -p "$WORK/img"
if [ -n "${HARNESS_BIN:-}" ]; then
  cp "$HARNESS_BIN" "$WORK/img/harness"
else
  command -v go >/dev/null 2>&1 || { echo "FATAL: set HARNESS_BIN or put go on PATH" >&2; exit 1; }
  (cd "$REPO_ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$WORK/img/harness" ./tests/e2e/ipv6-only-kind/harness)
fi
# Static: no base image to pull.
printf 'FROM scratch\nCOPY harness /harness\nENTRYPOINT ["/harness"]\n' > "$WORK/img/Dockerfile"
docker build -q -t "$HARNESS_IMAGE" "$WORK/img" >/dev/null

# ---- 2. the Docker host, then the cluster -------------------------------------
v6off=$(docker run --rm --network host --entrypoint cat "$AGENT_IMAGE" /proc/sys/net/ipv6/conf/all/disable_ipv6 2>/dev/null || echo unreadable)
[ "$v6off" = 0 ] || abort "C0: IPv6 is off in the Docker daemon's network namespace (net.ipv6.conf.all.disable_ipv6: $v6off), so kind cannot make an IPv6 cluster there"

if kind get clusters 2>/dev/null | grep -qx "$CLUSTER_NAME"; then
  say "deleting the leftover cluster $CLUSTER_NAME"
  kind delete cluster --name "$CLUSTER_NAME" >/dev/null 2>&1 || true
fi
# The API server is published on the Docker host's IPv4 loopback, so kubectl
# here needs no IPv6 of its own (it listens on "::"; kind puts the address in
# its certificate).
cat > "$WORK/kind.yaml" <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  ipFamily: ipv6
  apiServerAddress: "127.0.0.1"
EOF
say "creating cluster $CLUSTER_NAME (ipFamily ipv6; docker network $KIND_EXPERIMENTAL_DOCKER_NETWORK)"
CREATED=1
kind create cluster --name "$CLUSTER_NAME" --config "$WORK/kind.yaml" \
  ${KIND_NODE_IMAGE:+--image "$KIND_NODE_IMAGE"} --kubeconfig "$KUBECONFIG" --wait 180s >/dev/null
# The agent mounts the node's /sys/fs/bpf; a fresh kind node has the
# directory but not the mount.
for node in $(kind get nodes --name "$CLUSTER_NAME"); do
  docker exec "$node" sh -c 'mountpoint -q /sys/fs/bpf || mount -t bpf bpf /sys/fs/bpf'
done
say "loading images"
kind load docker-image --name "$CLUSTER_NAME" "$AGENT_IMAGE" "$HARNESS_IMAGE" >/dev/null

# ---- 3. the workload ----------------------------------------------------------
# app_pod NAME MODE: the application's Pod, keploy's agent (MODE record or
# test) its native sidecar.
app_pod() {
  cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $1
  namespace: $NS
  labels: {app: greeter}
spec:
  shareProcessNamespace: true
  terminationGracePeriodSeconds: 5
  initContainers:
    - name: keploy-agent
      image: $AGENT_IMAGE
      imagePullPolicy: Never
      restartPolicy: Always
      args: ["--port", "16000", "--proxy-port", "16789", "--dns-port", "26789", "--mode", "$2"$PROVEN_ARGS$DEBUG_ARG]
      # Its control-plane token, from a Secret, as k8s-proxy gives every
      # agent it starts one: an agent started without one refuses to start.
      env:
        - name: KEPLOY_AGENT_TOKEN
          valueFrom: {secretKeyRef: {name: agent-token, key: token}}
      securityContext: {privileged: true}
      startupProbe:
        httpGet: {path: /agent/health, port: 16000}
        periodSeconds: 1
        failureThreshold: $AGENT_WAIT
      volumeMounts:
        - {name: cgroup, mountPath: /sys/fs/cgroup}
        - {name: debugfs, mountPath: /sys/kernel/debug}
        - {name: bpffs, mountPath: /sys/fs/bpf}
  containers:
    - name: app
      image: $HARNESS_IMAGE
      imagePullPolicy: Never
      args: ["app", ":8080", "http://$DEP_HOST:8000/name"]
  volumes:
    - {name: cgroup, hostPath: {path: /sys/fs/cgroup}}
    - {name: debugfs, hostPath: {path: /sys/kernel/debug}}
    - {name: bpffs, hostPath: {path: /sys/fs/bpf}}
EOF
}

$K create namespace "$NS" >/dev/null
# The agents' control-plane token: the sidecars enforce it, and the driver,
# which drives them through keploy's own client, presents it (keploy adopts
# an inherited KEPLOY_AGENT_TOKEN as its session token).
$K -n "$NS" create secret generic agent-token \
  --from-literal=token="$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')" >/dev/null
$K apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: {name: dep, namespace: $NS, labels: {app: dep}}
spec:
  terminationGracePeriodSeconds: 1
  containers:
    - {name: dep, image: $HARNESS_IMAGE, imagePullPolicy: Never, args: ["dep", ":8000", "$TOKEN"]}
---
apiVersion: v1
kind: Service
metadata: {name: dep, namespace: $NS}
spec:
  selector: {app: dep}
  ports: [{port: 8000, targetPort: 8000}]
---
apiVersion: v1
kind: Service
metadata: {name: greeter, namespace: $NS}
spec:
  selector: {app: greeter}
  ports: [{port: 8080, targetPort: 8080}]
---
apiVersion: v1
kind: Pod
metadata: {name: driver, namespace: $NS}
spec:
  terminationGracePeriodSeconds: 1
  containers:
    - name: driver
      image: $HARNESS_IMAGE
      imagePullPolicy: Never
      args: ["idle"]
      env:
        - name: KEPLOY_AGENT_TOKEN
          valueFrom: {secretKeyRef: {name: agent-token, key: token}}
        - {name: KEPLOY_PROVEN, value: "${KEPLOY_PROVEN:-}"}
        - {name: KEPLOY_EXACT_VIEW_VERIFY, value: "${KEPLOY_EXACT_VIEW_VERIFY:-}"}
      volumeMounts: [{name: work, mountPath: /work}]
  volumes: [{name: work, emptyDir: {}}]
EOF
$K -n "$NS" wait --for=condition=Ready pod/dep pod/driver --timeout=180s >/dev/null ||
  abort "the dependency and driver Pods did not become ready"

# ---- C0: the cluster is IPv6-only ----------------------------------------------
pod_ips=$($K -n "$NS" get pod dep -o jsonpath='{range .status.podIPs[*]}{.ip}{" "}{end}')
svc_ips=$($K -n "$NS" get svc dep -o jsonpath='{range .spec.clusterIPs[*]}{@}{" "}{end}')
fam() { local a out=""; for a in "$@"; do case "$a" in *:*) out+="ipv6 " ;; *) out+="ipv4 " ;; esac; done; echo "${out% }"; }
# shellcheck disable=SC2086 # split into the addresses on purpose
pod_fams=$(fam $pod_ips) svc_fams=$(fam $svc_ips)
addrs=$($K -n "$NS" exec dep -- /harness addrs | tr '\n' ' ')
if [ "$pod_fams" = ipv6 ] && [ "$svc_fams" = ipv6 ] && [[ "$addrs" == *"ipv4 0 "* ]] && [[ "$addrs" != *"ipv6-routable 0 "* ]]; then
  pass "C0: the cluster is IPv6-only (a Pod's IPs: $pod_fams; a Service's: $svc_fams; in a Pod: ${addrs% })"
else
  abort "C0: the cluster is not IPv6-only (a Pod's IPs: [$pod_fams]; a Service's: [$svc_fams]; in a Pod: ${addrs% }), so this run proves nothing"
fi

# agent_started POD: waits for the agent sidecar to have loaded its hooks (its
# startup probe passed), and fails at once, quoting it, when it exits saying
# it has no address to be reached at.
agent_started() {
  local p=$1 since=$SECONDS started restarts
  while [ $((SECONDS - since)) -lt "$AGENT_WAIT" ]; do
    started=$($K -n "$NS" get pod "$p" -o jsonpath='{.status.initContainerStatuses[0].started}' 2>/dev/null || true)
    [ "$started" = true ] && return 0
    restarts=$($K -n "$NS" get pod "$p" -o jsonpath='{.status.initContainerStatuses[0].restartCount}' 2>/dev/null || true)
    if [ "${restarts:-0}" -ge 1 ]; then
      AGENT_SAID=$({ $K -n "$NS" logs "$p" -c keploy-agent --previous 2>/dev/null || true; } |
        grep -m1 -o 'could not find a non-loopback IP for the container[^"]*' || true)
      if [ -n "$AGENT_SAID" ]; then
        return 1
      fi
    fi
    sleep 1
  done
  AGENT_SAID="its hooks were not loaded within ${AGENT_WAIT}s (restarts: ${restarts:-0})"
  return 1
}

# app_ready POD: the application container runs, so its Pod is ready.
app_ready() {
  $K -n "$NS" wait --for=condition=Ready "pod/$1" --timeout=120s >/dev/null
}

agent_ip() { $K -n "$NS" get pod "$1" -o jsonpath='{.status.podIP}'; }

# ---- record ---------------------------------------------------------------------
app_pod app-record record | $K apply -f - >/dev/null
AGENT_SAID=""
if agent_started app-record; then
  pass "A0: the recording agent loaded its hooks in an IPv6-only Pod"
else
  abort "A0: the recording agent did not start in an IPv6-only Pod: $AGENT_SAID"
fi
app_ready app-record || abort "the application did not start beside the recording agent"

say "recording $N_CALLS calls"
rc=0
$K -n "$NS" exec driver -- /harness record "$(agent_ip app-record)" "$APP_URL" "$N_CALLS" /work/rec.gob \
  > "$ARTIFACTS/record.txt" 2>&1 || rc=$?
cat "$ARTIFACTS/record.txt"
if grep -qx "proven $PROVEN" "$ARTIFACTS/record.txt"; then
  pass "A1: the recording agent runs with the driver's proven switch ($PROVEN)"
else
  fail "A1: the recording agent does not run with the driver's proven switch ($PROVEN)"
fi
calls=0 tests=0 mocks=0
for ((i = 1; i <= N_CALLS; i++)); do
  grep -qx "call $i 200 hello $TOKEN-$i" "$ARTIFACTS/record.txt" && calls=$((calls + 1))
  grep -qx "testcase $i GET 200 hello $TOKEN-$i" "$ARTIFACTS/record.txt" && tests=$((tests + 1))
  grep -qx "mock $i Http $TOKEN-$i" "$ARTIFACTS/record.txt" && mocks=$((mocks + 1))
done
if [ "$calls" -eq "$N_CALLS" ]; then
  pass "R1: every /greet sent over IPv6 got the dependency's answer ($calls of $N_CALLS)"
else
  fail "R1: $calls of $N_CALLS /greet calls got the dependency's answer"
fi
if [ "$rc" -eq 0 ] && [ "$tests" -eq "$N_CALLS" ] && [ "$mocks" -eq "$N_CALLS" ]; then
  pass "R2: the agent recorded a test case ($tests) and a mock of the dependency's call ($mocks) for each call"
else
  fail "R2: the agent recorded $tests test cases and $mocks mocks of the dependency's call for $N_CALLS calls (record exited $rc)"
fi
[ "$FAIL" -eq 0 ] || finish

# ---- replay ---------------------------------------------------------------------
# Nothing but the mocks can answer the application now: the dependency and
# its name are gone. The recording Pods' logs are kept first.
collect
$K -n "$NS" delete pod app-record dep --wait=true >/dev/null
$K -n "$NS" delete svc dep --wait=true >/dev/null
app_pod app-replay test | $K apply -f - >/dev/null
AGENT_SAID=""
if agent_started app-replay; then
  pass "P0: the replaying agent loaded its hooks in an IPv6-only Pod, with the dependency gone"
else
  abort "P0: the replaying agent did not start in an IPv6-only Pod: $AGENT_SAID"
fi
app_ready app-replay || abort "the application did not start beside the replaying agent"

say "replaying"
rc=0
$K -n "$NS" exec driver -- /harness replay "$(agent_ip app-replay)" "$APP_URL" /work/rec.gob \
  > "$ARTIFACTS/replay.txt" 2>&1 || rc=$?
cat "$ARTIFACTS/replay.txt"
if grep -qx "proven $PROVEN" "$ARTIFACTS/replay.txt"; then
  pass "P1: the replaying agent runs with the driver's proven switch ($PROVEN)"
else
  fail "P1: the replaying agent does not run with the driver's proven switch ($PROVEN)"
fi
passed=0
for ((i = 1; i <= N_CALLS; i++)); do
  grep -qx "test $i pass 200 hello $TOKEN-$i" "$ARTIFACTS/replay.txt" && passed=$((passed + 1))
done
if [ "$rc" -eq 0 ] && [ "$passed" -eq "$N_CALLS" ]; then
  pass "P2: every recorded test case got its recorded answer, the dependency's call answered from the mocks ($passed of $N_CALLS)"
else
  fail "P2: $passed of $N_CALLS recorded test cases got their recorded answer (replay exited $rc)"
fi
finish
