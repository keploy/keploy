#!/usr/bin/env bash
# e2e for container keys (the proven model, KEPLOY_PROVEN=1): `keploy mock
# record` names a dependency that runs in a container by the container, not by
# the random port Docker published it on.
#
# Three containers of one image play a Testcontainers setup, each on a random
# host port: A is the suite's, started before the run and called while the
# tests are collected (outside every test); B and C are started by test_one
# and test_two themselves, as Testcontainers does, and removed when pytest
# exits. Each is configured apart (WHO=a, b, c). The recording must carry, on
# each call's mock, its container's key,
#   container:<repository>:8000@<fingerprint of its configuration>
# A's on the calls outside tests and test_two's to A, B's in test_one, C's in
# test_two: three keys, one per configuration. Recording again with every
# container new, on new ports, must name them the same, and so must recording
# test_two alone. The switch's recording
# replays without the switch, and a recording made without the switch has no
# keys. A replay under the switch needs a build that judges each test, keploy
# enterprise: this build refuses it before it runs the tests. (The agent names
# a replay's connections as it names a record's, which pkg/agent/proxy's
# TestHandleConnectionNamesAContainerDestination covers.) The suite's harness
# speaks scope protocol v1 both ways (keploy_v1.py, conftest_v1.py).
# MOCK_LANE_SWITCH (lane_switch.sh) picks the half a run runs; CI runs each in
# a job of its own.
#
# Env: RECORD_BIN / REPLAY_BIN (the keploy binaries under test); DEP_IMAGE (the
# dependency's image, python:3.11-slim by default).
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true

if ! python3 -c "import pytest" 2>/dev/null; then
  python3 -m ensurepip --default-pip >/dev/null 2>&1 || true
  bash "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/fetch.sh" pip --quiet --disable-pip-version-check pytest
fi

# This lane's directory: the shared harness (keploy_v1.py, conftest_v1.py).
# shellcheck source-path=SCRIPTDIR
MOCK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-keploy}"
IMAGE="${DEP_IMAGE:-python:3.11-slim}"
export DEP_IMAGE="$IMAGE"
WORK="$(mktemp -d)"
cd "$WORK"
FAIL=0
SUITE=""

cleanup() {
  [ -n "$SUITE" ] && docker rm -f "$SUITE" >/dev/null 2>&1
  # Containers a test started and could not remove (a failed run).
  [ -f started.txt ] && xargs -r docker rm -f < started.txt >/dev/null 2>&1
  return 0
}
# The work directory holds keploy's root-owned recordings: removed with it.
finish() {
  cleanup
  cd / && sudo rm -rf "$WORK"
}
trap finish EXIT

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/docker-build-retry.sh"
docker_pull_retry "$IMAGE"

# The dependency: answers every GET with which container it is and the path.
# It says "ready" once it listens; whoever starts it waits for that line, not
# for a clock.
cat > server.py <<'PY'
import http.server, json, os
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        b = json.dumps({"who": os.environ["WHO"], "path": self.path}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)
    def log_message(self, *a): pass
s = http.server.HTTPServer(("0.0.0.0", 8000), H)
print("ready", flush=True)
s.serve_forever()
PY

# deps.py starts a container as Testcontainers does (docker run on a random
# published port, then wait for its ready line) and notes "<who> <port>" in
# seen.txt so the script can tell whom a destination was. A container stays
# up until pytest exits, so no later container can take its port within a
# run. Under the switch it also waits for keploy's agent to say it indexed
# that container (its debug line, in the log the run writes to): a
# connection made before the agent read a container it never waits for is
# named by its address, and this lane checks keys, not that race. The Docker
# CLI talks to the daemon over its unix socket, which keploy leaves alone.
cat > deps.py <<'PY'
import atexit, json, os, subprocess, time, urllib.request
SERVER = open(os.path.join(os.path.dirname(os.path.abspath(__file__)), "server.py")).read()
def running(cid):
    out = subprocess.run(["docker", "inspect", "-f", "{{.State.Running}}", cid], capture_output=True, text=True)
    return out.stdout.strip() == "true"
