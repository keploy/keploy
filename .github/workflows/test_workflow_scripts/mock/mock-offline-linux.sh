#!/usr/bin/env bash
# E2E: `keploy mock record|replay` on a machine whose network is off.
#
# Replaying recorded mocks is exactly what has to work with no network: a
# laptop on a plane, Wi-Fi off, the dependencies unreachable. A native agent
# once looked up a non-loopback IPv4 address it had no need for, so on a
# machine with only loopback it refused to start and every `keploy mock`
# command exited 6 before the test command ran.
#
# Each phase runs as root in a network namespace of its own (unshare -n), so
# the runner's own network stays out of it, and a mount namespace of its own
# (unshare -m) for the resolv.conf and nsswitch.conf it names:
#   online   a non-loopback address, with the dependency and a DNS server
#            for dep.keploy.test on it: record the calls made by name
#   offline  loopback only, the dependency and its DNS server gone: replay
#            them, answering the DNS lookups as well as the HTTP calls
#   hosts    as online, but the name resolves from /etc/hosts, so no DNS
#            lookup is recorded; replayed offline, where the name does not
#            resolve, keploy's DNS server has no recorded answer and answers
#            with where the proxy is reached: loopback, which only its debug
#            log can show, as the kernel sends a native application's calls
#            to the proxy whatever address they are made at. The calls must
#            still reach both mocks. The agent's log names the question by
#            its labels' shape and tag, never the name (a name is traffic):
#            the replay's misses file names the lookup whole
#   The record phases that reach the dependency by its name (online,
#   hosts-record, and their *-proven runs) run at debug level too, and their
#   agent's whole log must not name it either: a recording names the DNS
#   question it records, and the calls it makes to the name.
#   loopback loopback only: record a dependency on 127.0.0.1, stop it, and
#            replay it
#
# Every record phase runs again under KEPLOY_PROVEN=1, which the flip makes
# the default (phases named *-proven, into sets of their own), and must record
# the same. A replay under the switch needs a build that judges each test
# (keploy enterprise): offline-proven checks this build refuses it before it
# runs the test command. The replays above run without the switch, today's
# default, which the flip deletes with the switch.
# MOCK_LANE_SWITCH (lane_switch.sh) picks the half a run runs; CI runs each in
# a job of its own.
#
# Env: RECORD_BIN (the keploy binary under test).
set -uo pipefail

# This lane's directory: lane_switch.sh; its parent: sudo_env.sh.
# shellcheck source-path=SCRIPTDIR
MOCK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# A lane that could not load it would skip both halves and pass: fail instead.
# shellcheck source=lane_switch.sh
if ! source "$MOCK_DIR/lane_switch.sh" || ! declare -F half >/dev/null; then
  echo "FAIL: could not load lane_switch.sh from $MOCK_DIR"
  exit 1
fi
# shellcheck source=../sudo_env.sh
if ! source "$MOCK_DIR/../sudo_env.sh" || ! declare -F verify_ran >/dev/null; then
  echo "FAIL: could not load sudo_env.sh from $MOCK_DIR/.."
  exit 1
fi
# http_mocks_are and http_mocks_only: what a record kept is the client's two
# calls, as Http mocks, and DNS mocks only where the phase looks the name up.
# shellcheck source=../mock-kinds.sh
if ! source "$MOCK_DIR/../mock-kinds.sh" || ! declare -F http_mocks_only >/dev/null; then
  echo "FAIL: could not load mock-kinds.sh from $MOCK_DIR/.."
  exit 1
fi

BIN="${RECORD_BIN:-keploy}"
BIN="$(command -v "$BIN" || echo "$BIN")"
WORK="$(mktemp -d)"
FAIL=0
PORT=18999
DEP_IP=10.203.0.1

fail() {
  echo "FAIL: $*"
  FAIL=1
}

