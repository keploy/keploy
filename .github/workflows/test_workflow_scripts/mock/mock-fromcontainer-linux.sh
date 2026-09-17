#!/usr/bin/env bash
# Self-contained e2e for `keploy mock record --from-container`, which records
# against a container the user is already running: keploy stops it, runs a copy
# of it under the agent's namespaces, and has to put the original back.
#
# Putting it back is the part that needs a real daemon. Stopping the container
# while the agent holds the same network under the app's own alias makes docker
# drop the container's network ENDPOINT, and published ports live on the
# endpoint - so it comes back attached to nothing and bound to nothing, its DNS
# names stop resolving, and neither `start` nor `restart` repairs it. The unit
# tests around this necessarily model that behaviour rather than proving it,
# which is why this lane exists.
#
# Env: RECORD_BIN (the keploy binary under test), set by the calling workflow
# from .github/actions/download-binary's `path` output. The agent image must be
# loaded as ghcr.io/keploy/keploy:v3-dev - .github/actions/download-image does
# that; a source build reports version 3-dev and that tag is not published, so
# without it there is no agent to start.
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true

RECORD_BIN="${RECORD_BIN:-keploy}"
APP=fc-app
DEP=fc-dep
PROJECT=fromcontainer
WORK="$(mktemp -d)"
cd "$WORK" || exit 1
FAIL=0

fail() {
  echo "FAIL: $*"
  FAIL=1
}

