#!/usr/bin/env bash
# e2e for an agent whose proxy or DNS port is taken in its own container's
# network namespace, by Docker's embedded DNS resolver. On a user-defined
# network, as every compose project's is, the resolver listens in the agent
# container's namespace, which the app joins, on 127.0.0.11, on a TCP and a
# UDP port the kernel drew from the namespace's ephemeral range (32768-60999)
# before the agent ran. The CLI checks the ports it hands the agent in its own
# namespace, where they are free, and the agent cannot name their holder,
# dockerd, which is in another PID namespace. An agent handed one of them must
# move that server to a free port and tell its hooks, which redirect the
# app's calls and DNS queries there, as it does natively; failing its start
# fails the run (a compose lane met it by chance, on ports CI hands out from
# 30000 up).
#
# The collision is made, not waited for: the agent image runs behind a
# wrapper entrypoint (port-taken-entry.sh) that reads the resolver's
# listeners in its namespace and hands the agent their ports in place of
# marker ports the CLI was given. The app reaches its dependency by its
# compose service name, so every call needs an answer of keploy's DNS server,
# and its HTTP calls go through keploy's proxy. Each leg records three calls
# and replays them:
#   1. the proxy on the resolver's TCP port and the DNS server on its UDP
#      port: both move;
#   2. the DNS server on the resolver's TCP port: it moves.
# Each fails on a keploy whose agent fails its start on a taken port, and on
# one that moves a server without telling its hooks (the app's calls, or
# its lookups, then reach nothing).
#
# Env: RECORD_BIN (the keploy binary under test). The agent image the binary
# names must be loaded (.github/actions/download-image).
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true

RECORD_BIN="${RECORD_BIN:-keploy}"
AGENT_IMAGE="${KEPLOY_AGENT_IMAGE_UNDER_TEST:-ghcr.io/keploy/keploy:v3-dev}"
IMAGE=python:3.11-slim
WORK="$(mktemp -d)"
cd "$WORK" || exit 1
FAIL=0
# Names of this run's own: two runs on one runner never share a container.
ID=$$
APP="ptk-app-$ID"
ORIG="keploy-port-taken-orig:$ID"
source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/free-port.sh"
# The app's port on this host, and the marker ports the CLI hands the agent:
# free here, below every ephemeral range, so the CLI keeps them.
PORT=$(free_port) || exit 1
M_PROXY_TCP=$(free_port "$PORT") || exit 1
M_DNS_TCP=$(free_port "$PORT" "$M_PROXY_TCP") || exit 1
M_DNS_UDP=$(free_port "$PORT" "$M_PROXY_TCP" "$M_DNS_TCP") || exit 1
FREE_PROXY=$(free_port "$PORT" "$M_PROXY_TCP" "$M_DNS_TCP" "$M_DNS_UDP") || exit 1

cleanup() {
  [ -f docker-compose.yml ] && sudo docker compose down --remove-orphans -t 2 >/dev/null 2>&1
  sudo docker rm -f "$APP" >/dev/null 2>&1
  return 0
}
# The agent image is put back as it was loaded, for the job's later steps.
finish() {
  cleanup
  if sudo docker image inspect "$ORIG" >/dev/null 2>&1; then
    sudo docker tag "$ORIG" "$AGENT_IMAGE" && sudo docker rmi "$ORIG" >/dev/null 2>&1
  fi
  cd / && sudo rm -rf "$WORK"
}
trap finish EXIT

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/docker-build-retry.sh"
docker_pull_retry "$IMAGE"
sudo docker image inspect "$AGENT_IMAGE" >/dev/null 2>&1 ||
  { echo "FAIL: the agent image $AGENT_IMAGE is not loaded"; exit 1; }

