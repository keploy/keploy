#!/usr/bin/env bash
# Self-contained e2e for `keploy mock record|replay` against a DOCKER COMPOSE
# project. Compose is the one command type where the agent lives inside the
# thing keploy is wrapping — it is a service in the project keploy generates —
# so the app cannot be started last the way it is natively, and the app service
# is instead held at the agent's healthcheck until keploy has armed the proxy.
#
# That inversion had no coverage at all, which is how both subcommands came to
# be broken end to end without anything noticing. This lane guards that the
# compose path works at all; the ORDER of the agent calls is pinned by
# TestComposeOrdering in pkg/service/mock, which can see it directly.
#
# Env: RECORD_BIN / REPLAY_BIN (the keploy binaries under test), set by the
# calling workflow from .github/actions/download-binary's `path` output. The
# agent image must be loaded as ghcr.io/keploy/keploy:v3-dev —
# .github/actions/download-image does that; a source build reports version
# 3-dev and that tag is not published, so without it the compose path cannot
# start an agent at all.
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-keploy}"
WORK="$(mktemp -d)"
cd "$WORK" || exit 1
FAIL=0

# Tear down only this project, by name. Never a blanket prune.
cleanup() {
  for f in docker-compose.yml docker-compose.nodep.yml docker-compose.fail.yml docker-compose.miss.yml docker-compose.linger.yml; do
    [ -f "$f" ] && sudo docker compose -f "$f" down --remove-orphans >/dev/null 2>&1
  done
}
trap cleanup EXIT

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

# The wrapped "test runner": makes its dependency calls and exits, which is
# what ends a mock recording. It retries so the first call does not race the
# dependency service's own startup.
cat > runner.py <<'PY'
import json, os, sys, time, urllib.request
DEP = os.environ.get("DEP_URL", "http://dep:9411")
EXIT = int(os.environ.get("RUNNER_EXIT", "0"))

def get(path):
    with urllib.request.urlopen(DEP + path, timeout=5) as r:
        return json.load(r)

# The agent must refuse a control-plane caller with no credential. Its token
# reaches it through compose: the generated agent service names it without a
# value and compose copies it from the environment keploy runs compose in. That
# handoff fails OPEN — an agent that got no token serves everything and every
# assertion below still passes — so it is checked here directly, from inside
# the agent's network namespace, at the address keploy exports for exactly
# this kind of caller.
import urllib.error
AGENT = os.environ.get("KEPLOY_MOCK_AGENT")
if not AGENT:
    print("FAILED: keploy did not export KEPLOY_MOCK_AGENT", flush=True)
    sys.exit(1)
try:
    urllib.request.urlopen(urllib.request.Request(
        AGENT + "/agent/scope/begin", data=json.dumps({"name": "unauthenticated-probe"}).encode(),
        headers={"Content-Type": "application/json"}, method="POST"), timeout=5).read()
    print("FAILED: the agent served /agent/scope/begin to a caller with no token", flush=True)
    sys.exit(1)
except urllib.error.HTTPError as e:
    if e.code != 401:
        print("FAILED: expected 401 for an unauthenticated scope call, got", e.code, flush=True)
        sys.exit(1)
print("AGENT REFUSED AN UNAUTHENTICATED CALLER", flush=True)

# keploy rewrites the app's depends_on to condition: service_started, so the
# dependency may not be listening yet. Retry on /warmup, which the assertions
# below do NOT count, so a retry can never inflate the recorded mock count.
for attempt in range(30):
    try:
        get("/warmup")
        break
    except Exception as e:
        if attempt == 29:
            print("FAILED to reach the dependency:", type(e).__name__, e, flush=True)
            sys.exit(1)
        time.sleep(1)

for p in ["/price/aapl", "/price/msft", "/price/goog"]:
    body = get(p)
    assert body["path"] == p, body
    print("ok", p, body, flush=True)
# A call the recording does not have, made only when asked. The runner does not
# depend on its answer: it is there for keploy to report as missed.
if os.environ.get("UNRECORDED"):
    try:
        get(os.environ["UNRECORDED"])
    except Exception as e:
        print("unrecorded call failed as expected:", type(e).__name__, flush=True)
print("RUNNER PASSED 3", flush=True)
# Stay up after the calls when asked, for a step that needs the runner alive
# while something else happens to the project.
time.sleep(int(os.environ.get("LINGER", "0")))
sys.exit(EXIT)
PY

# $1, if given, is one more environment entry for the runner.
runner_service() {
  cat <<YML
  runner:
    image: python:3.11-slim
    container_name: mockc-runner
    working_dir: /w
    volumes: [".:/w"]
    command: python runner.py
    environment:
      # Passed through by name, as a user's own compose file would: the
      # runner needs the agent's address to call its control plane.
      - KEPLOY_MOCK_AGENT
YML
  if [ -n "${1:-}" ]; then echo "      - $1"; fi
}

