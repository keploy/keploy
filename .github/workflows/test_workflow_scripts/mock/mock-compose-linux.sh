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
  for f in docker-compose.yml docker-compose.nodep.yml docker-compose.fail.yml; do
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
print("RUNNER PASSED 3", flush=True)
sys.exit(EXIT)
PY

runner_service() {
  cat <<YML
  runner:
    image: python:3.11-slim
    container_name: mockc-runner
    working_dir: /w
    volumes: [".:/w"]
    command: python runner.py
YML
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
{ echo "services:"; runner_service; cat <<'YML'
    environment:
      RUNNER_EXIT: "7"
YML
} > docker-compose.fail.yml

priced_mocks() {
  local n
  n=$(sudo grep -c 'url: /price/' keploy/e2e/mocks.yaml 2>/dev/null)
  echo "${n:-0}"
}

echo "== 1. record a compose project =="
sudo -E env PATH="$PATH" "$RECORD_BIN" mock record \
  -c "docker compose up" --container-name mockc-runner --name e2e --disable-tele 2>&1 | tee rec.log
grep -q "RUNNER PASSED 3" rec.log || { echo "FAIL: the runner never completed under record"; FAIL=1; }
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
grep -q "mock replay summary" rep.log || { echo "FAIL: replay reported no outcome at all"; FAIL=1; }
cleanup

echo "== 3. --strict must not pass a run it could not verify =="
# --abort-on-container-exit stops the agent with the app, so today the miss list
# is read from an agent that is already gone and --strict fails closed on every
# compose run. Assert the contract rather than today's branch of it: --strict
# fails exactly when the miss count came back unknown, and must not otherwise.
# Fixing the teardown so the outcome IS readable should make this lane greener.
sudo -E env PATH="$PATH" "$REPLAY_BIN" mock replay \
  -c "docker compose -f docker-compose.nodep.yml up" --container-name mockc-runner --name e2e --disable-tele --strict 2>&1 | tee strict.log
RC=${PIPESTATUS[0]}
echo "strict replay exit=$RC"
grep -q "RUNNER PASSED 3" strict.log || { echo "FAIL: the runner did not complete under --strict"; FAIL=1; }
if grep -q '"missed": "unknown"' strict.log; then
  echo "note: the miss list was unreadable (the agent is stopped with the app under compose)"
  [ "$RC" -ne 0 ] || { echo "FAIL: --strict passed a run whose misses were never read"; FAIL=1; }
  grep -q "could not be proven" strict.log || { echo "FAIL: --strict failed without saying why"; FAIL=1; }
else
  echo "note: the miss list was readable; --strict applied to it"
  [ "$RC" -eq 0 ] || { echo "FAIL: --strict failed a replay that missed nothing (exit $RC)"; FAIL=1; }
fi
cleanup

echo "== 4. exit-code propagation: a failing runner must fail keploy =="
sudo -E env PATH="$PATH" "$REPLAY_BIN" mock replay \
  -c "docker compose -f docker-compose.fail.yml up" --container-name mockc-runner --name e2e --disable-tele 2>&1 | tee fail.log
RC=${PIPESTATUS[0]}
echo "failing-runner replay exit=$RC"
[ "$RC" -eq 7 ] || { echo "FAIL: keploy must mirror the runner's exit code 7, got $RC"; FAIL=1; }

if [ "$FAIL" -eq 0 ]; then echo "MOCK COMPOSE E2E: PASSED"; else echo "MOCK COMPOSE E2E: FAILED"; fi
exit "$FAIL"