# Answers A for dep.keploy.test with DEP_IP, an empty AAAA, NXDOMAIN for
# every other name. Says "ready" once it is bound. A query it cannot parse goes unanswered rather than
# stopping it.
cat > "$WORK/dnsserver.py" <<'PY'
import socket, struct, sys
ip = sys.argv[1]
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.bind((ip, 53))
print("ready", flush=True)
def answer(q):
    tid = struct.unpack(">H", q[:2])[0]
    i, labels = 12, []
    while q[i]:
        labels.append(q[i + 1:i + 1 + q[i]].decode())
        i += 1 + q[i]
    qtype = struct.unpack(">H", q[i + 1:i + 3])[0]
    question = q[12:i + 5]
    if ".".join(labels).lower() != "dep.keploy.test":
        return struct.pack(">HHHHHH", tid, 0x8183, 1, 0, 0, 0) + question
    if qtype != 1:
        return struct.pack(">HHHHHH", tid, 0x8180, 1, 0, 0, 0) + question
    return (struct.pack(">HHHHHH", tid, 0x8180, 1, 1, 0, 0) + question +
            struct.pack(">HHHIH4s", 0xC00C, 1, 1, 60, 4, socket.inet_aton(ip)))
while True:
    q, addr = s.recvfrom(512)
    try:
        s.sendto(answer(q), addr)
    except (IndexError, struct.error, UnicodeDecodeError) as e:
        print("unanswered query:", e, file=sys.stderr)
PY

cat > "$WORK/depserver.py" <<'PY'
import http.server, json, socketserver, sys
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = json.dumps({"path": self.path}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a): pass
socketserver.TCPServer.allow_reuse_address = True
with socketserver.TCPServer((sys.argv[1], int(sys.argv[2])), H) as s:
    print("ready", flush=True)
    s.serve_forever()
PY

# The test command: calls the dependency at $1 and checks what came back.
cat > "$WORK/client.py" <<'PY'
import json, sys, urllib.request
for p in ("/users?id=1", "/items/42"):
    with urllib.request.urlopen(sys.argv[1] + p, timeout=5) as r:
        got = json.load(r)["path"]
    assert got == p, (got, p)
    print("ok", p)
PY

# Runs inside each namespace as root: $1 is the phase. Leaves, per phase, its
# addresses, the test command's exit without keploy, and keploy's exit and log
# in $WORK/<phase>.*.
cat > "$WORK/inside.sh" <<'SH'
#!/bin/sh
PHASE=$1
# <phase>-proven: the phase under the proven switch, into a set of its own.
BASE=${PHASE%-proven}
PROVEN=""
[ "$BASE" != "$PHASE" ] && PROVEN=1
PIDS=""
trap '[ -z "$PIDS" ] || kill $PIDS 2>/dev/null' EXIT
# ready PID FILE: waits for the server PID to say "ready" in FILE, once it
# listens; a server that exits first fails the phase.
ready() {
  until grep -qx ready "$2" 2>/dev/null; do
    kill -0 "$1" 2>/dev/null || { echo "$PHASE: a server exited before it listened:" >&2; cat "$2" >&2; exit 1; }
    sleep 0.1
  done
}
cd "$WORK" || exit 1
export HOME="$WORK/home"
mkdir -p "$HOME"
ip link set lo up || exit 1
case "$BASE" in
  online|offline|hosts-*)
    URL="http://dep.keploy.test:$PORT"
    echo "nameserver $DEP_IP" > "$WORK/resolv.conf"
    mount --bind "$WORK/resolv.conf" /etc/resolv.conf || exit 1
    # Names resolve from /etc/hosts and that resolv.conf, whatever the
    # runner's image lists before them: systemd-resolved, say, answers over
    # a socket that reaches out of this namespace.
    { grep -v '^hosts:' /etc/nsswitch.conf 2>/dev/null; echo "hosts: files dns"; } > "$WORK/nsswitch.conf"
    mount --bind "$WORK/nsswitch.conf" /etc/nsswitch.conf || exit 1
    ;;
  loopback-*)
    URL="http://127.0.0.1:$PORT"
    ;;
