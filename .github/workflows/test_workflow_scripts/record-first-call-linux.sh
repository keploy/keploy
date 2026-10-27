#!/usr/bin/env bash
# e2e: a call that reaches the app the moment it listens, during keploy
# record, is served and recorded.
#
# While recording, keploy moves the app's bind of its port P to another port
# inside the app's bind(2), and its ingress forwarder takes P over once it
# learns of the move. Without a hold, a call in between (after the app says it
# listens, before keploy logs "Started ingress forwarding") was refused
# natively and reset through a Docker-published port: replicated 7 of 10
# first calls native, 4 of 10 compose. keploy now holds P from inside the
# app's bind (BPF sk_lookup, keploy/ebpf ingress_lookup.c), so the call waits
# on keploy's park listener and is served and recorded like any other.
#
# Each run: keploy record starts the app; the client GETs /first the moment
# the app prints "APP LISTENING" (read from keploy's log, polled every 5 ms,
# which is how a user's script reacts to it); the call must answer 200, and
# keploy must write its test case. RUNS runs each, natively and under
# compose. Every wait is on an event: a line in the log, or keploy's exit.
#
# A third mode, v4mapped, runs natively an app whose IPv6 socket is bound to
# ::ffff:127.0.0.1 (how Java binds 127.0.0.1 on a dual-stack host), with
# another listener X already on [::1]:P. The two binds do not conflict, so as
# without keploy the app's IPv4 call must be answered and recorded, X must
# keep its ::1 call, and keploy must not yield the port to X.
#
# Env: RECORD_BIN (the keploy binary under test; its agent image loaded for
# the compose runs), RUNS (default 5), MODES (default "native compose v4mapped").
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true

RECORD_BIN="${RECORD_BIN:-keploy}"
RUNS="${RUNS:-5}"
MODES="${MODES:-native compose v4mapped}"
IMAGE=python:3.11-slim
WORK="$(mktemp -d)"
cd "$WORK" || exit 1
FAIL=0
ID=$$
APP="first-call-app-$ID"
free_port() { python3 -c 'import socket; s = socket.socket(); s.bind(("", 0)); print(s.getsockname()[1])'; }

cleanup() {
  [ -f docker-compose.yml ] && sudo docker compose down --remove-orphans >/dev/null 2>&1
  sudo docker rm -f "$APP" >/dev/null 2>&1
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
import http.server, json, os, socket
host = os.environ.get("APP_HOST", "0.0.0.0")
class S(http.server.HTTPServer):
    address_family = socket.AF_INET6 if ":" in host else socket.AF_INET
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        b = json.dumps({"path": self.path}).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    def log_message(self, *a): pass
s = S((host, int(os.environ.get("APP_PORT", "8080"))), H)
print("APP LISTENING", flush=True)
s.serve_forever()
PY
cat > keploy.yml <<'YML'
test:
    globalNoise:
        global: {"header": {"Date":[]}}
YML

# first.py LOG PID URL: once LOG has "APP LISTENING", GET URL at once; prints
# "ok" or the error. Exits 1 if keploy (PID) exited first.
cat > first.py <<'PY'
import os, sys, time, urllib.request
log, pid, url = sys.argv[1], int(sys.argv[2]), sys.argv[3]
while True:
    with open(log, "rb") as f:
        if b"APP LISTENING" in f.read():
            break
    try:
        with open("/proc/%d/stat" % pid) as st:
            if st.read().rsplit(")", 1)[1].split()[0] == "Z":
                print("keploy exited before the app listened"); sys.exit(1)
    except FileNotFoundError:
        print("keploy exited before the app listened"); sys.exit(1)
    time.sleep(0.005)
try:
    with urllib.request.urlopen(url, timeout=30) as r:
        print("ok" if r.status == 200 else "status %d" % r.status)
except Exception as e:
    print("error: %s" % (getattr(e, "reason", None) or e))
PY

# await LOG PID PATTERN: 0 once LOG has a line with PATTERN, 1 if keploy exited first.
await() {
  python3 - "$@" <<'PY'
import sys, time
log, pid, pattern = sys.argv[1], int(sys.argv[2]), sys.argv[3].encode()
while True:
    with open(log, "rb") as f:
        if pattern in f.read():
            sys.exit(0)
    try:
        with open("/proc/%d/stat" % pid) as st:
            if st.read().rsplit(")", 1)[1].split()[0] == "Z":
                sys.exit(1)
    except FileNotFoundError:
        sys.exit(1)
    time.sleep(0.2)
PY
}

# x.py PORT: the other listener, on [::1]:PORT over IPv6 only; answers "X".
cat > x.py <<'PY'
import http.server, socket, sys
class S(http.server.HTTPServer):
    address_family = socket.AF_INET6
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200); self.send_header("Content-Length", "1"); self.end_headers(); self.wfile.write(b"X")
    def log_message(self, *a): pass
