#!/usr/bin/env bash
# e2e, under the proven switch, of keploy's own stop of a docker compose app
# (P101-c). keploy runs its agent as a service of the app's stack, the app in
# the agent's network and pid namespaces. Under the switch compose runs
# without --abort-on-container-exit: keploy stops the app's container itself
# at each test set's end (SIGINT, its exit within the grace, then a kill it
# says), its stack and keploy's agent running on, reads the set's end from the
# live agent, and starts the app again for the next set.
#
# The app listens on the same port inside its container as on the host
# (PORT:PORT): keploy sends a test to the port the recording names, which is
# the container's.
#
# The app answers GET /price/<id> with its dependency's answer, and as it is
# stopped publishes POST /shutdown {"msg": $SHUTDOWN_MSG} to the dependency
# (a queue flushed on the way out). Two recordings of two calls each, made
# under the switch, each hold that publish: keploy record's stop is keploy's
# own too, and waits for the app's shutdown.
#
#   1. Both sets replayed as recorded: both pass, keploy test exits 0. The
#      second set ran against the app keploy started again, and neither
#      report says it was read with the app running (app_not_stopped).
#   2. A changed shutdown publish (SHUTDOWN_MSG=changed): both sets fail, and
#      each report names the publish among the calls made after the set's
#      last test. Read with the app running, the publish came after the read
#      and passed.
#   3. The app exits on its own after its first answer (HALT_AFTER=1): the
#      first set is APP_HALTED, as compose's abort made it, though keploy ran
#      compose without it.
#   4. keploy is interrupted (SIGTERM) as it replays: it exits, and leaves no
#      container and no network of the project behind.
#   5. keploy is given the app by its service's key (--container-name app),
#      the service setting no container_name, so compose names its container
#      <project>-app-1 (P101-b-n). keploy record's stop and each set's end
#      stop are of the container compose made for the service: the
#      recording holds the shutdown publish, the set as recorded passes, and
#      a changed publish fails it. Read by the name, the stop found no
#      container and took that for an app it had stopped: the recording
#      lacked the publish, and the replay of a changed one passed, exit 0.
#   6. The app's service has restart: always and stop_signal: SIGTERM
#      (P101-c-A2 SF-3(b)). Its first act is a startup call (POST /boot, a
#      fast boot, as a Go app's), the two recordings' differ (BOOT_MSG a,
#      then b), and the second recording is big (one 16 MB answer), so
#      staging it takes keploy a while. keploy interrupts the app with its
#      own stop signal, which the daemon takes for a stop, so it does not
#      start it again: the replay booting a starts the app's container twice
#      (compose's up, keploy's start for the second set), the first set
#      passes, and the second fails on its startup call, which keploy's start
#      made once the set was staged. Interrupted with SIGINT, the daemon
#      started the app again 100 ms after each of keploy's stops; its startup
#      call came while keploy staged the second set, which no set judged, and
#      the second set passed, keploy test exit 0 (2 of 3 replays on
#      a00bfa851).
# After every run no container or network of the project is left.
#
# Every wait is on an event: a line in a log, a file, or keploy's own exit.
#
# Env: RECORD_BIN, REPLAY_BIN (the keploy binary under test). The agent image
# the binary names must be loaded (.github/actions/download-image).
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-$RECORD_BIN}"
IMAGE=python:3.11-slim
DEP_IMAGE=python:3.11-alpine
WORK="$(mktemp -d)"
cd "$WORK" || exit 1
FAIL=0
ID=$$
PROJECT="cps$ID"
APP="cps-app-$ID"
PORT=$(python3 -c 'import socket; s = socket.socket(); s.bind(("", 0)); print(s.getsockname()[1])')
# KNAME is the name keploy is given the app by (--container-name).
KNAME="$APP"
fail() {
  echo "FAIL: $*"
  FAIL=$((FAIL + 1))
}

