#!/usr/bin/env bash
# e2e for container keys in docker mode (the proven model, KEPLOY_PROVEN=1):
# keploy's agent runs in its own container, which has no Docker socket, so the
# keploy CLI reads the containers from Docker and gives the agent their list.
# `keploy record` of an app that reaches its dependency
#   - in a compose project, by its service name (the container's IP on the
#     project's network), and
#   - under `docker run`, by its container name on a user network,
# must record each of the app's calls with the dependency's container key:
#   container:python:9411@<fingerprint of its configuration>
# (the dependency's port is not the app's, so a key naming the app fails)
# and a record without the switch has none, and gives the agent no list.
# (`keploy mock` refuses docker commands under the switch in this release, so
# this lane records with `keploy record`. compose-dep-retry-linux.sh's last
# leg covers an agent that compose starts again mid-run.)
#
# Every wait here is on an event: a line in the run's log, read in whole lines,
# with keploy still running. keploy is stopped with SIGINT, as a user stops a
# record, once it has written both calls' test cases.
#
# Env: RECORD_BIN (the keploy binary under test). The agent image the binary
# names must be loaded (.github/actions/download-image).
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true

RECORD_BIN="${RECORD_BIN:-keploy}"
IMAGE=python:3.11-slim
DEP_IMAGE=python:3.11-alpine
WORK="$(mktemp -d)"
cd "$WORK" || exit 1
FAIL=0
# Names of this run's own: two runs on one runner never share a container.
ID=$$
# The app's port on this host.
PORT=$(python3 -c 'import socket; s = socket.socket(); s.bind(("", 0)); print(s.getsockname()[1])')

