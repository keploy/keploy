#!/usr/bin/env bash
# e2e for keploy's agent whose host port is taken between keploy's draw and
# `docker compose up`. Setup draws a free port P for the agent and writes it
# into the compose file it runs as the agent's publish, 127.0.0.1:P; compose
# publishes it later. A socket that takes P in between makes the engine
# refuse the publish, and the up fails before the app starts. keploy now
# moves the agent to another port and runs the up again, at most twice,
# saying so; before, the run ended there.
#
# The collision is made, not waited for: keploy runs compose through a
# `docker` on PATH ahead of the real one. On a `compose ... up`, it reads
# the agent's publish in the compose file the up names, binds 127.0.0.1:P on
# the host with a listener of its own, and only then runs the real docker.
# The legs:
#   1. P taken at the first up only: keploy moves the agent and records two
#      calls; `keploy test`, its first up's port taken too, replays them.
#   2. P taken at every up: keploy moves the agent twice, then ends the run,
#      saying it does not move it again.
#
# Env: RECORD_BIN (the keploy binary under test). The agent image the binary
# names must be loaded (.github/actions/download-image).
set -uo pipefail

# shellcheck source=sudo_env.sh
source "$(dirname "${BASH_SOURCE[0]}")/sudo_env.sh" || exit 1
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true

RECORD_BIN="${RECORD_BIN:-keploy}"
IMAGE=python:3.11-slim
WORK="$(mktemp -d)"
cd "$WORK" || exit 1
FAIL=0
ID=$$
APP="apt-app-$ID"
source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/free-port.sh"

REAL_DOCKER=$(command -v docker) || { echo "FAIL: no docker on PATH"; exit 1; }
mkdir -p "$WORK/shim"
# The shim's record: one line per compose up, "up PORT taken|free", and the
# pid of each listener it started, for cleanup. Root's (keploy runs it under
# sudo), so they live in a directory anyone may write.
mkdir -p "$WORK/ups" && chmod 777 "$WORK/ups"
cat > "$WORK/shim/docker" <<SH
#!/usr/bin/env bash
# A compose up takes the agent's host port first, as TAKE (in $WORK/ups/take)
# says: "first" at the first up only, "every" at each up.
up=0 file=docker-compose.yml next=
for a in "\$@"; do
  [ -n "\$next" ] && { file=\$a; next=; continue; }
  case \$a in -f|--file) next=1 ;; up) up=1 ;; esac
done
if [ "\$up" = 1 ] && [ -f "$WORK/ups/take" ]; then
  port=\$(grep -oE '127\.0\.0\.1:[0-9]+:[0-9]+' "\$file" | head -n1 | cut -d: -f2)
  n=\$(cat "$WORK/ups/log" 2>/dev/null | wc -l)
  if [ -n "\$port" ] && { [ "\$(cat "$WORK/ups/take")" = every ] || [ "\$n" = 0 ]; }; then
    rm -f "$WORK/ups/bound"
    python3 -c '
import os, socket, sys, time
s = socket.socket()
s.bind(("127.0.0.1", int(sys.argv[1])))
s.listen(1)
open(sys.argv[2], "w").close()
time.sleep(600)
' "\$port" "$WORK/ups/bound" &
    echo \$! >> "$WORK/ups/pids"
    for _ in \$(seq 50); do [ -f "$WORK/ups/bound" ] && break; sleep 0.1; done
    if [ -f "$WORK/ups/bound" ]; then echo "up \$port taken" >> "$WORK/ups/log"; else echo "up \$port not-taken" >> "$WORK/ups/log"; fi
  else
    echo "up \${port:-none} free" >> "$WORK/ups/log"
  fi
fi
exec "$REAL_DOCKER" "\$@"
SH
chmod +x "$WORK/shim/docker"

# Tear down only this run's project, container and listeners, by name and
# pid. Never a blanket prune.
cleanup() {
  [ -f docker-compose.yml ] && sudo "$REAL_DOCKER" compose down --remove-orphans >/dev/null 2>&1
  sudo "$REAL_DOCKER" rm -f "$APP" >/dev/null 2>&1
  [ -f ups/pids ] && xargs -r sudo kill < ups/pids 2>/dev/null
  rm -f ups/pids ups/log ups/take ups/bound
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
import http.server, json
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
cat > keploy.yml <<'YML'
test:
    globalNoise:
        global: {"header": {"Date":[]}}
YML
HP=$(free_port) || exit 1
cat > docker-compose.yml <<YML
services:
  app:
    image: $IMAGE
    container_name: $APP
    working_dir: /w
    volumes: [".:/w:z"] # :z relabels on SELinux hosts; ignored elsewhere
    command: python app.py
    ports: ["$HP:8080"]
YML

# await LOG PID PATTERN N: 0 once LOG has N lines that carry PATTERN, 1 when
# keploy (PID) exited first.
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
        try:
            with open("/proc/%d/stat" % pid) as st:
                if st.read().rsplit(")", 1)[1].split()[0] == "Z":
                    sys.exit(1)
        except FileNotFoundError:
            sys.exit(1)
        time.sleep(0.2)
PY
}

