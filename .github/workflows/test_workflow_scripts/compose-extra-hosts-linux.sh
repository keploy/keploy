#!/usr/bin/env bash
# e2e for an app's extra host entries in docker mode: a compose service's
# `extra_hosts`, and `docker run --add-host` (both spellings). Under keploy the
# app joins the agent container's network namespace, where Docker refuses host
# entries of the app's own ("conflicting options: custom host-to-IP mapping and
# the network mode"), so keploy moves them onto the agent container, whose
# /etc/hosts the app then reads. keploy also gives the agent
# host.docker.internal at the Docker host's gateway (host-gateway), as Docker
# Desktop gives every container, unless the app maps that name itself.
#
# Each leg records an app that reaches its dependency, published on the Docker
# host, by a name only its own host entry gives (deps.keploy-lane.test at
# host-gateway): keploy must start it, the app must reach the dependency
# through keploy, and the recording must hold the dependency's calls under that
# name. The app's /etc/hosts must carry host.docker.internal too.
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
DEP_NAME=deps.keploy-lane.test
source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/free-port.sh"
# The app's port, and the dependency's, on this host.
PORT=$(free_port) || exit 1
DEP_PORT=$(free_port "$PORT") || exit 1

# Tear down only this run's project and containers, by name. Never a blanket
# prune.
cleanup() {
  [ -f docker-compose.yml ] && sudo docker compose down --remove-orphans >/dev/null 2>&1
  sudo docker rm -f "ehd-app-$ID" >/dev/null 2>&1
  return 0
}
finish() {
  cleanup
  sudo docker rm -f "eh-dep-$ID" >/dev/null 2>&1
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
# reaching the dependency at DEP_URL, by the name its host entry gives. It
# says what its /etc/hosts holds for that name and for host.docker.internal
# before it listens, and exits if either is missing: Docker writes /etc/hosts
# when it creates the container, so a name it lacks now it lacks for good, and
# keploy, which ends with the app, then says so at once. The dependency may
# not listen yet: /warmup is retried until it answers (the job's timeout
# bounds one that never does).
cat > app.py <<'PY'
import http.server, os, socket, sys, time, urllib.request
DEP = os.environ["DEP_URL"]
missing = False
for name in (os.environ["DEP_NAME"], "host.docker.internal"):
    try:
        print("RESOLVES %s %s" % (name, socket.gethostbyname(name)), flush=True)
    except OSError as e:
        print("DOES NOT RESOLVE %s: %s" % (name, e), flush=True)
        missing = True
if missing:
    sys.exit(1)
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

# The dependency, published on the Docker host, where the app's host entry
# (host-gateway) leads.
sudo docker run -d --name "eh-dep-$ID" -p "$DEP_PORT:9411" -v "$PWD:/w:z" -w /w "$DEP_IMAGE" python dep.py >/dev/null ||
  { echo "FAIL: cannot start the dependency"; exit 1; }

cat > docker-compose.yml <<YML
services:
  app:
    image: $IMAGE
    container_name: ehc-app-$ID
    working_dir: /w
    volumes: [".:/w:z"] # :z relabels on SELinux hosts; ignored elsewhere
    command: python app.py
    ports: ["$PORT:8080"]
    extra_hosts:
      - "$DEP_NAME:host-gateway"
    environment:
      - DEP_NAME=$DEP_NAME
      - DEP_URL=http://$DEP_NAME:$DEP_PORT
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

# record NAME CMD CONTAINER: keploy records CMD, whose app listens at $PORT,
# while the lane makes two calls through it; it is stopped once both calls'
# test cases are written. The log is NAME.log.
record() {
  local name=$1 cmd=$2 ctr=$3 log="$1.log" pid rc p made before=$FAIL
  sudo rm -rf keploy
  : > "$log"
  sudo -E env PATH="$PATH" "$RECORD_BIN" record -c "$cmd" --container-name "$ctr" \
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
    grep -n "conflicting options\|Error response from daemon" "$log" | head -5
    FAIL=1
  fi
  wait "$pid"
  rc=$?
  [ "$rc" -eq 0 ] || { echo "FAIL: $name: keploy record exited $rc"; FAIL=1; }
  for host in "$DEP_NAME" host.docker.internal; do
    grep -q "RESOLVES $host " "$log" || { echo "FAIL: $name: $host did not resolve in the app"; grep -n "RESOLVE" "$log"; FAIL=1; }
  done
  # The dependency's calls, recorded under the name the app's host entry gave:
  # a /price mock whose request names that host.
  n=$(sudo sh -c 'cat keploy/test-set-*/mocks.yaml' 2>/dev/null | python3 -c '
import re, sys
host = sys.argv[1]
n = 0
for d in sys.stdin.read().split("\n---"):
    if re.search(r"^\s+url: \S*/price/(aapl|msft)$", d, re.M) and re.search(r"^\s+Host: %s$" % re.escape(host), d, re.M | re.I):
        n += 1
print(n)' "$DEP_NAME:$DEP_PORT")
  [ "$n" -eq 2 ] || {
    echo "FAIL: $name: $n of the 2 calls to $DEP_NAME were recorded; the recording's urls and hosts:"
    sudo sh -c 'cat keploy/test-set-*/mocks.yaml' 2>/dev/null | grep -iE '^\s+(url|host): ' | sort | uniq -c
    FAIL=1
  }
  [ "$FAIL" -eq "$before" ] || tail -60 "$log"
}

echo "== 1. record a compose app with extra_hosts =="
record compose "docker compose up" "ehc-app-$ID"
cleanup

echo "== 2. record a docker run app with --add-host NAME:IP =="
record run "docker run --rm --name ehd-app-$ID -p $PORT:8080 --add-host $DEP_NAME:host-gateway -v $PWD:/w:z -w /w -e DEP_NAME=$DEP_NAME -e DEP_URL=http://$DEP_NAME:$DEP_PORT $IMAGE python app.py" "ehd-app-$ID"
cleanup

echo "== 3. record a docker run app with --add-host=NAME:IP =="
record run-eq "docker run --rm --name ehd-app-$ID -p $PORT:8080 --add-host=$DEP_NAME:host-gateway -v $PWD:/w:z -w /w -e DEP_NAME=$DEP_NAME -e DEP_URL=http://$DEP_NAME:$DEP_PORT $IMAGE python app.py" "ehd-app-$ID"
cleanup

if [ "$FAIL" -ne 0 ]; then
  echo "compose-extra-hosts: FAILED"
  exit 1
fi
echo "compose-extra-hosts: all legs passed"