# The wrapper: in the agent container's namespace, before the agent runs, it
# finds the resolver's TCP listener and UDP socket on 127.0.0.11 (0B00007F in
# /proc/net's little-endian hex) and hands the agent their ports where the
# CLI handed it a marker: --proxy-port M_PROXY_TCP -> the TCP port,
# --dns-port M_DNS_TCP -> the TCP port, --dns-port M_DNS_UDP -> the UDP port.
mkdir -p img
cat > img/port-taken-entry.sh <<'SH'
#!/bin/bash
tcp=$(awk 'NR>1 && $4=="0A" && $2 ~ /^0B00007F:/ {split($2,a,":"); print a[2]; exit}' /proc/net/tcp)
udp=$(awk 'NR>1 && $2 ~ /^0B00007F:/ {split($2,a,":"); print a[2]; exit}' /proc/net/udp)
if [ -z "$tcp" ] || [ -z "$udp" ]; then
  echo "port-taken: no resolver socket on 127.0.0.11 in this namespace (tcp=$tcp udp=$udp)" >&2
  exit 97
fi
tcp=$((16#$tcp)); udp=$((16#$udp))
echo "port-taken: Docker's resolver listens on tcp 127.0.0.11:$tcp and udp 127.0.0.11:$udp here" >&2
args=(); prev=
for a in "$@"; do
  new=$a
  case "$prev:$a" in
    --proxy-port:"$M_PROXY_TCP") new=$tcp ;;
    --dns-port:"$M_DNS_TCP") new=$tcp ;;
    --dns-port:"$M_DNS_UDP") new=$udp ;;
  esac
  case "$a" in
    --proxy-port="$M_PROXY_TCP") new=--proxy-port=$tcp ;;
    --dns-port="$M_DNS_TCP") new=--dns-port=$tcp ;;
    --dns-port="$M_DNS_UDP") new=--dns-port=$udp ;;
  esac
  [ "$new" != "$a" ] && echo "port-taken: ${prev:+$prev }$a -> $new" >&2
  args+=("$new"); prev=$a
done
exec /app/entrypoint.sh "${args[@]}"
SH
sudo docker tag "$AGENT_IMAGE" "$ORIG" || { echo "FAIL: cannot keep the loaded agent image"; exit 1; }
cat > img/Dockerfile <<DOCKERFILE
FROM $ORIG
ENV M_PROXY_TCP=$M_PROXY_TCP M_DNS_TCP=$M_DNS_TCP M_DNS_UDP=$M_DNS_UDP
COPY port-taken-entry.sh /app/port-taken-entry.sh
RUN chmod +x /app/port-taken-entry.sh
ENTRYPOINT ["/app/port-taken-entry.sh", "/app/keploy", "agent", "--is-docker"]
DOCKERFILE
sudo docker build -q -t "$AGENT_IMAGE" img >/dev/null ||
  { echo "FAIL: cannot build the wrapped agent image"; exit 1; }

# The app answers each GET with its dependency's answer, reached by its
# service name: each call is a lookup keploy's DNS server answers and a call
# through keploy's proxy.
cat > app.py <<'PY'
import http.server, urllib.request
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        n = len(urllib.request.urlopen("http://upstream:8000/", timeout=10).read())
        b = ("%s: upstream answered %d bytes\n" % (self.path, n)).encode()
        self.send_response(200); self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    def log_message(self, *a): pass
s = http.server.ThreadingHTTPServer(("0.0.0.0", 8080), H)
print("APP LISTENING", flush=True)
s.serve_forever()
PY
cat > docker-compose.yml <<YML
services:
  upstream:
    image: $IMAGE
    command: ["python", "-m", "http.server", "8000"]
  app:
    image: $IMAGE
    container_name: $APP
    working_dir: /w
    volumes: [".:/w:z"] # :z relabels on SELinux hosts; ignored elsewhere
    command: ["python", "-u", "app.py"]
    ports: ["$PORT:8080"]
    depends_on: [upstream]
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

# moved LOG LEG SERVER...: each SERVER ("proxy", "DNS server") said its port
# was taken and where it listens instead.
moved() {
  local log=$1 leg=$2 s
  shift 2
  grep -q "port-taken: Docker's resolver listens" "$log" ||
    { echo "FAIL: $leg: the wrapper did not find the resolver's sockets"; grep -a "port-taken:" "$log" | head -5; FAIL=$((FAIL + 1)); return; }
  for s in "$@"; do
    grep -aqE "the $s's port [0-9]+ is taken.*the $s listens on port [0-9]+ instead" "$log" ||
      { echo "FAIL: $leg: the $s did not move off the resolver's port"; grep -aE "port-taken:|is taken|failed to start" "$log" | head -8; FAIL=$((FAIL + 1)); }
  done
}

