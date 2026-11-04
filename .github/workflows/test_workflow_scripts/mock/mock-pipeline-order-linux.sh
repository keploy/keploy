#!/usr/bin/env bash
# End-to-end guard for the order `keploy mock replay` serves the recordings of
# one request time in: a pipeline of identical MySQL commands, sent in ONE
# write, is recorded as one mock per command, all at that write's request
# time, and must be answered with them in the order they were recorded.
#
#   1. Record against a real MySQL. One connection takes a user lock and
#      makes a temporary table with an AUTO_INCREMENT key. A second
#      connection asks for the lock and waits in MySQL. Once MySQL says it
#      waits (the driver asks MySQL through `docker exec`, so the question
#      is not recorded), the first sends 20 identical INSERTs in one write
#      and reads the 20 insert ids, 1..20. Once the recording holds their 20
#      mocks (the driver reads the mock file keploy writes), it releases the
#      lock and the waiter is answered. So the waiter's request is read
#      before the pipeline and its mock written after the pipeline's: the
#      mock file is not in time order, which made the agent's on-disk store,
#      sorting it by time, reorder the pipeline's mocks, all of one time. The
#      step checks that shape in the mock file before it replays.
#   2. Replay with MySQL DOWN and consume-once mocks
#      (KEPLOY_STRICT_MOCK_WINDOW=1): the 20 INSERTs must get 1..20 in turn.
#
# The driver speaks the MySQL protocol itself (no client library pipelines
# commands): the handshake as root with the empty password, COM_QUERY, and
# the OK, ERR and text result-set answers. Ports: 3306 (MySQL). Env:
# RECORD_BIN, REPLAY_BIN; MYSQL_IMAGE (default mysql:8.0). It runs without the
# proven switch: this build refuses a replay under it (mock-stateful-linux.sh).
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
# shellcheck source=../sudo_env.sh
source "$(dirname "${BASH_SOURCE[0]}")/../sudo_env.sh" || exit 1
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true

# docker_pull_retry: a registry hiccup is retried and named, not taken for a
# failed lane.
# shellcheck source-path=SCRIPTDIR source=../docker-build-retry.sh
source "$(dirname "${BASH_SOURCE[0]}")/../docker-build-retry.sh"

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-keploy}"
MYSQL_IMAGE="${MYSQL_IMAGE:-mysql:8.0}"
PORT=3306
N=20
WANT=$(seq -s, 1 "$N")
WORK="$(mktemp -d)"
cd "$WORK" || exit 1
FAIL=0

MYSQL_CID=""
stop_mysql() {
  [ -z "$MYSQL_CID" ] || docker rm -f "$MYSQL_CID" >/dev/null 2>&1
  MYSQL_CID=""
}
trap stop_mysql EXIT

# The driver, argv PORT N record|replay [MYSQL container]. Prints HELD= (the
# lock the first connection takes), PIPE= (the N insert ids, in the order they
# were answered), RELEASED= and LOCK= (the waiter's answer); MISS where no
# answer came, ERR:<message> for a MySQL error.
cat > pipe.py <<'PY'
import glob, socket, struct, subprocess, sys, threading, time
PORT, N, phase = int(sys.argv[1]), int(sys.argv[2]), sys.argv[3]
CID = sys.argv[4] if len(sys.argv) > 4 else ""

def frame(payload, seq):
    return struct.pack("<I", len(payload))[:3] + bytes([seq]) + payload

def query(sql):
    return frame(b"\x03" + sql.encode(), 0)

def lenenc(p, i):
    b = p[i]
    if b < 0xFB:
        return b, i + 1
    size = {0xFC: 2, 0xFD: 3, 0xFE: 8}[b]
    return int.from_bytes(p[i + 1:i + 1 + size], "little"), i + 1 + size