# Tear down only this run's project and containers, by name. Never a blanket
# prune.
cleanup() {
  [ -f docker-compose.yml ] && sudo docker compose down --remove-orphans >/dev/null 2>&1
  sudo docker rm -f "ckd-dep-$ID" "ckd-app-$ID" >/dev/null 2>&1
  sudo docker network rm "ckn-$ID" >/dev/null 2>&1
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

# The app: answers each GET with its dependency's answer for the same path.
# Under the switch (LISTED set) it first waits for keploy's CLI to say, at
# Debug, that it gave the agent a container list with the dependency (LISTED,
# its container name, quoted as the line lists it) in it, reading the run's log (KEPLOY_LANE_LOG, in /w) in
# whole lines: the dependency starts a moment before the app, and without the
# wait the lane would assert that start race, not the keys. The dependency may
# not listen yet either: /warmup is retried until it answers (the job's
# timeout bounds a dependency that never does). Only /price calls are checked.
cat > app.py <<'PY'
import http.server, os, time, urllib.request
LISTED, LOG, DEP = os.environ.get("LISTED"), os.environ.get("KEPLOY_LANE_LOG"), os.environ["DEP_URL"]
def listed():
    with open(LOG, "rb") as f:
        buf = b""
        while True:
            chunk = f.read(65536)
            if not chunk:
                time.sleep(0.2)
                continue
            *lines, buf = (buf + chunk).split(b"\n")
            for line in lines:
                if b"gave keploy's agent the list" in line and b'"%s"' % LISTED.encode() in line:
                    return
if LISTED:
    listed()
while True:
    try:
        urllib.request.urlopen(DEP + "/warmup").read()
        break
    except OSError as e:
        print("the dependency does not answer yet:", type(e).__name__, e, flush=True)
        time.sleep(1)
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        b = urllib.request.urlopen(DEP + self.path).read()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    def log_message(self, *a): pass
s = http.server.HTTPServer(("0.0.0.0", 8080), H)
print("APP LISTENING", flush=True)
s.serve_forever()
PY

cat > docker-compose.yml <<YML
services:
  app:
    image: $IMAGE
    container_name: ckc-app-$ID
    working_dir: /w
    volumes: [".:/w:z"] # :z relabels on SELinux hosts; ignored elsewhere
    command: python app.py
    ports: ["$PORT:8080"]
    environment:
      - LISTED
      - KEPLOY_LANE_LOG
      - DEP_URL=http://dep:9411
    depends_on: [dep]
  dep:
    image: $DEP_IMAGE
    container_name: ckc-dep-$ID
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

# keys_of: "<path> <containerKey or ->" for each /price mock recorded.
keys_of() {
  sudo sh -c 'cat keploy/test-set-*/mocks.yaml' 2>/dev/null | python3 -c '
import re, sys
for d in sys.stdin.read().split("\n---"):
    url = re.search(r"^\s+url: (\S+)$", d, re.M)
    if not url or "/price/" not in url.group(1):
        continue
    key = re.search(r"^\s+containerKey: (\S+)$", d, re.M)
    print(re.sub(r"^https?://[^/]+", "", url.group(1)), key.group(1) if key else "-")
' | sort
}

# record NAME PROVEN CMD CONTAINER: keploy records CMD, whose app listens at
# $PORT, while the lane makes two calls through it; it is stopped once both
# calls' test cases are written. The log is NAME.log.
record() {
  local name=$1 proven=$2 cmd=$3 ctr=$4 log="$1.log" pid rc p made before=$FAIL
  sudo rm -rf keploy
  : > "$log"
  sudo -E env PATH="$PATH" KEPLOY_PROVEN="$proven" "$RECORD_BIN" record -c "$cmd" --container-name "$ctr" \
    --disable-tele --debug > "$log" 2>&1 &
  pid=$!
  if await "$log" "$pid" "APP LISTENING" 1; then
    made=0
    for p in /price/aapl /price/msft; do
      if curl -fsS -o /dev/null "http://127.0.0.1:$PORT$p"; then
        made=$((made + 1))
      else
        echo "FAIL: $name: GET $p through the app failed"; FAIL=1
      fi
    done
    # A call that failed writes no test case: wait for those of the calls made.
    [ "$made" -eq 0 ] || await "$log" "$pid" "Keploy has captured test cases" "$made" ||
      { echo "FAIL: $name: keploy did not write the test cases of the $made calls made"; FAIL=1; }
    sudo kill -INT "$pid" 2>/dev/null
  else
    echo "FAIL: $name: keploy exited before the app listened"
    FAIL=1
  fi
  wait "$pid"
  rc=$?
  [ "$rc" -eq 0 ] || { echo "FAIL: $name: keploy record exited $rc"; FAIL=1; }
  [ "$FAIL" -eq "$before" ] || {
    tail -60 "$log"
    # A call reset as the app came up: did keploy's ingress forwarder hold
    # the app's port yet? (It starts on the app's bind, after the app does.)
    grep -n "APP LISTENING\|Intercepted application bind event\|Started ingress forwarding" "$log"
  }
}

# ended_at_markers NAME: the recording NAME's stop read each of the agent's
# streams to the agent's end marker, after the agent said it held nothing more
# from before the stop: keploy says so (at debug) for each drain, and neither
# failed to ask for the markers nor gave a drain up. An agent docker compose
# starts (not keploy) ends them so too: the recording names its streams to it.
# keploy's own lines, not the agent's summary: compose forwards the agent's
# log, and can drop its last lines as the stack stops.
ended_at_markers() {
  local log="$1.log" frames
  for frames in "test cases" mocks mappings; do
    if ! grep "read the stream to the keploy agent's end marker" "$log" | grep -q "\"frames\": \"$frames\""; then
      echo "FAIL: $1: the $frames drain did not end at the agent's marker"
      grep -n "end marker\|recording complete" "$log"
      FAIL=1
    fi
  done
  if grep -Eq 'could not ask the keploy agent to end its streams|teardown drain made no progress' "$log"; then
    echo "FAIL: $1: a drain did not end at the agent's markers"
    grep -En 'could not ask the keploy agent to end its streams|teardown drain made no progress' "$log"
    FAIL=1
  fi
}

# expect_keys WHAT WANT...: the /price mocks' keys are WANT's, one per argument.
expect_keys() {
  local what=$1
  shift
  keys_of > keys.txt
  cat keys.txt
  printf '%s\n' "$@" > keys.want
  diff -u keys.want keys.txt || { echo "FAIL: $what"; FAIL=1; }
}

# dep_key WHAT: the key both /price mocks carry, which must be the
# dependency's: its repository (no tag), its port and its configuration's
# fingerprint. It fails (1) otherwise: run in a command substitution, it
# cannot set FAIL itself, so its caller does.
dep_key() {
  local k
  k=$(keys_of | awk '{ print $2 }' | sort -u)
  echo "$k"
  if [ "$(grep -c . <<<"$k")" -ne 1 ] || ! grep -qxE "container:${DEP_IMAGE%%:*}:9411@s[0-9a-f]{12}" <<<"$k"; then
    echo "FAIL: $1: the /price mocks carry '$k', want one key of the dependency's port 9411" >&2
    return 1
  fi
}

echo "== 1. record a compose project under the proven switch =="
LISTED="ckc-dep-$ID" KEPLOY_LANE_LOG=/w/compose.log record compose 1 "docker compose up" "ckc-app-$ID"
ended_at_markers compose
want=$(dep_key "compose") || FAIL=1
expect_keys "the compose dependency was not recorded with its container's key" \
  "/price/aapl $want" "/price/msft $want"
cleanup

echo "== 2. record a docker run app under the proven switch =="
sudo docker network create "ckn-$ID" >/dev/null || { echo "FAIL: cannot create the network"; exit 1; }
sudo docker run -d --name "ckd-dep-$ID" --network "ckn-$ID" -v "$PWD:/w:z" -w /w "$DEP_IMAGE" python dep.py >/dev/null || { echo "FAIL: cannot start the dependency"; exit 1; }
record run 1 "docker run --rm --name ckd-app-$ID --network ckn-$ID -p $PORT:8080 -v $PWD:/w:z -w /w -e DEP_URL=http://ckd-dep-$ID:9411 -e LISTED=ckd-dep-$ID -e KEPLOY_LANE_LOG=/w/run.log $IMAGE python app.py" "ckd-app-$ID"
ended_at_markers run
want=$(dep_key "docker run") || FAIL=1
expect_keys "the docker run dependency was not recorded with its container's key" \
  "/price/aapl $want" "/price/msft $want"
cleanup

echo "== 3. without the switch nothing is keyed =="
LISTED= KEPLOY_LANE_LOG= record compose-off "" "docker compose up" "ckc-app-$ID"
ended_at_markers compose-off
expect_keys "a recording made without the switch carries container keys" "/price/aapl -" "/price/msft -"
if grep -E "gave keploy's agent the list|named a container destination|indexed a container" compose-off.log; then
  echo "FAIL: without the switch keploy read Docker for container keys, or gave its agent a list"
  FAIL=1
fi

# A race build reports a data race and carries on: fail on one in any leg.
if grep -l "WARNING: DATA RACE" ./*.log; then
  echo "FAIL: a data race, in the logs above"
  FAIL=1
fi

if [ "$FAIL" -ne 0 ]; then
  echo "compose container keys e2e FAILED"
  exit 1
fi
echo "compose container keys e2e passed"