# ups_are NAME WANT: the shim's lines, "taken" or "free" per up in order, are
# WANT (e.g. "taken free"), and each up after a taken one is on another port.
ups_are() {
  local got ports
  got=$(awk '{print $3}' ups/log 2>/dev/null | xargs)
  ports=$(awk '{print $2}' ups/log 2>/dev/null | xargs)
  echo "$1: ups: $(xargs < ups/log 2>/dev/null)"
  [ "$got" = "$2" ] || { echo "FAIL: $1: the ups were [$got], want [$2]"; FAIL=1; return; }
  [ "$(tr ' ' '\n' <<<"$ports" | sort -u | wc -l)" = "$(wc -w <<<"$ports")" ] ||
    { echo "FAIL: $1: an up ran on a port an earlier up was refused: $ports"; FAIL=1; }
}

MOVED="moving the agent to another port, before the app starts"
AGAIN="running docker compose up again, with keploy's agent on another port"

echo "== 1. the agent's port taken at the first up"
echo first > ups/take
sudo rm -rf keploy
: > record.log
sudo env PATH="$WORK/shim:$PATH" "${SUDO_ENV[@]}" "$RECORD_BIN" record -c "docker compose up" --container-name "$APP" \
  --disable-tele --disable-ansi > record.log 2>&1 &
pid=$!
made=0
if await record.log "$pid" "APP LISTENING" 1; then
  for p in /hello/one /hello/two; do
    if curl -fsS -o /dev/null --max-time 10 "http://127.0.0.1:$HP$p"; then
      made=$((made + 1))
    else
      echo "FAIL: record: GET $p through host port $HP failed"; FAIL=1
    fi
  done
  [ "$made" -eq 0 ] || await record.log "$pid" "Keploy has captured test cases" "$made" ||
    { echo "FAIL: record: keploy did not write the test cases of the $made calls made"; FAIL=1; }
  sudo kill -INT "$pid" 2>/dev/null
else
  echo "FAIL: record: keploy exited before the app listened"; FAIL=1
fi
wait "$pid"
rc=$?
[ "$rc" -eq 0 ] || { echo "FAIL: record: keploy record exited $rc"; FAIL=1; }
[ "$made" -eq 2 ] || { echo "FAIL: record: $made of 2 calls recorded"; FAIL=1; }
grep -qF "$MOVED" record.log || { echo "FAIL: record: keploy did not say it moved the agent"; FAIL=1; }
grep -qF "$AGAIN" record.log || { echo "FAIL: record: keploy did not say it ran the up again"; FAIL=1; }
ups_are record "taken free"
[ "$FAIL" = 0 ] || tail -60 record.log
cleanup

echo first > ups/take
sudo rm -rf keploy/reports
sudo env PATH="$WORK/shim:$PATH" "${SUDO_ENV[@]}" "$RECORD_BIN" test -c "docker compose up" --container-name "$APP" \
  --delay 5 --disable-tele --disable-ansi > test.log 2>&1
rc=$?
verify_ran "${KEPLOY_PROVEN:-}" test.log || FAIL=1
status=$(sudo sh -c 'cat keploy/reports/test-run-*/test-set-0-report.yaml 2>/dev/null' | sed -n 's/^status: //p' | tail -n 1)
echo "test: keploy test exited $rc, report ${status:-missing}"
{ [ "$rc" -eq 0 ] && [ "$status" = PASSED ]; } || { echo "FAIL: test: want exit 0 and PASSED"; FAIL=1; tail -60 test.log; }
grep -qF "$MOVED" test.log || { echo "FAIL: test: keploy did not say it moved the agent"; FAIL=1; }
ups_are test "taken free"
cleanup

echo "== 2. the agent's port taken at every up"
echo every > ups/take
sudo rm -rf keploy
sudo env PATH="$WORK/shim:$PATH" "${SUDO_ENV[@]}" "$RECORD_BIN" record -c "docker compose up" --container-name "$APP" \
  --disable-tele --disable-ansi > bound.log 2>&1 &
pid=$!
# keploy ends the run itself; the bound is the step's, not this wait's.
wait "$pid"
rc=$?
echo "bound: keploy record exited $rc"
[ "$rc" -ne 0 ] || { echo "FAIL: bound: keploy record exited 0 with its agent's port taken at every up"; FAIL=1; }
grep -qF "docker compose up failed again after keploy moved its agent to another port; not moving it again" bound.log ||
  { echo "FAIL: bound: keploy did not say it stops moving the agent"; FAIL=1; tail -60 bound.log; }
[ "$(grep -cF "$MOVED" bound.log)" = 2 ] || { echo "FAIL: bound: want 2 moves said, got $(grep -cF "$MOVED" bound.log)"; FAIL=1; }
ups_are bound "taken taken taken"
grep -q "APP LISTENING" bound.log && { echo "FAIL: bound: the app started"; FAIL=1; }
cleanup

[ "$FAIL" = 0 ] && echo "PASS: compose agent publish taken" || echo "FAIL: compose agent publish taken"
exit "$FAIL"