{ echo "services:"; runner_service; cat <<'YML'
    depends_on: [dep]
  dep:
    image: python:3.11-slim
    container_name: mockc-dep
    working_dir: /w
    volumes: [".:/w"]
    command: python dep.py
YML
} > docker-compose.yml

# Replay runs the SAME runner with the dependency service deleted outright, so
# every answer it gets can only have come from the recorded set.
{ echo "services:"; runner_service; } > docker-compose.nodep.yml

# Same again, but the runner exits non-zero after passing its assertions.
{ echo "services:"; runner_service RUNNER_EXIT=7; } > docker-compose.fail.yml

# Same again, plus one call the recording does not have.
{ echo "services:"; runner_service UNRECORDED=/price/tsla; } > docker-compose.miss.yml

# Same again, but the runner stays up after its calls.
{ echo "services:"; runner_service LINGER=60; } > docker-compose.linger.yml

# The runner proves the agent refused it (AGENT REFUSED ...). The agent and the
# CLI each say so too when the token did not arrive, so a run is failed on
# either of those lines as well.
assert_agent_guarded() {
  local log=$1
  grep -q "AGENT REFUSED AN UNAUTHENTICATED CALLER" "$log" || { echo "FAIL: $log: the runner never saw the agent refuse an unauthenticated call"; FAIL=1; }
  if grep -qE "running WITHOUT authentication|NOT enforcing control-plane authentication" "$log"; then
    echo "FAIL: $log: the agent came up without its control-plane token — the compose handoff is broken"; FAIL=1
  fi
}

priced_mocks() {
  local n
  n=$(sudo grep -c 'url: /price/' keploy/e2e/mocks.yaml 2>/dev/null)
  echo "${n:-0}"
}

echo "== 1. record a compose project =="
sudo -E env PATH="$PATH" "$RECORD_BIN" mock record \
  -c "docker compose up" --container-name mockc-runner --name e2e --disable-tele 2>&1 | tee rec.log
grep -q "RUNNER PASSED 3" rec.log || { echo "FAIL: the runner never completed under record"; FAIL=1; }
assert_agent_guarded rec.log
MOCKS=$(priced_mocks)
echo "recorded /price/ mocks: $MOCKS"
[ "$MOCKS" -eq 3 ] || { echo "FAIL: expected 3 recorded /price/ calls, got $MOCKS — the app was released before the proxy was armed"; FAIL=1; }
cleanup

echo "== 2. replay with the dependency service DELETED (must serve from mocks) =="
sudo -E env PATH="$PATH" "$REPLAY_BIN" mock replay \
  -c "docker compose -f docker-compose.nodep.yml up" --container-name mockc-runner --name e2e --disable-tele 2>&1 | tee rep.log
RC=${PIPESTATUS[0]}
echo "replay exit=$RC"
[ "$RC" -eq 0 ] || { echo "FAIL: replay should pass entirely from mocks (exit 0), got $RC"; FAIL=1; }
grep -q "RUNNER PASSED 3" rep.log || { echo "FAIL: the runner did not get all 3 answers from the mock set"; FAIL=1; }
assert_agent_guarded rep.log
# --abort-on-container-exit stops the agent with the app, so the agent is gone
# before keploy can ask it what it served. It leaves that account as it is
# stopped, and keploy reads it out of the stopped container: the outcome must be
# known, and this run -- every answer from the recording -- must be PROVEN.
grep -q 'mock replay summary.*"missed": 0' rep.log || { echo "FAIL: the replay's outcome was not read back from the stopped agent (consumed/missed unknown)"; FAIL=1; }
grep -q 'mock replay summary.*"consumed": [1-9]' rep.log || { echo "FAIL: the replay served every answer from the recording, and its outcome says it served none"; FAIL=1; }
grep -q "incomplete" rep.log && { echo "FAIL: the replay summary is incomplete under compose"; FAIL=1; }
sudo grep -q '^isolated: true' keploy/e2e/last-replay.yaml || { echo "FAIL: the receipt does not prove the run isolated:"; sudo cat keploy/e2e/last-replay.yaml; FAIL=1; }
cleanup

echo "== 3. --strict passes a compose replay it can now verify =="
sudo -E env PATH="$PATH" "$REPLAY_BIN" mock replay \
  -c "docker compose -f docker-compose.nodep.yml up" --container-name mockc-runner --name e2e --disable-tele --strict 2>&1 | tee strict.log