def indexed(cid):
    # The agent's line for this container in the run's log, which only grows:
    # indexed, or that its index stopped (a read that fails stops it). A
    # "did not index" is not final: Docker says a container joined its network
    # before it says it started, and the read for the first can find it not
    # yet running; the start reads it again. Read whole lines.
    log, want, seen, rest = os.environ.get("KEPLOY_LANE_LOG", ""), '"containerID": "%s"' % cid, 0, b""
    while True:
        with open(log, "rb") as f:
            f.seek(seen)
            chunk = f.read()
        seen += len(chunk)
        lines = (rest + chunk).split(b"\n")
        rest = lines.pop()
        for raw in lines:
            line = raw.decode("utf-8", "replace")
            if want in line and "indexed a container" in line and "did not" not in line:
                return
            if "the container index stopped" in line:
                raise RuntimeError("keploy's agent stopped reading Docker: %s" % line)
        if not running(cid):
            raise RuntimeError("container %s stopped before keploy's agent indexed it" % cid)
        time.sleep(0.05)
def start(who):
    cid = subprocess.check_output(["docker", "run", "-d", "-e", "WHO=" + who, "-p", "127.0.0.1::8000",
                                   os.environ["DEP_IMAGE"], "python3", "-c", SERVER], text=True).strip()
    with open("started.txt", "a") as f:
        f.write(cid + "\n")
    atexit.register(subprocess.run, ["docker", "rm", "-f", cid], stdout=subprocess.DEVNULL, check=False)
    logs = subprocess.Popen(["docker", "logs", "-f", cid], stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    ready = any(line.strip() == "ready" for line in logs.stdout)
    logs.kill()
    logs.wait()
    if not ready:
        raise RuntimeError("container %s exited before it listened" % who)
    if os.environ.get("KEPLOY_PROVEN") == "1":
        indexed(cid)
    out = subprocess.check_output(["docker", "port", cid, "8000/tcp"], text=True)
    port = [l for l in out.splitlines() if l.startswith("127.0.0.1:")][0].rsplit(":", 1)[1]
    with open("seen.txt", "a") as f:
        f.write("%s %s\n" % (who, port))
    return port
def note(who, port):
    with open("seen.txt", "a") as f:
        f.write("%s %s\n" % (who, port))
def get(port, path):
    # urllib opens one connection per call, so every call is its own
    # connection, named in the scope it is made in.
    with urllib.request.urlopen("http://127.0.0.1:%s%s" % (port, path)) as r:
        return json.load(r)
PY

# The test harness: keploy's scope protocol v1 (claim the run before pytest
# collects, then begin each test with its file and canSkip, and end it with
# its outcome), failing on any answer it cannot trust. Without the switch the
# agent has no claim route, and the harness speaks the scope API as it was.
# A lane that could not load it would skip both halves and pass: fail instead.
# shellcheck source=lane_switch.sh
if ! source "$MOCK_DIR/lane_switch.sh" || ! declare -F half >/dev/null; then
  echo "FAIL: could not load lane_switch.sh from $MOCK_DIR"
  exit 1
fi
cp "$MOCK_DIR/keploy_v1.py" keploy_v1.py || { echo "FAIL: no keploy_v1.py beside this lane, in $MOCK_DIR"; exit 1; }
cp "$MOCK_DIR/conftest_v1.py" conftest.py || { echo "FAIL: no conftest_v1.py beside this lane, in $MOCK_DIR"; exit 1; }

cat > test_containers.py <<'PY'
from deps import get, note, start
A = open("a.port").read().strip()
note("a", A)
# The suite's container, called while the tests are collected: outside tests.
BOOT = get(A, "/boot")
def test_one():
    assert BOOT["who"] == "a"
    port = start("b")
    assert get(port, "/one")["who"] == "b"
def test_two():
    port = start("c")
    assert get(port, "/two-c")["who"] == "c"
    assert get(A, "/two-a")["who"] == "a"
PY

# wait_ready ID: reads the container's log until its "ready" line (0), or
# until the log ends because the container exited first (1). docker logs is
# this shell's own child, so it is stopped by its pid, never by a pattern.
wait_ready() {
  local line ok=1 fifo pid
  fifo="$WORK/logs.$1.fifo"
  mkfifo "$fifo"
  docker logs -f "$1" >"$fifo" 2>&1 &
  pid=$!
  while IFS= read -r line; do
    if [ "$line" = ready ]; then ok=0; break; fi
  done <"$fifo"
  kill "$pid" 2>/dev/null
  wait "$pid" 2>/dev/null
  rm -f "$fifo"
  return $ok
}

# start_suite starts A, the suite's container, on a new random port.
start_suite() {
  cleanup
  SUITE=$(docker run -d -e WHO=a -p 127.0.0.1::8000 "$IMAGE" python3 -c "$(cat server.py)") || { echo "FAIL: cannot start the suite's container"; exit 1; }
  wait_ready "$SUITE" || { echo "FAIL: the suite's container exited before it listened"; docker logs "$SUITE"; exit 1; }
  docker port "$SUITE" 8000/tcp | grep -m1 '^127\.0\.0\.1:' | cut -d: -f2 > a.port
  [ -s a.port ] || { echo "FAIL: the suite's container has no published port"; exit 1; }
  : > seen.txt
  : > started.txt
}

CMD="python3 -m pytest -q -p no:cacheprovider test_containers.py"
# A key: the image's repository (no tag), the port, the configuration's
# fingerprint.
KEY_RE="^container:${IMAGE%%:*}:8000@[0-9a-f]{12}$"

# keys_of NAME: "<path> <containerKey or ->" for each HTTP mock of the set.
keys_of() {
  python3 - "keploy/$1/mocks.yaml" <<'PY'
import re, sys
try:
    docs = open(sys.argv[1]).read().split("\n---")
except FileNotFoundError:
    docs = []
for d in docs:
    if not re.search(r"^kind: Http$", d, re.M):
        continue
    url = re.search(r"^\s+url: (\S+)$", d, re.M)
    key = re.search(r"^\s+containerKey: (\S+)$", d, re.M)
    path = re.sub(r"^https?://[^/]+", "", url.group(1)) if url else "?"
    print(path, key.group(1) if key else "-")
PY
}

# record NAME LOG PROVEN [PYTEST ARGS]: records the set with the agent's debug
# lines, the suite's container and the tests' own all new, on new ports.
record() {
  local name="$1" log="$2" proven="$3"
  shift 3
  start_suite
  : > "$log"
  sudo -E env PATH="$PATH" DEP_IMAGE="$IMAGE" KEPLOY_PROVEN="$proven" KEPLOY_LANE_LOG="$WORK/$log" "$RECORD_BIN" mock record -c "$CMD $*" --name "$name" --disable-tele --debug > "$log" 2>&1
  local rc=$?
  grep -E "[0-9]+ (passed|failed|error)" "$log" | tail -3
  if [ "$rc" -ne 0 ]; then
    echo "FAIL: record of $name ($log) exited $rc"
    tail -40 "$log"
    FAIL=1
  fi
}

# untouched LOG WHAT: an agent without the switch never reads Docker for
# container keys. Its debug log has none of the index's or the keyer's lines.
CK_LINES="named a container destination|indexed a container|did not index a container|the container index stopped|names container destinations|cannot read the containers of the Docker daemon"
untouched() {
  if grep -E "$CK_LINES" "$1"; then
    echo "FAIL: $2"
    FAIL=1
  fi
}

# expect FILE WANT...: FILE's lines are WANT's, one per argument.
expect() {
  local file="$1" what="$2"
  shift 2
  printf '%s\n' "$@" > "$file.want"
  if ! diff -u "$file.want" "$file"; then
    echo "FAIL: $what"
    FAIL=1
  fi
}

# key_at FILE PATH: the key FILE's line for PATH carries.
key_at() { awk -v p="$2" '$1 == p { print $2 }' "$1"; }

if half on; then
echo "== 1. record under the proven switch =="
record ck rec.log 1
grep -q "2 passed" rec.log || { echo "FAIL: pytest did not pass both tests while recording"; FAIL=1; }
keys_of ck | sort > keys.txt
cat keys.txt
KA=$(key_at keys.txt /boot)
KB=$(key_at keys.txt /one)
KC=$(key_at keys.txt /two-c)
for k in "$KA" "$KB" "$KC"; do
  grep -qE "$KEY_RE" <<<"$k" || { echo "FAIL: '$k' is not a container key of $IMAGE's port 8000"; FAIL=1; }
done
[ "$KA" != "$KB" ] && [ "$KA" != "$KC" ] && [ "$KB" != "$KC" ] || { echo "FAIL: containers configured apart share a key"; FAIL=1; }
expect keys.txt "the recorded container keys are not A's outside tests and in test_two, B's and C's in their tests" \
  "/boot $KA" "/one $KB" "/two-a $KA" "/two-c $KC"

echo "== 2. record again, every container new on a new random port =="
record ck-again rec-again.log 1
grep -q "2 passed" rec-again.log || { echo "FAIL: pytest did not pass both tests recording again"; FAIL=1; }
keys_of ck-again | sort > keys-again.txt
cat keys-again.txt
expect keys-again.txt "new containers on new ports were not named as the first record named the old ones" \
  "/boot $KA" "/one $KB" "/two-a $KA" "/two-c $KC"

echo "== 3. record test_two alone: its own container keeps its key =="
record ck-two rec-two.log 1 -k test_two
grep -q "1 passed" rec-two.log || { echo "FAIL: pytest did not pass test_two alone while recording"; FAIL=1; }
keys_of ck-two | sort > keys-two.txt
cat keys-two.txt
expect keys-two.txt "test_two alone did not name its containers as the full run did" \
  "/boot $KA" "/two-a $KA" "/two-c $KC"

echo "== 4. the switch's recording replays without the switch =="
start_suite
sudo -E env PATH="$PATH" DEP_IMAGE="$IMAGE" KEPLOY_PROVEN= "$REPLAY_BIN" mock replay -c "$CMD" --name ck --disable-tele --debug > rep-off.log 2>&1
RC=$?
grep -E "[0-9]+ (passed|failed|error)" rep-off.log | tail -3
[ "$RC" -eq 0 ] || { echo "FAIL: the replay without the switch exited $RC"; tail -40 rep-off.log; FAIL=1; }
grep -q "2 passed" rep-off.log || { echo "FAIL: pytest did not pass both tests in the replay without the switch"; FAIL=1; }
untouched rep-off.log "an agent without the switch read Docker for container keys in replay"

echo "== 4b. under the switch this build refuses the replay, the tests never run =="
sudo -E env PATH="$PATH" DEP_IMAGE="$IMAGE" KEPLOY_PROVEN=1 "$REPLAY_BIN" mock replay -c "$CMD" --name ck --disable-tele > rep-on.log 2>&1
RC=$?
[ "$RC" -eq 1 ] || { echo "FAIL: under the switch this build should refuse the replay with exit 1, got $RC"; tail -20 rep-on.log; FAIL=1; }
grep -q "needs a keploy build that judges each test" rep-on.log || { echo "FAIL: the refused replay does not say it needs a build that judges each test"; FAIL=1; }
grep -qE "[0-9]+ (passed|failed|error)" rep-on.log && { echo "FAIL: under the switch the refused replay ran the tests"; FAIL=1; }

fi

if half off; then
echo "== 5. without the switch nothing is keyed =="
record ck-off rec-off.log ""
grep -q "2 passed" rec-off.log || { echo "FAIL: pytest did not pass both tests recording without the switch"; FAIL=1; }
untouched rec-off.log "an agent without the switch read Docker for container keys in record"
keys_of ck-off | sort > keys-off.txt
cat keys-off.txt
expect keys-off.txt "a recording made without the switch carries container keys" \
  "/boot -" "/one -" "/two-a -" "/two-c -"
fi

# A race build reports a data race and carries on: fail on one in any leg.
if grep -l "WARNING: DATA RACE" ./*.log; then
  echo "FAIL: a data race, in the logs above"
  FAIL=1
fi

if [ "$FAIL" -ne 0 ]; then
  echo "container keys e2e FAILED"
  exit 1
fi
echo "container keys e2e passed"