esac
case "$BASE" in
  online|offline) SET=online ;;
  hosts-*) SET=hosts ;;
  loopback-*) SET=loopback ;;
esac
[ -n "$PROVEN" ] && SET="$SET-proven"
case "$BASE" in
  online|hosts-record|loopback-record) SUB=record ;;
  *) SUB=replay ;;
esac
if [ "$BASE" = online ] || [ "$BASE" = hosts-record ]; then
  # A veth pair only to hold DEP_IP: the veth driver is there wherever Docker
  # is, which a dummy link's module is not on every runner kernel.
  ip link add kdep0 type veth peer name kdep1 || exit 1
  ip addr add "$DEP_IP/24" dev kdep0 || exit 1
  ip link set kdep0 up && ip link set kdep1 up || exit 1
  python3 depserver.py "$DEP_IP" "$PORT" > "$WORK/$PHASE.dep.out" 2>&1 &
  PIDS="$!"
  ready "$!" "$WORK/$PHASE.dep.out"
fi
if [ "$BASE" = online ]; then
  python3 dnsserver.py "$DEP_IP" > "$WORK/$PHASE.dns.out" 2>&1 &
  PIDS="$PIDS $!"
  ready "$!" "$WORK/$PHASE.dns.out"
fi
if [ "$BASE" = hosts-record ]; then
  { cat /etc/hosts; echo "$DEP_IP dep.keploy.test"; } > "$WORK/hosts"
  mount --bind "$WORK/hosts" /etc/hosts || exit 1
fi
if [ "$BASE" = loopback-record ]; then
  python3 depserver.py 127.0.0.1 "$PORT" > "$WORK/$PHASE.dep.out" 2>&1 &
  PIDS="$!"
  ready "$!" "$WORK/$PHASE.dep.out"
fi
ip -brief addr > "$WORK/$PHASE.addr"
# The test command without keploy: it gets its answers only where the phase
# has a dependency (which listens by now).
python3 client.py "$URL" > /dev/null 2>&1
echo $? > "$WORK/$PHASE.direct"
# Only the debug log says what keploy's DNS server answered a name it has no
# recorded answer for, which hosts-replay has to show. The record phases that
# call the dependency by its name run at debug level too: their agent's whole
# log is checked for the name (hostcheck), at the level production runs it.
DEBUG=""
case "$PHASE" in
  hosts-replay|online|hosts-record|online-proven|hosts-record-proven) DEBUG="--debug" ;;
esac
KEPLOY_PROVEN="$PROVEN" "$BIN" mock "$SUB" -c "python3 client.py $URL" --name "$SET" --disable-tele $DEBUG > "$WORK/$PHASE.log" 2>&1
echo $? > "$WORK/$PHASE.exit"
exit 0
SH
chmod +x "$WORK/inside.sh"

# phase <name> <direct: 0 = the dependency answers, 1 = it does not>
phase() {
  local name=$1 direct=$2 failed=$FAIL
  FAIL=0
  echo "--- $name ---"
  sudo env PATH="$PATH" "${SUDO_ENV[@]}" WORK="$WORK" BIN="$BIN" PORT="$PORT" DEP_IP="$DEP_IP" \
    unshare -n -m --propagation private "$WORK/inside.sh" "$name" || fail "$name: the namespace itself failed"
  cat "$WORK/$name.addr" 2>/dev/null
  [ "$(cat "$WORK/$name.direct" 2>/dev/null)" = "$direct" ] ||
    fail "$name: the test command without keploy exited $(cat "$WORK/$name.direct" 2>/dev/null), want $direct"
  local got
  got="$(cat "$WORK/$name.exit" 2>/dev/null)"
  [ "$got" = 0 ] || fail "$name: keploy exited ${got:-nothing}, want 0"
  # A *-proven phase records under the switch (inside.sh).
  [ "${name%-proven}" = "$name" ] || verify_ran 1 "$WORK/$name.log" || FAIL=1
  grep -c "^ok /" "$WORK/$name.log" | grep -x 2 >/dev/null || fail "$name: the test command did not get both answers"
  if [ "$FAIL" -ne 0 ]; then
    echo "--- $name: the last 40 lines keploy logged ---"
    tail -n 40 "$WORK/$name.log" 2>/dev/null
  fi
  [ "$failed" -eq 0 ] || FAIL=1
}