# Tear down only this run's project, by name. Never a blanket prune.
cleanup() {
  [ -f docker-compose.yml ] && sudo docker compose -p "$PROJECT" down --remove-orphans >/dev/null 2>&1
  return 0
}
finish() {
  cleanup
  cd / && sudo rm -rf "$WORK"
}
trap finish EXIT

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/docker-build-retry.sh"
docker_pull_retry "$IMAGE"
docker_pull_retry "$DEP_IMAGE"

cat > dep.py <<'PY'
import base64, json, os, http.server
class H(http.server.BaseHTTPRequestHandler):
    def answer(self, obj):
        b = json.dumps(obj).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    def do_GET(self):
        if self.path.startswith("/blob/"):
            # A big, incompressible answer: a big recording to stage.
            self.answer({"blob": base64.b64encode(os.urandom(int(self.path.split("/")[2]))).decode()})
            return
        self.answer({"path": self.path, "price": 42})
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length") or 0))
        self.answer({"published": json.loads(body or b"{}")})
    def log_message(self, *a): pass
http.server.HTTPServer(("0.0.0.0", 9411), H).serve_forever()
PY

cat > app.py <<'PY'
import http.server, json, os, signal, time, urllib.request
DEP = os.environ["DEP_URL"]
MSG = os.environ.get("SHUTDOWN_MSG") or "bye"
BOOT = os.environ.get("BOOT_MSG") or ""
HALT_AFTER = int(os.environ.get("HALT_AFTER") or "0")
answered = 0
while BOOT:
    # A startup call, the app's first act (a fast boot), made once per
    # start of it, again while the dependency is not up yet; one answered
    # by no mock does not stop the app from listening.
    try:
        urllib.request.urlopen(urllib.request.Request(DEP + "/boot", data=json.dumps({"msg": BOOT}).encode(),
                               method="POST", headers={"Content-Type": "application/json"}), timeout=10).read()
        print("APP BOOTED", BOOT, flush=True)
        break
    except urllib.error.HTTPError as e:
        print("APP BOOT CALL FAILED", BOOT, e, flush=True)
        break
    except OSError as e:
        print("the dependency does not answer the boot call yet:", type(e).__name__, e, flush=True)
        time.sleep(0.2)
while True:
    try:
        urllib.request.urlopen(DEP + "/warmup").read()
        break
    except OSError as e:
        print("the dependency does not answer yet:", type(e).__name__, e, flush=True)
        time.sleep(1)
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        global answered
        if self.path.startswith("/bulk/"):
            urllib.request.urlopen(DEP + "/blob/" + self.path.split("/")[2]).read()
        b = urllib.request.urlopen(DEP + self.path).read()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
        self.wfile.flush()
        if self.path.startswith("/price/"):
            answered += 1
            if HALT_AFTER and answered >= HALT_AFTER:
                print("APP HALTS", flush=True)
                os._exit(3)
    def log_message(self, *a): pass
def stop(*_):
    raise KeyboardInterrupt
signal.signal(signal.SIGINT, stop)
signal.signal(signal.SIGTERM, stop)
s = http.server.HTTPServer(("0.0.0.0", int(os.environ["APP_PORT"])), H)
print("APP LISTENING", flush=True)
try:
    s.serve_forever()
except KeyboardInterrupt:
    pass
req = urllib.request.Request(DEP + "/shutdown", data=json.dumps({"msg": MSG}).encode(), method="POST",
                             headers={"Content-Type": "application/json"})
urllib.request.urlopen(req, timeout=10).read()
print("APP SHUTDOWN PUBLISHED", MSG, flush=True)
PY

cat > docker-compose.yml <<YML
services:
  app:
    image: $IMAGE
    container_name: $APP
    working_dir: /w
    volumes: [".:/w:z"]
    command: python app.py
    ports: ["$PORT:$PORT"]
    environment:
      - APP_PORT=$PORT
      - DEP_URL=http://dep:9411
      - SHUTDOWN_MSG=\${SHUTDOWN_MSG:-bye}
      - BOOT_MSG=\${BOOT_MSG:-}
      - HALT_AFTER=\${HALT_AFTER:-0}
    depends_on: [dep]
  dep:
    image: $DEP_IMAGE
    container_name: cps-dep-$ID
    working_dir: /w
    volumes: [".:/w:z"]
    command: python dep.py