# leg NAME PROXY_PORT DNS_PORT SERVER...: keploy records the app with those
# ports while the lane makes three calls through it, is stopped once their
# test cases are written, and replays them; each SERVER must move, in record
# and in replay.
leg() {
  local name=$1 pp=$2 dp=$3 log="$1-record.log" tlog="$1-test.log" pid rc made warm p before=$FAIL end
  shift 3
  sudo rm -rf keploy
  : > "$log"
  sudo -E env PATH="$PATH" "$RECORD_BIN" record -c "docker compose up" --container-name "$APP" \
    --proxy-port "$pp" --dns-port "$dp" --disable-tele --disable-ansi --generate-github-actions=false > "$log" 2>&1 &
  pid=$!
  made=0 warm=0
  if await "$log" "$pid" "APP LISTENING" 1; then
    # The dependency may not listen yet: a first call is retried until it
    # answers, for up to 60 s.
    end=$((SECONDS + 60))
    until curl -fsS -m 10 -o /dev/null "http://127.0.0.1:$PORT/warmup" 2>/dev/null && warm=1; do
      [ "$SECONDS" -lt "$end" ] || break
      sleep 1
    done
    for p in /a /b /c; do
      if curl -fsS -m 10 -o /dev/null "http://127.0.0.1:$PORT$p"; then
        made=$((made + 1))
      else
        echo "FAIL: $name: GET $p through the app failed"; FAIL=$((FAIL + 1))
      fi
    done
    [ "$((made + warm))" -eq 0 ] || await "$log" "$pid" "Keploy has captured test cases" "$((made + warm))" ||
      { echo "FAIL: $name: keploy did not write the test cases of the calls made"; FAIL=$((FAIL + 1)); }
    sudo kill -INT "$pid" 2>/dev/null
  else
    echo "FAIL: $name: keploy exited before the app listened"; FAIL=$((FAIL + 1))
  fi
  wait "$pid"
  rc=$?
  cleanup
  [ "$rc" -eq 0 ] || { echo "FAIL: $name: keploy record exited $rc"; FAIL=$((FAIL + 1)); }
  moved "$log" "$name record" "$@"
  local dnsm
  dnsm=$(sudo sh -c 'cat keploy/test-set-0/mocks.yaml' 2>/dev/null | grep -c "kind: DNS")
  [ "${dnsm:-0}" -ge 1 ] || { echo "FAIL: $name: no DNS mock recorded: the app's lookups did not reach keploy's DNS server"; FAIL=$((FAIL + 1)); }
  echo "$name record: $made/3 calls answered, keploy exited $rc, ${dnsm:-0} DNS mocks"
  if [ "$FAIL" -ne "$before" ]; then
    tail -40 "$log"
    return
  fi

  sudo rm -rf keploy/reports
  sudo -E env PATH="$PATH" "$RECORD_BIN" test -c "docker compose up" --container-name "$APP" \
    --proxy-port "$pp" --dns-port "$dp" --delay 5 --disable-tele --disable-ansi > "$tlog" 2>&1
  rc=$?
  cleanup
  local status
  status=$(sudo sh -c 'cat keploy/reports/test-run-*/test-set-0-report.yaml 2>/dev/null' | sed -n 's/^status: //p' | tail -n 1)
  echo "$name replay: keploy test exited $rc, report ${status:-missing}"
  [ "$rc" -eq 0 ] && [ "$status" = PASSED ] || { echo "FAIL: $name: the replay did not pass"; FAIL=$((FAIL + 1)); }
  moved "$tlog" "$name replay" "$@"
  [ "$FAIL" -eq "$before" ] || tail -40 "$tlog"
}

echo "== 1. the proxy on the resolver's TCP port, the DNS server on its UDP port =="
leg both "$M_PROXY_TCP" "$M_DNS_UDP" proxy "DNS server"

echo "== 2. the DNS server on the resolver's TCP port =="
leg dns-tcp "$FREE_PROXY" "$M_DNS_TCP" "DNS server"

if [ "$FAIL" -ne 0 ]; then
  echo "compose-agent-port-taken: FAILED"
  exit 1
fi
echo "compose-agent-port-taken: all legs passed"
