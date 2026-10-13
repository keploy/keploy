#!/usr/bin/env bash
# e2e for container keys in docker mode (the proven model, KEPLOY_PROVEN=1):
# keploy's agent runs in its own container, which has no Docker socket, so the
# keploy CLI reads the containers from Docker and pushes the list to it. A
# compose app's dependency, reached by its service name at its container IP on
# the project's network, must be recorded with its container's key:
#   container:python:3.11-slim:9411#1
# The same for `docker run`: a dependency on a user network, reached by its
# container name. The compose recording then replays with the dependency
# deleted outright (a smoke test of the docker-mode session start: nothing
# compares keys yet), and a recording made without the switch has no keys.
#
# Env: RECORD_BIN / REPLAY_BIN (the keploy binaries under test). The agent
# image the binaries name must be loaded (.github/actions/download-image).
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-keploy}"
IMAGE=python:3.11-slim
WORK="$(mktemp -d)"
cd "$WORK" || exit 1
FAIL=0
# Names of this run's own: two runs on one runner never share a container.
ID=$$

# Tear down only this run's projects and containers, by name. Never a blanket
# prune.
cleanup() {
  for f in docker-compose.yml docker-compose.nodep.yml; do
    [ -f "$f" ] && sudo docker compose -f "$f" down --remove-orphans >/dev/null 2>&1
  done
  sudo docker rm -f "ckd-dep-$ID" "ckd-runner-$ID" >/dev/null 2>&1
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

# The wrapped test runner: one test's calls to the dependency, by its service
# name. Under the switch (LISTED set) it first waits for keploy's CLI to say,
# at Debug, that it gave the agent a container list with the dependency
# (LISTED, its container name) in it: it reads the run's log (KEPLOY_LANE_LOG,
# in /w) in whole lines. The dependency starts a moment before the runner,
# and without the wait the lane would assert that start race, not the keys.
# The dependency may not listen yet either: /warmup is retried until it
# answers (the job's timeout bounds a dependency that never does), and not
# counted.
cat > runner.py <<'PY'
import json, os, time, urllib.request
AGENT, TOKEN = os.environ.get("KEPLOY_MOCK_AGENT"), os.environ.get("KEPLOY_MOCK_AGENT_TOKEN")
LISTED, LOG = os.environ.get("LISTED"), os.environ.get("KEPLOY_LANE_LOG")
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
                if b"gave keploy's agent the list" in line and LISTED.encode() in line:
                    return
if LISTED:
    listed()
def get(path):
    with urllib.request.urlopen(os.environ.get("DEP_URL", "http://dep:9411") + path) as r:
        return json.load(r)
def scope(path, body):
    h = {"Content-Type": "application/json"}
    if TOKEN:
        h["Authorization"] = "Bearer " + TOKEN
    urllib.request.urlopen(urllib.request.Request(AGENT + path, data=json.dumps(body).encode(), headers=h, method="POST")).read()
while True:
    try:
        get("/warmup")
        break
    except OSError as e:
        print("the dependency does not answer yet:", type(e).__name__, e, flush=True)
        time.sleep(1)
scope("/agent/scope/begin", {"name": "compose_test", "canSkip": True})
for p in ["/price/aapl", "/price/msft"]:
    assert get(p)["path"] == p
scope("/agent/scope/end", {"name": "compose_test", "outcome": "passed"})
print("RUNNER PASSED", flush=True)
PY

runner_service() {
  cat <<YML
  runner:
    image: $IMAGE
    container_name: ckc-runner-$ID
    working_dir: /w
    volumes: [".:/w:z"] # :z relabels on SELinux hosts; ignored elsewhere
    command: python runner.py
    environment:
      - KEPLOY_MOCK_AGENT
      - KEPLOY_MOCK_AGENT_TOKEN
      - LISTED
      - KEPLOY_LANE_LOG
YML
}
{ echo "services:"; runner_service; cat <<YML
    depends_on: [dep]
  dep:
    image: $IMAGE
    container_name: ckc-dep-$ID
    working_dir: /w
    volumes: [".:/w:z"] # :z relabels on SELinux hosts; ignored elsewhere
    command: python dep.py
YML
} > docker-compose.yml
{ echo "services:"; runner_service; } > docker-compose.nodep.yml

# keys_of NAME: "<path> <containerKey or ->" for each priced HTTP mock.
keys_of() {
  sudo cat "keploy/$1/mocks.yaml" 2>/dev/null | python3 -c '
import re, sys
for d in sys.stdin.read().split("\n---"):
    url = re.search(r"^\s+url: (\S+)$", d, re.M)
    if not url or "/price/" not in url.group(1):
        continue
    key = re.search(r"^\s+containerKey: (\S+)$", d, re.M)
    print(re.sub(r"^https?://[^/]+", "", url.group(1)), key.group(1) if key else "-")
' | sort
}

# ran LOG RC WHAT: the leg's keploy exited 0 and its runner passed.
ran() {
  [ "$2" -eq 0 ] || { echo "FAIL: $3: keploy exited $2"; FAIL=1; }
  grep -q "RUNNER PASSED" "$1" || { echo "FAIL: $3: the runner did not pass"; tail -60 "$1"; FAIL=1; }
}

echo "== 1. record a compose project under the proven switch =="
: > rec.log
sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 LISTED="ckc-dep-$ID" KEPLOY_LANE_LOG=/w/rec.log "$RECORD_BIN" mock record \
  -c "docker compose up" --container-name "ckc-runner-$ID" --name ck --disable-tele --debug 2>&1 | tee rec.log
ran rec.log "${PIPESTATUS[0]}" "record under the switch"
keys_of ck | tee keys.txt
printf '%s\n' "/price/aapl container:$IMAGE:9411#1" "/price/msft container:$IMAGE:9411#1" > keys.want
if ! diff -u keys.want keys.txt; then
  echo "FAIL: the compose dependency was not recorded with its container's key"
  grep -iE "container list|containers keploy reads|cannot read the containers" rec.log | tail -5
  FAIL=1
fi
cleanup

echo "== 2. replay with the dependency deleted, under the switch =="
sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$REPLAY_BIN" mock replay \
  -c "docker compose -f docker-compose.nodep.yml up" --container-name "ckc-runner-$ID" --name ck --disable-tele 2>&1 | tee rep.log
ran rep.log "${PIPESTATUS[0]}" "replay under the switch"
cleanup

echo "== 3. record a docker run app under the proven switch =="
sudo docker network create "ckn-$ID" >/dev/null || { echo "FAIL: cannot create the network"; exit 1; }
sudo docker run -d --name "ckd-dep-$ID" --network "ckn-$ID" -v "$PWD:/w:z" -w /w "$IMAGE" python dep.py >/dev/null || { echo "FAIL: cannot start the dependency"; exit 1; }
: > rec-run.log
sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$RECORD_BIN" mock record \
  -c "docker run --rm --name ckd-runner-$ID --network ckn-$ID -v $PWD:/w:z -w /w -e KEPLOY_MOCK_AGENT -e KEPLOY_MOCK_AGENT_TOKEN -e DEP_URL=http://ckd-dep-$ID:9411 -e LISTED=ckd-dep-$ID -e KEPLOY_LANE_LOG=/w/rec-run.log $IMAGE python runner.py" \
  --container-name "ckd-runner-$ID" --name ck-run --disable-tele --debug 2>&1 | tee rec-run.log
ran rec-run.log "${PIPESTATUS[0]}" "docker run record under the switch"
keys_of ck-run | tee keys-run.txt
if ! diff -u keys.want keys-run.txt; then
  echo "FAIL: the docker run dependency was not recorded with its container's key"
  FAIL=1
fi
cleanup

echo "== 4. without the switch nothing is keyed =="
sudo -E env PATH="$PATH" KEPLOY_PROVEN= "$RECORD_BIN" mock record \
  -c "docker compose up" --container-name "ckc-runner-$ID" --name ck-off --disable-tele 2>&1 | tee rec-off.log
ran rec-off.log "${PIPESTATUS[0]}" "record without the switch"
keys_of ck-off | tee keys-off.txt
printf '%s\n' "/price/aapl -" "/price/msft -" > keys-off.want
diff -u keys-off.want keys-off.txt || { echo "FAIL: a recording made without the switch carries container keys"; FAIL=1; }

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