# proven_offline <set>: the replay proved the tests ran with every dependency
# off.
proven_offline() {
  local r="$WORK/keploy/$1/last-replay.yaml"
  if ! grep -qx 'missed: 0' "$r" 2>/dev/null || ! grep -qx 'isolated: true' "$r" 2>/dev/null; then
    fail "$1: the offline replay did not prove the tests ran with every dependency off; its receipt:"
    cat "$r" 2>/dev/null
  fi
}

# No line of the agent's names the dependency: not its DNS lines, nor any
# other (an egress, proxy, ingress or miss line), each naming a host by its
# labels' shape and tag (a name is traffic). One field is exempt, by its key:
# the agent's config dump ("config has been initialised") quotes the user's
# own command line in config.command, which names the dependency as the user
# wrote it, and is no traffic. The dump must be there exactly once, so a
# change in its form fails the check instead of exempting more.
hostcheck() {
  python3 -I - "$1" dep.keploy.test <<'PY'
import json, re, sys
log, name = sys.argv[1], sys.argv[2]
text = re.sub(r"\x1b\[[0-9;]*m", "", open(log, encoding="utf-8", errors="replace").read())
leaks, dumps = [], 0
for line in text.splitlines():
    if "Keploy(agent):" not in line:
        continue
    parts = line.split("\t")
    if len(parts) >= 4 and parts[2] == "config has been initialised":
        dumps += 1
        try:
            fields = json.loads(parts[3])
            fields["config"].pop("command")
        except (ValueError, KeyError, TypeError) as e:
            leaks.append("the config dump has no config.command to exempt (%s): %s" % (e, line[:300]))
            continue
        if name in json.dumps(fields):
            leaks.append("the config dump names it outside config.command: " + line[:400])
        continue
    if name in line:
        leaks.append(line[:400])
if dumps != 1:
    leaks.append("want the agent's config dump exactly once, found %d" % dumps)
for l in leaks[:5]:
    print(l)
sys.exit(1 if leaks else 0)
PY
}
# record_hostcheck <phase>: a record phase's agent names the dependency
# nowhere in its log (hostcheck).
record_hostcheck() {
  local out
  if ! out=$(hostcheck "$WORK/$1.log"); then
    fail "$1: the recording agent's log names the dependency dep.keploy.test outside the user's command line: $out"
  fi
}

if half off; then
phase online 0
record_hostcheck online
[ "$(grep -c '^kind: DNS' "$WORK/keploy/online/mocks.yaml" 2>/dev/null)" -ge 1 ] ||
  fail "online: no DNS lookup was recorded"
MOCKS=$(http_mocks_are "$WORK/keploy/online/mocks.yaml" 2) ||
  fail "online: want 2 Http mocks and no mock but DNS beside them, got $MOCKS"
phase offline 1
proven_offline online

phase hosts-record 0
record_hostcheck hosts-record
[ "$(grep -c '^kind: DNS' "$WORK/keploy/hosts/mocks.yaml" 2>/dev/null)" -eq 0 ] ||
  fail "hosts-record: a DNS lookup was recorded for a name /etc/hosts answers"
MOCKS=$(http_mocks_only "$WORK/keploy/hosts/mocks.yaml" 2) ||
  fail "hosts-record: want 2 Http mocks and no other, got $MOCKS"
phase hosts-replay 1
# The answer, by its line: the question by its labels' shape and its tag
# under the replay's session, never its name, which the agent's log never
# holds; the lookup itself, whole, where the replay names each call no mock
# answered (keploy/<set>/last-replay-misses.yaml).
grep -aqE 'synthesized A fallback.*"query": "\{\}\.\{\}\.\{\}\. #[0-9a-f]{12}".*"proxy_ip4": "127\.0\.0\.1"' "$WORK/hosts-replay.log" ||
  fail "hosts-replay: keploy's DNS server did not say it answered the name, by its shape and tag, with loopback"
