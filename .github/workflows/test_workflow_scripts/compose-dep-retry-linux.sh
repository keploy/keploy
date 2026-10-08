#!/usr/bin/env bash
# E2E: a docker compose dependency that crashes once, after keploy has set up
# its agent, must not cost the run.
#
# Compose aborts `up` when a service the app depends_on exits before it is
# healthy, and keploy retries the bring-up. That retry used to bring the whole
# stack down first, keploy's agent with it, and the agent compose created again
# had none of the setup the run had already given the first one: its record
# streams, its mocks and their filters, its readiness. Its healthcheck never
# passed, the app behind it never started, and the run recorded nothing (or
# failed every test) and exited 0. Now the retry keeps a running agent, and
# keploy sets up again an agent compose starts again (it stops every container
# when the crash is an attached container's exit) or recreates (a recreate
# flag). The app here calls the dependency on every request, so the outgoing
# mocks are recorded and replayed through all of it; both are Python's
# http.server, which answers in HTTP/1.0:
#
#   1. record, `up app` (only the app attached, as the atg lane runs it): the
#      crash leaves the agent running, and the retry reuses it;
#   2. record, `up` (every service attached): compose either keeps the agent
#      as in 1 or stops every container, and keploy sets the agent compose
#      starts again up as before;
#   3. record, `up --always-recreate-deps app`: the retry recreates the agent,
#      always, so it must be set up again;
#   4. test (replay) of 3's recording, `up --always-recreate-deps app`, with
#      the dependency crashing once again: the recreated agent must get the
#      test set's mocks, their filters and its readiness again.
#
# Env: RECORD_BIN and REPLAY_BIN (the keploy binaries under test). The agent
# image must be loaded as ghcr.io/keploy/keploy:v3-dev
# (.github/actions/download-image).
set -uo pipefail

RECORD_BIN="$(command -v "${RECORD_BIN:-keploy}" || echo "${RECORD_BIN:-keploy}")"
REPLAY_BIN="$(command -v "${REPLAY_BIN:-$RECORD_BIN}" || echo "${REPLAY_BIN:-$RECORD_BIN}")"
WORK="$(mktemp -d)"
cd "$WORK" || exit 1
FAIL=0
fail() { echo "FAIL: $*"; FAIL=$((FAIL + 1)); }

cleanup() { sudo docker compose down -v --remove-orphans >/dev/null 2>&1; }
trap 'cleanup; cd /; sudo rm -rf "$WORK"' EXIT

docker image inspect python:3.11-slim >/dev/null 2>&1 || docker pull -q python:3.11-slim >/dev/null