class Conn:
    def __init__(self):
        # The bound only keeps a broken run from hanging: an answer that does
        # not come is printed as MISS and fails the step by its content.
        self.s = socket.create_connection(("127.0.0.1", PORT), timeout=120)
        self.f = self.s.makefile("rb")
        greeting = self.packet()
        if greeting[0] != 10:
            raise RuntimeError("not a protocol-10 greeting: %r" % greeting[:16])
        # LONG_PASSWORD | CONNECT_WITH_DB | PROTOCOL_41 | SECURE_CONNECTION |
        # PLUGIN_AUTH; root, the empty password (an empty answer), database pipe.
        caps = 0x1 | 0x8 | 0x200 | 0x8000 | 0x80000
        login = struct.pack("<IIB", caps, 1 << 24, 45) + b"\0" * 23 + b"root\0" + b"\0" + b"pipe\0" + b"caching_sha2_password\0"
        self.s.sendall(frame(login, self.seq + 1))
        while True:
            p = self.packet()
            if p[0] == 0x00:
                return
            if p[0] == 0xFF:
                raise RuntimeError("login refused: %r" % p[9:])
            if p[0] == 0xFE:
                # Another auth method: the empty password again.
                self.s.sendall(frame(b"", self.seq + 1))
            # 0x01 (more auth data, the fast-auth result): read on.

    def packet(self):
        h = self.f.read(4)
        if len(h) < 4:
            raise EOFError("the connection closed")
        n = h[0] | h[1] << 8 | h[2] << 16
        self.seq = h[3]
        p = self.f.read(n)
        if len(p) < n:
            raise EOFError("the connection closed in a packet")
        return p

    def answer(self):
        """The next answer: an OK's insert id, an ERR, or a result set's first value."""
        p = self.packet()
        if p[0] == 0x00:
            _, i = lenenc(p, 1)
            return str(lenenc(p, i)[0])
        if p[0] == 0xFF:
            return "ERR:" + p[9:].decode(errors="replace")
        columns, _ = lenenc(p, 0)
        for _ in range(columns):
            self.packet()
        self.packet()  # EOF after the column definitions
        first = None
        while True:
            p = self.packet()
            if p[0] == 0xFE and len(p) < 9:
                return first if first is not None else "NOROW"
            if first is None:
                if p[0] == 0xFB:
                    first = "NULL"
                else:
                    size, i = lenenc(p, 0)
                    first = p[i:i + size].decode()

    def ask(self, sql):
        self.s.sendall(query(sql))
        return self.answer()

def safe(fn):
    try:
        return fn()
    except Exception as e:  # printed, so the step fails by what it got
        return "MISS(%s)" % e

def waiting():
    """How many sessions MySQL holds waiting for a user lock, asked outside the recording."""
    out = subprocess.run(["docker", "exec", CID, "mysql", "-uroot", "-N", "-B", "-e",
                          "SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE STATE = 'User lock'"],
                         capture_output=True, text=True)
    return int(out.stdout.strip() or 0) if out.returncode == 0 else -1

def recorded(text):
    """How many mocks the recording written so far holds with text in them."""
    n = 0
    for path in glob.glob("keploy/*/mocks.*"):
        try:
            n += open(path, errors="replace").read().count(text)
        except OSError:
            pass
    return n

b = Conn()
print("HELD=" + safe(lambda: b.ask("SELECT GET_LOCK('pipe', 0)")), flush=True)
safe(lambda: b.ask("CREATE TEMPORARY TABLE c (n INT AUTO_INCREMENT PRIMARY KEY)"))
lock = {}
def waiter():
    lock["v"] = safe(lambda: Conn().ask("SELECT GET_LOCK('pipe', 120)"))
t = threading.Thread(target=waiter)
t.start()
if phase == "record":
    # Until MySQL holds the waiter's GET_LOCK: its request is then read
    # before the pipeline's.
    while t.is_alive() and waiting() < 1:
        time.sleep(0.05)
got = []
def pipeline():
    b.s.sendall(query("INSERT INTO c () VALUES ()") * N)
    for _ in range(N):
        got.append(b.answer())
safe(pipeline)
got += ["MISS"] * (N - len(got))
if phase == "record":
    # Until the recording holds the pipeline's N mocks: the waiter, answered
    # only after the release below, is then written after them.
    while t.is_alive() and recorded("INSERT INTO c") < N:
        time.sleep(0.05)
released = safe(lambda: b.ask("SELECT RELEASE_LOCK('pipe')"))
t.join()
print("PIPE=" + ",".join(got), flush=True)
print("RELEASED=" + released, flush=True)
print("LOCK=" + lock.get("v", "MISS"), flush=True)
PY

echo "== 1. record a pipeline of $N INSERTs in one write, the lock's waiter answered after them =="
docker_pull_retry "$MYSQL_IMAGE"
MYSQL_CID=$(docker run -d --init -p 127.0.0.1:$PORT:3306 -e MYSQL_ALLOW_EMPTY_PASSWORD=yes \
  -e MYSQL_DATABASE=pipe "$MYSQL_IMAGE") || { echo "FAIL: cannot start $MYSQL_IMAGE"; exit 1; }
# Ready once it answers on its TCP port (the entrypoint's first server is
# socket-only).
until docker exec "$MYSQL_CID" mysql -h127.0.0.1 -uroot -e 'SELECT 1' pipe >/dev/null 2>&1; do
  [ "$(docker inspect -f '{{.State.Running}}' "$MYSQL_CID" 2>/dev/null)" = true ] ||
    { echo "FAIL: MySQL exited before it was ready"; docker logs "$MYSQL_CID" 2>&1 | tail -30; exit 1; }
  sleep 1