YML

# await LOG PID PATTERN N: 0 once LOG has N whole lines that carry PATTERN, 1
# when keploy (PID) exited first.
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
test_files() { sudo sh -c 'ls keploy/test-set-*/tests/ 2>/dev/null' | grep -c '\.yaml$'; }
# pid_running PID: whether PID still runs, by its state in /proc, which shows
# root's processes to this user too: gone, or a zombie, is not running. Not
# kill -0: keploy runs under sudo here, and while sudo checks its policy it
# holds every uid as root, so kill -0 from this user fails (EPERM) on a sudo
# that is alive, and keploy would be taken for ended before it started.
pid_running() {
  local st
  case $1 in "" | *[!0-9]*) return 1 ;; esac
  { read -r st <"/proc/$1/stat"; } 2>/dev/null || return 1
  st=${st##*) }
  [ "${st%% *}" != Z ]
}

# wait_tests N PID: until the recordings hold N test cases, or keploy (PID)
# exits.
wait_tests() {
  while [ "$(test_files)" -lt "$1" ]; do
    pid_running "$2" || return 1
    sleep 0.2
  done
}
# signal_keploy SUDO_PID SIG: SIG to the keploy that sudo (SUDO_PID) runs, its
# child: nothing is looked up by name.
signal_keploy() {
  local child
  child="$(pgrep -P "$1" 2>/dev/null | head -n1)"
  [ -n "$child" ] && sudo kill "-$2" "$child" 2>/dev/null
}
# no_orphans STEP: no container and no network of the project is left.
no_orphans() {
  local left nets
  left=$(sudo docker ps -aq --filter "label=com.docker.compose.project=$PROJECT")
  nets=$(sudo docker network ls -q --filter "label=com.docker.compose.project=$PROJECT")
  if [ -n "$left$nets" ]; then
    fail "$1: keploy left the project's containers or networks behind: $(sudo docker ps -a --filter "label=com.docker.compose.project=$PROJECT" --format '{{.Names}} {{.Status}}') $(sudo docker network ls --filter "label=com.docker.compose.project=$PROJECT" --format '{{.Name}}')"
    cleanup
  fi
}
newest_run() { sudo sh -c 'ls -1d keploy/reports/test-run-* 2>/dev/null' | sort -V | tail -n1; }
report() { echo "$(newest_run)/$1-report.yaml"; }
set_status() { sudo grep -m1 '^status:' "$(report "$1")" 2>/dev/null | awk '{print $2}'; }

# record N [VAR=VALUE...]: a recording of /price/r<N>a and /price/r<N>b,
# under the switch, and, with BULK set, of /bulk/$BULK (one dependency call
# whose answer is BULK random bytes). keploy record's stop, its own, waits
# for the app's shutdown publish, which the recording must hold.
record() {
  local n=$1 log="record$1.log" pid before=$FAIL
  shift
  sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$@" "$RECORD_BIN" record -c "docker compose -p $PROJECT up" --container-name "$KNAME" \
    --disable-tele > "$log" 2>&1 &
  pid=$!
  if await "$log" "$pid" "APP LISTENING" 1; then
    curl -fsS -o /dev/null "http://127.0.0.1:$PORT/price/r${n}a" || fail "record $n: GET /price/r${n}a failed"
    curl -fsS -o /dev/null "http://127.0.0.1:$PORT/price/r${n}b" || fail "record $n: GET /price/r${n}b failed"
    local want=$((2 * (n + 1)))
    if [ -n "${BULK:-}" ]; then
      curl -fsS -o /dev/null "http://127.0.0.1:$PORT/bulk/$BULK" || fail "record $n: GET /bulk/$BULK failed"
      want=$((want + 1))
    fi
    # A call that failed writes no test case, so its count is never reached:
    # wait only when every call was made.
    if [ "$FAIL" -eq "$before" ]; then
      wait_tests "$want" "$pid" || fail "record $n: keploy record wrote $(test_files) test cases, want $want"
    fi
  else
    fail "record $n: keploy exited before the app listened"
  fi
  signal_keploy "$pid" INT
  wait "$pid"
  grep -q "APP SHUTDOWN PUBLISHED bye" "$log" || fail "record $n: the app did not publish as keploy record stopped it"
  [ "$FAIL" -eq "$before" ] || {
    tail -60 "$log"
    # A call reset as the app came up: did keploy's ingress forwarder hold
    # the app's port yet? (It starts on the app's bind, after the app does.)
    grep -n "APP LISTENING\|Intercepted application bind event\|Started ingress forwarding" "$log"
  }
  no_orphans "record $n"
}

