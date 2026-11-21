#!/usr/bin/env bash
# e2e for a compose project whose names are variables, as the parse-server
# sample writes them: the project's name:, the app's container_name, its
# published port and its network's name are each ${VAR:-default}.
# `keploy record --container-name <name>` must find the app's service by the
# name docker compose gives its container, after compose's own interpolation:
#   1. every variable unset: the defaults name the container;
#   2. the project's .env sets the app's container_name: that name, and the
#      default no longer names it;
#   3. a service with no container_name: <project>-<service>-1, the project
#      from a name: with capitals, lowercased as compose lowercases it.
# Each leg records one call and must write its test case; a keploy that
# matches container_name as written ends at once with "container ... not
# found in any of the provided docker-compose files".
#
# Every wait here is on an event: a line in the run's log, read in whole lines,
# with keploy still running. keploy is stopped with SIGINT, as a user stops a
# record, once it has written the call's test case.
#
# Env: RECORD_BIN (the keploy binary under test). The agent image the binary
# names must be loaded (.github/actions/download-image).
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
# shellcheck source=sudo_env.sh
source "$(dirname "${BASH_SOURCE[0]}")/sudo_env.sh" || exit 1
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true

RECORD_BIN="${RECORD_BIN:-keploy}"
IMAGE=python:3.11-slim
WORK="$(mktemp -d)"
cd "$WORK" || exit 1
FAIL=0
# Names of this run's own: two runs on one runner never share a container,
# a project or a network.
ID=$$
source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/free-port.sh"
PORT=$(free_port) || exit 1
# The variables the files name start unset, as a user's shell has them.
unset APP_PROJECT APP_CONTAINER APP_PORT APP_NETWORK COMPOSE_PROJECT_NAME COMPOSE_ENV_FILES

# Tear down only this run's projects, by their files. Never a blanket prune.
cleanup() {
  local f
  for f in docker-compose.yml noname.yml; do
    [ -f "$f" ] && sudo docker compose -f "$f" down --remove-orphans >/dev/null 2>&1
  done
  return 0
}
finish() {
  cleanup
  cd / && sudo rm -rf "$WORK"
}
trap finish EXIT

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/docker-build-retry.sh"
docker_pull_retry "$IMAGE"

cat > app.py <<'PY'
import json, http.server
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        b = json.dumps({"path": self.path}).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    def log_message(self, *a): pass
s = http.server.HTTPServer(("0.0.0.0", 8080), H)
print("APP LISTENING", flush=True)
s.serve_forever()
PY

# The parse-server shape: each name a variable with a default ($ID and $PORT
# are written in now; \${...} is left for compose).
cat > docker-compose.yml <<YML
name: \${APP_PROJECT:-cecn-$ID}
services:
  app:
    image: $IMAGE
    container_name: \${APP_CONTAINER:-cecn-app-$ID}
    working_dir: /w
    volumes: [".:/w:z"] # :z relabels on SELinux hosts; ignored elsewhere
    command: python app.py
    ports: ["\${APP_PORT:-$PORT}:8080"]
    networks: [appnet]
networks:
  appnet:
    name: \${APP_NETWORK:-cecn-net-$ID}
YML

# No container_name: compose names the container <project>-app-1.
cat > noname.yml <<YML
name: \${APP_PROJECT:-Cecn-Proj-$ID}
services:
  app:
    image: $IMAGE
    working_dir: /w
    volumes: [".:/w:z"]
    command: python app.py
    ports: ["\${APP_PORT:-$PORT}:8080"]
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

# names: the containers on this host, by name.
names() { sudo docker ps -a --format '{{.Names}}'; }

# record NAME CMD CONTAINER [NOT]: keploy records CMD, whose app listens at
# $PORT, while the lane makes one call through it; it is stopped once the
# call's test case is written, and that test case must be on disk. While it
# runs, compose must have named a container CONTAINER (the lane's premise)
# and none NOT. The log is NAME.log.
record() {
  local name=$1 cmd=$2 ctr=$3 not=${4:-} log="$1.log" pid rc before=$FAIL n
  sudo rm -rf keploy
  : > "$log"
  sudo env PATH="$PATH" "${SUDO_ENV[@]}" "$RECORD_BIN" record -c "$cmd" --container-name "$ctr" \
    --disable-tele --debug > "$log" 2>&1 &
  pid=$!
  if await "$log" "$pid" "APP LISTENING" 1; then
    if curl -fsS -o /dev/null "http://127.0.0.1:$PORT/hello/$name"; then
      await "$log" "$pid" "Keploy has captured test cases" 1 ||
        { echo "FAIL: $name: keploy did not write the call's test case"; FAIL=1; }
      names | grep -qxF "$ctr" || { echo "FAIL: $name: compose named no container $ctr"; names; FAIL=1; }
      if [ -n "$not" ] && names | grep -qxF "$not"; then
        echo "FAIL: $name: compose also named a container $not"; FAIL=1
      fi
    else
      echo "FAIL: $name: GET /hello/$name through the app failed"; FAIL=1
    fi
    sudo kill -INT "$pid" 2>/dev/null
  else
    echo "FAIL: $name: keploy exited before the app listened"
    FAIL=1
  fi
  wait "$pid"
  rc=$?
  [ "$rc" -eq 0 ] || { echo "FAIL: $name: keploy record exited $rc"; FAIL=1; }
  n=$(sudo sh -c 'ls keploy/test-set-*/tests/*.yaml 2>/dev/null' | grep -c .)
  [ "$n" -ge 1 ] || { echo "FAIL: $name: no test case on disk"; FAIL=1; }
  if grep -a "not found in any of the provided docker-compose files" "$log"; then
    echo "FAIL: $name: keploy did not find the container compose named $ctr"
    FAIL=1
  fi
  verify_ran "" "$log" || FAIL=$((FAIL + 1))
  [ "$FAIL" -eq "$before" ] || tail -60 "$log"
}

echo "== 1. every variable unset: the defaults name the container =="
record defaults "docker compose up" "cecn-app-$ID"
cleanup

echo "== 2. the project's .env names the container =="
printf 'APP_CONTAINER=cecn-dotenv-%s\n' "$ID" > .env
record dotenv "docker compose up" "cecn-dotenv-$ID" "cecn-app-$ID"
cleanup
rm -f .env

echo "== 3. no container_name: <project>-<service>-1, the project lowercased =="
record noname "docker compose -f noname.yml up" "cecn-proj-$ID-app-1"
cleanup

# A race build reports a data race and carries on: fail on one in any leg.
if grep -l "WARNING: DATA RACE" ./*.log; then
  echo "FAIL: a data race, in the logs above"
  FAIL=1
fi

if [ "$FAIL" -ne 0 ]; then
  echo "compose env container name e2e FAILED"
  exit 1
fi
echo "compose env container name e2e passed"