if ! out=$(hostcheck "$WORK/hosts-replay.log"); then
  fail "hosts-replay: the agent's log names the dependency dep.keploy.test outside the user's command line: $out"
fi
grep -aqE '^[ -]*call: A dep\.keploy\.test\.$' "$WORK/keploy/hosts/last-replay-misses.yaml" 2>/dev/null ||
  fail "hosts-replay: the replay's misses file does not name the lookup of dep.keploy.test: $(cat "$WORK/keploy/hosts/last-replay-misses.yaml" 2>/dev/null | head -20)"
# Not proven_offline: the lookups have no recorded answer, so the receipt
# counts them as misses and the set as not isolated. What this phase can
# prove is that the calls made at the answer reached both recorded mocks.
grep -qx 'consumed: 2' "$WORK/keploy/hosts/last-replay.yaml" 2>/dev/null ||
  fail "hosts-replay: the calls did not reach both recorded mocks"

phase loopback-record 0
MOCKS=$(http_mocks_only "$WORK/keploy/loopback/mocks.yaml" 2) ||
  fail "loopback-record: want 2 Http mocks and no other, got $MOCKS"
phase loopback-replay 1
proven_offline loopback
fi

if half on; then
# Under the proven switch: the same records.
phase online-proven 0
record_hostcheck online-proven
[ "$(grep -c '^kind: DNS' "$WORK/keploy/online-proven/mocks.yaml" 2>/dev/null)" -ge 1 ] ||
  fail "online-proven: no DNS lookup was recorded"
MOCKS=$(http_mocks_are "$WORK/keploy/online-proven/mocks.yaml" 2) ||
  fail "online-proven: want 2 Http mocks and no mock but DNS beside them, got $MOCKS"
phase hosts-record-proven 0
record_hostcheck hosts-record-proven
[ "$(grep -c '^kind: DNS' "$WORK/keploy/hosts-proven/mocks.yaml" 2>/dev/null)" -eq 0 ] ||
  fail "hosts-record-proven: a DNS lookup was recorded for a name /etc/hosts answers"
MOCKS=$(http_mocks_only "$WORK/keploy/hosts-proven/mocks.yaml" 2) ||
  fail "hosts-record-proven: want 2 Http mocks and no other, got $MOCKS"
phase loopback-record-proven 0
MOCKS=$(http_mocks_only "$WORK/keploy/loopback-proven/mocks.yaml" 2) ||
  fail "loopback-record-proven: want 2 Http mocks and no other, got $MOCKS"

# ...and this build refuses the replay under it, offline, before the test
# command runs.
echo "--- offline-proven ---"
sudo env PATH="$PATH" "${SUDO_ENV[@]}" WORK="$WORK" BIN="$BIN" PORT="$PORT" DEP_IP="$DEP_IP" \
  unshare -n -m --propagation private "$WORK/inside.sh" offline-proven || fail "offline-proven: the namespace itself failed"
[ "$(cat "$WORK/offline-proven.exit" 2>/dev/null)" = 1 ] ||
  fail "offline-proven: keploy exited $(cat "$WORK/offline-proven.exit" 2>/dev/null), want this build's refusal (1)"
grep -q "needs a keploy build that judges each test" "$WORK/offline-proven.log" ||
  fail "offline-proven: keploy did not say it needs a build that judges each test"
grep -q "^ok /" "$WORK/offline-proven.log" && fail "offline-proven: the refused replay ran the test command"
fi

if [ "$FAIL" -ne 0 ]; then
  echo "mock offline e2e FAILED; logs, mocks and receipts are kept in $WORK"
  exit 1
fi
sudo rm -rf "$WORK"
echo "mock offline e2e passed"