# Tear down only this project, by name. Never a blanket prune.
cleanup() {
  sudo docker compose -p "$PROJECT" down --remove-orphans --volumes >/dev/null 2>&1
  for stray in $(sudo docker ps -aq --filter "name=keploy-restore-backup" 2>/dev/null) \
    $(sudo docker ps -aq --filter "name=-keploy\$" 2>/dev/null); do
    sudo docker rm -f "$stray" >/dev/null 2>&1
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
http.server.HTTPServer(("0.0.0.0", 9421), H).serve_forever()
PY

# The app resolves its dependency BY NAME on every request, so a request only
# succeeds while the container is on its network with working DNS - which is
# exactly what the endpoint carries.
cat > app.py <<'PY'
import json, os, urllib.request, http.server
DEP = os.environ.get("DEP_URL", "http://fc-dep:9421")
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        try:
            with urllib.request.urlopen(DEP + self.path, timeout=5) as r:
                dep = json.load(r)
            body, code = json.dumps({"dep": dep}).encode(), 200
        except Exception as e:
            body, code = json.dumps({"error": type(e).__name__}).encode(), 502
        self.send_response(code); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def log_message(self, *a): pass
http.server.HTTPServer(("0.0.0.0", 9420), H).serve_forever()
PY

# A named volume and an anonymous one: the anonymous volume's name appears ONLY
# in the container's Mounts, so a rebuild that replays Config.Volumes instead
# hands the app a brand new empty volume and orphans its data.
cat > docker-compose.yml <<'YML'
services:
  fc-dep:
    image: python:3.11-slim
    container_name: fc-dep
    command: python /srv/dep.py
    volumes:
      - ./dep.py:/srv/dep.py:ro
    networks: [fcnet]
  fc-app:
    image: python:3.11-slim
    container_name: fc-app
    command: python /srv/app.py
    ports:
      - "127.0.0.1:9420:9420"
    volumes:
      - ./app.py:/srv/app.py:ro
      - fcdata:/data
      - /anon
    networks: [fcnet]
    depends_on: [fc-dep]
volumes:
  fcdata:
networks:
  fcnet:
YML

echo "--- bringing the fixture up ---"
sudo docker compose -p "$PROJECT" up -d >/dev/null 2>&1
for _ in $(seq 1 30); do
  [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 4 http://127.0.0.1:9420/alpha)" = "200" ] && break
  sleep 2
done
if [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 4 http://127.0.0.1:9420/alpha)" != "200" ]; then
  echo "FAIL: fixture never came up; nothing to record against"
  sudo docker compose -p "$PROJECT" logs
  exit 1
fi

# Markers in both volumes, to prove a rebuild remounts the SAME storage.
sudo docker exec "$APP" sh -c 'echo named > /data/marker; echo anon > /anon/marker' || {
  echo "FAIL: could not write the volume markers, so nothing below would mean anything"
  exit 1
}
ANON_VOL_BEFORE="$(sudo docker inspect -f '{{range .Mounts}}{{if eq .Destination "/anon"}}{{.Name}}{{end}}{{end}}' "$APP")"
# Asserted non-empty, or the before/after comparison below passes on two blanks.
[ -n "$ANON_VOL_BEFORE" ] || {
  echo "FAIL: the fixture has no anonymous volume, so the swap it guards cannot be detected"
  exit 1
}

echo "--- recording ---"
"$RECORD_BIN" mock record --from-container "$APP" --name fc --path . --local --disable-ansi > record.log 2>&1 &
REC_PID=$!
# keploy re-execs itself under sudo for --from-container, so the process this
# waits on and signals is root-owned: an unprivileged kill is refused, not
# delivered, and the session would never be told to stop.
for _ in $(seq 1 150); do
  sudo grep -aq "your app is running under keploy" record.log && break
  sudo kill -0 "$REC_PID" 2>/dev/null || break
  sleep 1
done

# The agent publishes the app's ports on its behalf, because the app shares the
# agent's network namespace and cannot publish anything itself.
for _ in $(seq 1 20); do
  [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://127.0.0.1:9420/alpha)" = "200" ] && break
  sleep 2
done
CODE_DURING="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://127.0.0.1:9420/alpha)"
[ "$CODE_DURING" = "200" ] || fail "the app did not answer through the agent during recording (got ${CODE_DURING:-none})"

echo "--- stopping the session, then asserting WITHOUT any reset ---"
sudo kill -INT "$REC_PID" 2>/dev/null
for _ in $(seq 1 150); do sudo kill -0 "$REC_PID" 2>/dev/null || break; sleep 1; done
# Restoring is a teardown step, so give it a moment after the process is gone.
sleep 10

RUNNING="$(sudo docker inspect -f '{{.State.Running}}' "$APP" 2>/dev/null)"
[ "$RUNNING" = "true" ] || fail "the user's container is not running after the session (Running=${RUNNING:-missing})"

# The endpoint id is the real attachment signal: a dropped endpoint is still
# LISTED, with nothing in it.
ENDPOINT="$(sudo docker inspect -f '{{range .NetworkSettings.Networks}}{{.EndpointID}}{{end}}' "$APP" 2>/dev/null)"
[ -n "$ENDPOINT" ] && [ "$ENDPOINT" != "<no value>" ] || fail "the container came back attached to no network endpoint"

PUBLISHED="$(sudo docker inspect -f '{{index .NetworkSettings.Ports "9420/tcp"}}' "$APP" 2>/dev/null)"
case "$PUBLISHED" in
  *127.0.0.1*9420*) ;;
  *) fail "9420 is not published after the session (got '${PUBLISHED}')" ;;
esac

CODE_AFTER="$(curl -s -o /dev/null -w '%{http_code}' --max-time 8 http://127.0.0.1:9420/alpha)"
[ "$CODE_AFTER" = "200" ] || fail "the app does not answer on its published port after the session (got ${CODE_AFTER:-none}); a 502 means its DNS did not come back"

# The volumes have to be the same storage, not new empties.
NAMED_MARKER="$(sudo docker exec "$APP" cat /data/marker 2>/dev/null | tr -d '\r\n')"
[ "$NAMED_MARKER" = "named" ] || fail "the named volume did not come back (marker='${NAMED_MARKER}')"
ANON_MARKER="$(sudo docker exec "$APP" cat /anon/marker 2>/dev/null | tr -d '\r\n')"
[ "$ANON_MARKER" = "anon" ] || fail "the anonymous volume was replaced with an empty one (marker='${ANON_MARKER}'); its name lives only in the container's Mounts"
ANON_VOL_AFTER="$(sudo docker inspect -f '{{range .Mounts}}{{if eq .Destination "/anon"}}{{.Name}}{{end}}{{end}}' "$APP")"
[ "$ANON_VOL_BEFORE" = "$ANON_VOL_AFTER" ] || fail "the anonymous volume was swapped ($ANON_VOL_BEFORE -> $ANON_VOL_AFTER)"

# Compose has to keep recognising it as its own service, or the next `up`
# recreates the wrong container.
LABELS="$(sudo docker inspect -f '{{index .Config.Labels "com.docker.compose.project"}}/{{index .Config.Labels "com.docker.compose.service"}}' "$APP" 2>/dev/null)"
[ "$LABELS" = "$PROJECT/$APP" ] || fail "compose no longer owns the container (labels='${LABELS}')"

# Nothing of keploy's may outlive the session under a name the user did not pick.
LEFTOVERS="$(sudo docker ps -aq --filter 'name=keploy-restore-backup' | wc -l | tr -d ' ')"
[ "$LEFTOVERS" = "0" ] || fail "$LEFTOVERS container(s) left behind under keploy's temporary restore name"
REPLICAS="$(sudo docker ps -aq --filter "name=^${APP}-keploy\$" | wc -l | tr -d ' ')"
[ "$REPLICAS" = "0" ] || fail "the replica keploy ran the app as was left behind"

# And the recording itself has to have captured something, or the lane would
# pass on a session that did nothing.
DOCS="$(sudo grep -hc '^kind:' keploy/fc/mocks.yaml 2>/dev/null)"
DOCS="${DOCS:-0}"
[ "$DOCS" -gt 0 ] || fail "recorded no mocks, so this proves nothing about a real session"

# What keploy actually did, on success as well as failure. Whether the endpoint
# was dropped at all is a property of the daemon and differs between platforms,
# so a lane that only prints on failure cannot say which half it exercised: the
# contract (the container came back intact) or the repair that restores it.
echo "--- how the container came back ---"
sudo sed 's/\x1b\[[0-9;]*m//g' record.log |
  grep -aE "your container|waiting to start|not publishing|not on |stopping your container so keploy" |
  sed 's/^.*\(INFO\|WARN\|ERROR\)[[:space:]]*/\1 /' | tail -6
if sudo grep -aq "rebuilding it as it was" record.log; then
  echo "  (the rebuild ran: this platform dropped the endpoint)"
else
  echo "  (no rebuild needed: this platform kept the endpoint, so only the restore contract was exercised)"
fi

if [ "$FAIL" -ne 0 ]; then
  echo "--- keploy log ---"
  sudo sed 's/\x1b\[[0-9;]*m//g' record.log | tail -60
  echo "--- app container ---"
  sudo docker inspect "$APP" --format '{{json .NetworkSettings}}' 2>/dev/null | head -c 2000
  echo
  sudo docker ps -a
  exit 1
fi

echo "PASS: the container came back on its network, publishing its ports, with its data and its compose identity ($DOCS mocks recorded)"