RC=${PIPESTATUS[0]}
echo "strict replay exit=$RC"
grep -q "RUNNER PASSED 3" strict.log || { echo "FAIL: the runner did not complete under --strict"; FAIL=1; }
[ "$RC" -eq 0 ] || { echo "FAIL: --strict failed a compose replay that missed nothing (exit $RC)"; FAIL=1; }
grep -q "could not be proven" strict.log && { echo "FAIL: --strict could not read the miss list under compose"; FAIL=1; }
cleanup

echo "== 3b. --strict fails on a call the recording lacks, read back from the stopped agent =="
sudo -E env PATH="$PATH" "$REPLAY_BIN" mock replay \
  -c "docker compose -f docker-compose.miss.yml up" --container-name mockc-runner --name e2e --disable-tele --strict 2>&1 | tee miss.log
RC=${PIPESTATUS[0]}
echo "strict replay with an unrecorded call exit=$RC"
grep -q "RUNNER PASSED 3" miss.log || { echo "FAIL: the runner did not complete"; FAIL=1; }
[ "$RC" -eq 1 ] || { echo "FAIL: --strict must fail a replay that missed a call (exit 1), got $RC"; FAIL=1; }
grep -q 'mock replay summary.*"missed": 1' miss.log || { echo "FAIL: the missed call was not read back from the stopped agent"; FAIL=1; }
sudo grep -q '^failedBy: strict' keploy/e2e/last-replay.yaml || { echo "FAIL: the receipt does not blame --strict:"; sudo cat keploy/e2e/last-replay.yaml; FAIL=1; }
cleanup

echo "== 4. exit-code propagation: a failing runner must fail keploy =="
sudo -E env PATH="$PATH" "$REPLAY_BIN" mock replay \
  -c "docker compose -f docker-compose.fail.yml up" --container-name mockc-runner --name e2e --disable-tele 2>&1 | tee fail.log
RC=${PIPESTATUS[0]}
echo "failing-runner replay exit=$RC"
[ "$RC" -eq 7 ] || { echo "FAIL: keploy must mirror the runner's exit code 7, got $RC"; FAIL=1; }
cleanup

echo "== 5. an agent that dies mid-run is keploy's failure, not the runner's =="
# The agent is a service in the project: when it dies, compose aborts the
# project and stops the runner, whose exit is then compose's stop (137/143),
# not its verdict. keploy must say its agent died and exit with its own 1.
( for _ in $(seq 1 240); do grep -q "RUNNER PASSED 3" agentdies.log 2>/dev/null && break; sleep 0.5; done
  agent=$(sudo docker ps --format '{{.Names}}' | grep '^keploy-v3-' | head -1)
  echo "killing the agent container ${agent:-<none found>}"
  [ -n "$agent" ] && sudo docker kill "$agent" >/dev/null ) &
KILLER=$!
sudo -E env PATH="$PATH" "$REPLAY_BIN" mock replay \
  -c "docker compose -f docker-compose.linger.yml up" --container-name mockc-runner --name e2e --disable-tele 2>&1 | tee agentdies.log
RC=${PIPESTATUS[0]}
wait "$KILLER"
echo "replay whose agent was killed exit=$RC"
grep -q "RUNNER PASSED 3" agentdies.log || { echo "FAIL: the runner never got as far as the kill"; FAIL=1; }
[ "$RC" -eq 1 ] || { echo "FAIL: keploy must exit its own 1 when its agent dies, got $RC (compose stopping the runner, mirrored as the runner's own exit)"; FAIL=1; }
grep -q "the keploy agent stopped while the test command was running" agentdies.log || { echo "FAIL: keploy did not say its agent died"; FAIL=1; }
sudo grep -q '^failedBy: keploy' keploy/e2e/last-replay.yaml || { echo "FAIL: the receipt does not blame keploy:"; sudo cat keploy/e2e/last-replay.yaml; FAIL=1; }
sudo grep -q '^runnerExitCode: -1' keploy/e2e/last-replay.yaml || { echo "FAIL: the receipt gives the runner an exit of its own:"; sudo cat keploy/e2e/last-replay.yaml; FAIL=1; }
# Killed, the agent left no account of what it served: what it served and
# missed is unknown, and must read so. Read as 0 and 0, it proves a run nobody
# saw -- --strict passes it, and it is metered as zero.
grep -q 'mock replay summary.*"missed": "unknown"' agentdies.log || { echo "FAIL: the replay summary gives a killed agent's outcome as known"; FAIL=1; }
sudo grep -q '^missed: -1' keploy/e2e/last-replay.yaml || { echo "FAIL: the receipt gives a killed agent's misses as known:"; sudo cat keploy/e2e/last-replay.yaml; FAIL=1; }

if [ "$FAIL" -eq 0 ]; then echo "MOCK COMPOSE E2E: PASSED"; else echo "MOCK COMPOSE E2E: FAILED"; fi
exit "$FAIL"