# dep crashes 10s into its first start (after keploy's agent is set up), and
# serves data.json on every later one; the marker lives in a volume, so the
# restart sees it. The app answers each request with dep's data.json.
cat > docker-compose.yml <<'EOF'
services:
  dep:
    image: python:3.11-slim
    command:
      - sh
      - -c
      - |
        if [ -f /data/marker ]; then
          mkdir -p /srv && printf '{"answer": 42}' > /srv/data.json
          exec python -m http.server 8000 --directory /srv
        fi
        touch /data/marker; sleep 10; exit 3
    volumes: ["flaky:/data"]
    healthcheck:
      test: ["CMD", "python", "-c", "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8000/data.json', timeout=2)"]
      interval: 1s
      timeout: 3s
      retries: 20
      start_period: 1s
  app:
    image: python:3.11-slim
    container_name: retryapp
    command:
      - python
      - -c
      - |
        import http.server, urllib.request
        class H(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                body = urllib.request.urlopen("http://dep:8000/data.json", timeout=5).read()
                self.send_response(200)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
            def log_message(self, *a):
                pass
        http.server.ThreadingHTTPServer(("0.0.0.0", 8080), H).serve_forever()
    ports: ["8080:8080"]
    depends_on:
      dep: {condition: service_healthy}
volumes:
  flaky: {}
EOF

# requests: three requests once the app answers, while keploy runs as $1.
requests() {
  local pid=$1 i
  for i in $(seq 1 120); do
    if curl -s -m 2 -o /dev/null http://localhost:8080/ 2>/dev/null; then break; fi
    kill -0 "$pid" 2>/dev/null || return
    sleep 1
  done
  for i in 1 2 3; do curl -s -m 5 -o /dev/null "http://localhost:8080/?r=$i" 2>/dev/null; done
}

# retried CASE: says how keploy's agent fared in the retry of the dependency
# crash; fails when the crash was not retried. It runs in $(...), so the
# caller records the failure.
retried() {
  local name=$1
  grep -q 'failed to start transiently' "$name.log" || return 1
  if setup_again "$name"; then
    echo "compose started keploy's agent again, and keploy set it up again"
  else
    echo "compose kept keploy's agent running, and the retry reused it"
  fi
}

# setup_again CASE: keploy set up the agent compose started again.
setup_again() { grep -q 'started again by docker compose; set it up again' "$1.log"; }

# record CASE COMMAND: record three requests through a dependency crash.
record() {
  local name=$1 cmd=$2 tests mocks how rc before=$FAIL
  cleanup
  sudo rm -rf keploy
  # shellcheck disable=SC2024 # the log is this user's, so the shell redirects
  sudo -E env PATH="$PATH" timeout -k 10 180 "$RECORD_BIN" record -c "$cmd" --container-name retryapp \
    --record-timer 60s --disable-tele > "$name.log" 2>&1 &
  local pid=$!
  requests "$pid"
  wait "$pid"
  rc=$?
  [ "$rc" -eq 0 ] || fail "$name: keploy record exited $rc"
  if ! how=$(retried "$name"); then
    fail "$name: the dependency crash was not retried"
    tail -60 "$name.log"
    return
  fi
  tests=$( (sudo find keploy -path '*/tests/*.yaml' 2>/dev/null || true) | wc -l)
  mocks=$( (sudo cat keploy/test-set-0/mocks.yaml 2>/dev/null || true) | grep -c '^kind: Http')
  [ "$tests" -ge 3 ] || fail "$name: $tests test case(s) recorded after the retry, want at least 3"
  [ "$mocks" -ge 3 ] || fail "$name: $mocks HTTP mock(s) of the app's calls to its dependency, want at least 3"
  if [ "$FAIL" -ne "$before" ]; then
    tail -60 "$name.log"
  else
    echo "ok: $name: $how; $tests test cases and $mocks mocks recorded"
  fi
}

record app-attached "docker compose up app"
record all-attached "docker compose up"
record recreated "docker compose up --always-recreate-deps app"
setup_again recreated || fail "recreated: the agent compose recreated was not set up again"

# 4. Replay 3's recording, the dependency crashing once again (a new volume)
# and the agent recreated by the retry.
cleanup
before=$FAIL
# shellcheck disable=SC2024 # the log is this user's, so the shell redirects
sudo -E env PATH="$PATH" timeout -k 10 300 "$REPLAY_BIN" test -c "docker compose up --always-recreate-deps app" \
  --container-name retryapp --delay 10 --disable-tele > replay.log 2>&1
rc=$?
[ "$rc" -eq 0 ] || fail "replay: keploy test exited $rc"
how=$(retried replay) || fail "replay: the dependency crash was not retried"
setup_again replay || fail "replay: the agent compose recreated was not set up again"
summary=$(sed -E 's/\x1b\[[0-9;]*m//g' replay.log | { grep -E 'Total test (passed|failed):' || true; } | tr -s ' \t' ' ' | tr '\n' ' ')
if ! grep -q 'Total test failed: 0' <<<"$summary" || grep -q 'Total test passed: 0' <<<"$summary"; then
  fail "replay: want every test passed on the recreated agent: ${summary:-no summary}"
fi
if [ "$FAIL" -ne "$before" ]; then
  tail -80 replay.log
else
  echo "ok: replay: $how; $summary"
fi

if [ "$FAIL" -ne 0 ]; then
  echo "compose-dep-retry e2e: FAILED"
  exit 1
fi
echo "compose-dep-retry e2e: all cases passed"