# replay NAME [VAR=VALUE...]: keploy test of both recordings under the
# switch; RC is its exit code.
replay() {
  local name=$1 before
  shift
  before="$(newest_run)"
  sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$@" "$REPLAY_BIN" test -c "docker compose -p $PROJECT up" --container-name "$KNAME" \
    --delay 5 --disable-tele > "$name.log" 2>&1 </dev/null
  RC=$?
  [ "$(newest_run)" != "$before" ] || fail "$name: keploy test wrote no report (exit $RC)"
  no_orphans "$name"
}

echo "== recordings =="
record 0
record 1
for s in test-set-0 test-set-1; do
  sudo grep -q '/shutdown' "keploy/$s/mocks.yaml" 2>/dev/null || fail "$s: the shutdown publish was not recorded"
done

echo "== 1. both sets as recorded =="
replay as-recorded
[ "$RC" -eq 0 ] || fail "1: keploy test exited $RC, want 0"
for s in test-set-0 test-set-1; do
  [ "$(set_status $s)" = PASSED ] || fail "1: $s is $(set_status $s), want PASSED"
  if sudo grep -q '^app_not_stopped:' "$(report $s)" 2>/dev/null; then
    fail "1: $s was read with the app running: $(sudo grep -m1 '^app_not_stopped:' "$(report $s)")"
  fi
done
[ "$FAIL" -eq 0 ] || tail -80 as-recorded.log

echo "== 2. a changed shutdown publish =="
replay changed SHUTDOWN_MSG=changed
[ "$RC" -ne 0 ] || fail "2: keploy test exited 0, want non-zero"
for s in test-set-0 test-set-1; do
  [ "$(set_status $s)" = FAILED ] || fail "2: $s is $(set_status $s), want FAILED"
  sudo grep -A6 '^unclaimed_calls:' "$(report $s)" 2>/dev/null | grep -q '/shutdown' ||
    fail "2: $s does not name the publish among the calls made after its last test"
done

echo "== 3. the app exits on its own =="
replay halts HALT_AFTER=1
[ "$(set_status test-set-0)" = APP_HALTED ] || fail "3: test-set-0 is $(set_status test-set-0), want APP_HALTED"
# The halt takes the stack down, as compose's abort did, whichever of keploy's
# watch and its set-end stop sees it first: the next set cannot pass.
[ "$(set_status test-set-1)" != PASSED ] || fail "3: test-set-1 passed after the app's halt; the halt must take its stack down"

echo "== 4. keploy interrupted as it replays =="
sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$REPLAY_BIN" test -c "docker compose -p $PROJECT up" --container-name "$APP" \
  --delay 5 --disable-tele > interrupted.log 2>&1 </dev/null &
pid=$!
if await interrupted.log "$pid" "starting test for" 1; then
  signal_keploy "$pid" TERM
else
  fail "4: keploy exited before its first test"
fi
wait "$pid"
no_orphans "4"

# variant NAME [COMPOSE_APP_KEYS]: the cases below each run in a directory
# and a project of their own (NAME), the app's service in its compose file as
# above, its container named $APP-NAME, plus the keys COMPOSE_APP_KEYS gives;
# a COMPOSE_APP_KEYS that begins no-container-name sets no container_name,
# so compose names the container.
variant() {
  local keys=${2:-} cname="    container_name: $APP-$1"
  case "$keys" in no-container-name*) cname="" keys="${keys#no-container-name}" ;; esac
  cleanup
  mkdir -p "$WORK/$1" && cp "$WORK/app.py" "$WORK/dep.py" "$WORK/$1/" && cd "$WORK/$1" || exit 1
  PROJECT="cps$1$ID" KNAME="$APP-$1"
  cat > docker-compose.yml <<YML