s = S(("::1", int(sys.argv[1])), H)
print("X LISTENING", flush=True)
s.serve_forever()
PY

# run MODE N: one record run; the first call must answer, and be recorded.
run() {
  local mode=$1 n=$2 log="$1-$2.log" pid url got port xpid=
  sudo rm -rf keploy
  : > "$log"
  port=$(free_port)
  if [ "$mode" = v4mapped ]; then
    python3 -u x.py "$port" > "x-$n.log" 2>&1 &
    xpid=$!
    if ! await "x-$n.log" "$xpid" "X LISTENING"; then
      echo "FAIL: $mode run $n: the other listener could not listen on [::1]:$port"; cat "x-$n.log"; FAIL=1
      return
    fi
  fi
  if [ "$mode" = compose ]; then
    cat > docker-compose.yml <<YML
services:
  app:
    image: $IMAGE
    container_name: $APP
    working_dir: /w
    volumes: [".:/w:z"]
    command: python -u app.py
    ports: ["$port:8080"]
YML
    sudo -E env PATH="$PATH" "$RECORD_BIN" record -c "docker compose up" --container-name "$APP" \
      --disable-tele --disable-ansi > "$log" 2>&1 &
  else
    rm -f docker-compose.yml
    local host=0.0.0.0
    [ "$mode" = v4mapped ] && host=::ffff:127.0.0.1
    sudo -E env PATH="$PATH" APP_PORT="$port" APP_HOST="$host" "$RECORD_BIN" record -c "python3 -u app.py" \
      --disable-tele --disable-ansi > "$log" 2>&1 &
  fi
  pid=$!
  url="http://127.0.0.1:$port/first"
  got=$(python3 first.py "$log" "$pid" "$url")
  if [ "$got" != ok ]; then
    echo "FAIL: $mode run $n: the call made as the app listened got: $got"; FAIL=1
  elif ! await "$log" "$pid" "Keploy has captured test cases"; then
    echo "FAIL: $mode run $n: the call made as the app listened was answered and not recorded"; FAIL=1
  else
    echo "$mode run $n: the call made as the app listened was answered and recorded"
  fi
  if [ -n "$xpid" ]; then
    got=$(curl -gs --max-time 10 "http://[::1]:$port/x" || echo refused)
    [ "$got" = X ] || { echo "FAIL: $mode run $n: the other listener on [::1]:$port did not get its call: $got"; FAIL=1; }
    if grep -aq "another listener, not keploy's, holds the application's port" "$log"; then
      echo "FAIL: $mode run $n: keploy yielded the app's port to a listener its bind does not conflict with"; FAIL=1
    fi
  fi
  sudo kill -INT "$pid" 2>/dev/null
  wait "$pid"
  [ -n "$xpid" ] && { kill "$xpid" 2>/dev/null; wait "$xpid" 2>/dev/null; }
  grep -a "served calls to the application's port that arrived before its forwarder listened" "$log" | head -n 1
  if grep -aq "keploy cannot hold the application's port from its bind" "$log"; then
    echo "FAIL: $mode run $n: keploy could not hold the app's port on this runner's kernel ($(uname -r))"
    grep -a "keploy cannot hold the application's port" "$log" | head -n 1; FAIL=1
  fi
  [ "$FAIL" -eq 0 ] || tail -40 "$log"
  cleanup
}

for mode in $MODES; do
  for n in $(seq 1 "$RUNS"); do
    run "$mode" "$n"
  done
done

if [ "$FAIL" -ne 0 ]; then
  echo "record-first-call: FAILED"
  exit 1
fi
echo "record-first-call: every call made as the app listened was answered and recorded"