done

sudo env PATH="$PATH" "${SUDO_ENV[@]}" KEPLOY_PROVEN= "$RECORD_BIN" mock record -c "python3 pipe.py $PORT $N record $MYSQL_CID" --name pipe --disable-tele 2>&1 | tee rec.log
RC=${PIPESTATUS[0]}
stop_mysql
[ "$RC" -eq 0 ] || { echo "FAIL: the record should pass (exit 0), got $RC"; FAIL=1; }
for want in "HELD=1" "PIPE=$WANT" "RELEASED=1" "LOCK=1"; do
  grep -qx "$want" rec.log || { echo "FAIL: on record, want $want, got: $(grep -oE '^(HELD|PIPE|RELEASED|LOCK)=.*' rec.log | tr '\n' ' ')"; FAIL=1; }
done

# What the replay stands on, from the mock file: the pipeline's INSERTs share
# one request time, and the waiter, read before them, is written after them.
python3 - keploy/pipe/mocks.yaml "$N" <<'PY' || FAIL=1
import datetime, re, sys
def nanos(t):
    m = re.match(r"(.*T\d\d:\d\d:\d\d)(?:\.(\d+))?(.*)$", t.strip("'\""))
    zone = "+00:00" if m.group(3) == "Z" else m.group(3)
    whole = datetime.datetime.fromisoformat(m.group(1) + zone)
    return int(whole.timestamp()) * 10**9 + int((m.group(2) or "0").ljust(9, "0"))
try:
    # One YAML document a mock, its kind at the top level, matched whole.
    docs = [d for d in open(sys.argv[1]).read().split("\n---") if re.search(r"(?m)^kind: MySQL$", d)]
except OSError as e:
    sys.exit("FAIL: no mock file: %s" % e)
seen = []
for d in docs:
    # A MySQL mock's spec writes its request time as reqtimestampmock.
    q, at = re.search(r"\n\s*query: (.*)", d), re.search(r"(?i)reqtimestampmock: (\S+)", d)
    if q and at:
        seen.append((q.group(1), nanos(at.group(1))))
inserts = [(i, at) for i, (q, at) in enumerate(seen) if "INSERT INTO c" in q]
waiter = [(i, at) for i, (q, at) in enumerate(seen) if "GET_LOCK" in q and "120" in q]
if len(inserts) != int(sys.argv[2]) or len({at for _, at in inserts}) != 1:
    sys.exit("FAIL: want the pipeline's %s INSERTs recorded at one request time, got %d at %d times, of %d MySQL mocks with a query and a request time (%d MySQL mocks)"
             % (sys.argv[2], len(inserts), len({at for _, at in inserts}), len(seen), len(docs)))
if len(waiter) != 1 or not waiter[0][0] > inserts[-1][0] or not waiter[0][1] < inserts[0][1]:
    sys.exit("FAIL: want the waiter's GET_LOCK recorded before the pipeline and written after it, got %s"
             % [q for q, _ in seen])
print("mock file: %d INSERTs at one request time; the waiter, read before them, written after them" % len(inserts))
PY

echo "== 2. replay with MySQL DOWN, consume-once mocks: the INSERTs get 1..$N in turn =="
sudo env PATH="$PATH" "${SUDO_ENV[@]}" KEPLOY_PROVEN= KEPLOY_STRICT_MOCK_WINDOW=1 "$REPLAY_BIN" mock replay -c "python3 pipe.py $PORT $N replay" --name pipe --disable-tele 2>&1 | tee rep.log
RC=${PIPESTATUS[0]}
[ "$RC" -eq 0 ] || { echo "FAIL: the replay should pass (exit 0), got $RC"; FAIL=1; }
grep -qx "PIPE=$WANT" rep.log ||
  { echo "FAIL: the pipeline's INSERTs were not served 1..$N in turn (got: $(grep -o '^PIPE=.*' rep.log | head -1))"; FAIL=1; }
for want in "HELD=1" "RELEASED=1" "LOCK=1"; do
  grep -qx "$want" rep.log || { echo "FAIL: on replay, want $want, got: $(grep -oE '^(HELD|RELEASED|LOCK)=.*' rep.log | tr '\n' ' ')"; FAIL=1; }
done

[ "$FAIL" = 0 ] && echo "MOCK PIPELINE ORDER E2E: PASSED" || echo "MOCK PIPELINE ORDER E2E: FAILED"
exit "$FAIL"