services:
  app:
    image: $IMAGE
$cname
    working_dir: /w
    volumes: [".:/w:z"]
    command: python app.py
    ports: ["$PORT:$PORT"]
    environment:
      - APP_PORT=$PORT
      - DEP_URL=http://dep:9411
      - SHUTDOWN_MSG=\${SHUTDOWN_MSG:-bye}
      - BOOT_MSG=\${BOOT_MSG:-}
      - HALT_AFTER=\${HALT_AFTER:-0}
    depends_on: [dep]
$keys
  dep:
    image: $DEP_IMAGE
    working_dir: /w
    volumes: [".:/w:z"]
    command: python dep.py
YML
}
# app_starts SINCE: how many times the daemon started the app's container
# since SINCE (a time), as its events say.
app_starts() {
  sudo docker events --since "$1" --until "$(date +%s.%N)" --filter type=container --filter event=start \
    --filter "label=com.docker.compose.project=$PROJECT" --filter label=com.docker.compose.service=app --format '{{.Actor.ID}}' | wc -l
}

echo "== 5. keploy given the app by its service's key =="
variant svckey no-container-name
KNAME=app
record 0
sudo grep -q '/shutdown' keploy/test-set-0/mocks.yaml 2>/dev/null || fail "5: the shutdown publish was not recorded: keploy record did not stop the app's container"
replay svckey-as-recorded
[ "$RC" -eq 0 ] || fail "5: keploy test exited $RC as recorded, want 0"
[ "$(set_status test-set-0)" = PASSED ] || fail "5: test-set-0 is $(set_status test-set-0) as recorded, want PASSED"
replay svckey-changed SHUTDOWN_MSG=changed
[ "$RC" -ne 0 ] || fail "5: a changed shutdown publish: keploy test exited 0, want non-zero: the set's end was read with the app running"
[ "$(set_status test-set-0)" = FAILED ] || fail "5: a changed shutdown publish: test-set-0 is $(set_status test-set-0), want FAILED"
sudo grep -A6 '^unclaimed_calls:' "$(report test-set-0)" 2>/dev/null | grep -q '/shutdown' ||
  fail "5: test-set-0 does not name the publish among the calls made after its last test"
[ "$FAIL" -eq 0 ] || tail -60 svckey-changed.log

echo "== 6. the app's restart policy =="
variant restart "    restart: always
    stop_signal: SIGTERM"
record 0 BOOT_MSG=a
BULK=16000000 record 1 BOOT_MSG=b
since=$(date +%s.%N)
replay restart-boot-a BOOT_MSG=a
starts=$(app_starts "$since")
[ "$starts" -eq 2 ] || fail "6: the app's container was started $starts times in the replay of two sets, want 2 (compose's up, keploy's start for the second set): the daemon started it again after keploy's stop"
[ "$(set_status test-set-0)" = PASSED ] || fail "6: test-set-0, recorded booting a, replayed booting a, is $(set_status test-set-0), want PASSED"
[ "$(set_status test-set-1)" = FAILED ] || fail "6: test-set-1, recorded booting b, replayed booting a, is $(set_status test-set-1), want FAILED: its startup call was judged by no set"
[ "$RC" -ne 0 ] || fail "6: keploy test exited 0, want non-zero"
[ "$FAIL" -eq 0 ] || tail -80 restart-boot-a.log
cd "$WORK" || exit 1

# A race build reports a data race and carries on: fail on one in any run.
if grep -l "WARNING: DATA RACE" ./*.log ./*/*.log 2>/dev/null; then
  fail "a data race, in the logs above"
fi

if [ "$FAIL" -ne 0 ]; then
  echo "::error::$FAIL check(s) failed"
  exit 1
fi
echo "compose proven set-end e2e passed"
