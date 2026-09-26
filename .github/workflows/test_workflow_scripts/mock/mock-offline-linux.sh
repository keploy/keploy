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
#            still reach both mocks
#   loopback loopback only: record a dependency on 127.0.0.1, stop it, and
#            replay it
#
# Env: RECORD_BIN (the keploy binary under test).
set -uo pipefail

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
# every other name. A query it cannot parse goes unanswered rather than
# stopping it.
cat > "$WORK/dnsserver.py" <<'PY'
import socket, struct, sys
ip = sys.argv[1]
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.bind((ip, 53))
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
PIDS=""
trap '[ -z "$PIDS" ] || kill $PIDS 2>/dev/null' EXIT
cd "$WORK" || exit 1
export HOME="$WORK/home"
mkdir -p "$HOME"
ip link set lo up || exit 1
case "$PHASE" in
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
case "$PHASE" in
  online|offline) SET=online ;;
  hosts-*) SET=hosts ;;
  loopback-*) SET=loopback ;;
esac
case "$PHASE" in
  online|hosts-record|loopback-record) SUB=record ;;
  *) SUB=replay ;;
esac
if [ "$PHASE" = online ] || [ "$PHASE" = hosts-record ]; then
  # A veth pair only to hold DEP_IP: the veth driver is there wherever Docker
  # is, which a dummy link's module is not on every runner kernel.
  ip link add kdep0 type veth peer name kdep1 || exit 1
  ip addr add "$DEP_IP/24" dev kdep0 || exit 1
  ip link set kdep0 up && ip link set kdep1 up || exit 1
  python3 depserver.py "$DEP_IP" "$PORT" &
  PIDS="$!"
fi
if [ "$PHASE" = online ]; then
  python3 dnsserver.py "$DEP_IP" &
  PIDS="$PIDS $!"
fi
if [ "$PHASE" = hosts-record ]; then
  { cat /etc/hosts; echo "$DEP_IP dep.keploy.test"; } > "$WORK/hosts"
  mount --bind "$WORK/hosts" /etc/hosts || exit 1
fi
if [ "$PHASE" = loopback-record ]; then
  python3 depserver.py 127.0.0.1 "$PORT" &
  PIDS="$!"
fi
ip -brief addr > "$WORK/$PHASE.addr"
# The dependency has to answer only where the phase has one: wait up to 20s
# for it to come up there, and ask once where there is none.
DEADLINE=$(($(date +%s) + 20))
while :; do
  python3 client.py "$URL" > /dev/null 2>&1
  DIRECT=$?
  [ "$DIRECT" = 0 ] || [ -z "$PIDS" ] || [ "$(date +%s)" -ge "$DEADLINE" ] && break
  sleep 0.2
done
echo "$DIRECT" > "$WORK/$PHASE.direct"
# Only the debug log says what keploy's DNS server answered a name it has no
# recorded answer for, which hosts-replay has to show.
DEBUG=""
[ "$PHASE" = hosts-replay ] && DEBUG="--debug"
timeout -s INT -k 30 90 "$BIN" mock "$SUB" -c "python3 client.py $URL" --name "$SET" --disable-tele $DEBUG > "$WORK/$PHASE.log" 2>&1
echo $? > "$WORK/$PHASE.exit"
exit 0
SH
chmod +x "$WORK/inside.sh"

# phase <name> <direct: 0 = the dependency answers, 1 = it does not>
phase() {
  local name=$1 direct=$2 failed=$FAIL
  FAIL=0
  echo "--- $name ---"
  sudo env PATH="$PATH" WORK="$WORK" BIN="$BIN" PORT="$PORT" DEP_IP="$DEP_IP" \
    unshare -n -m --propagation private "$WORK/inside.sh" "$name" || fail "$name: the namespace itself failed"
  cat "$WORK/$name.addr" 2>/dev/null
  [ "$(cat "$WORK/$name.direct" 2>/dev/null)" = "$direct" ] ||
    fail "$name: the test command without keploy exited $(cat "$WORK/$name.direct" 2>/dev/null), want $direct"
  local got
  got="$(cat "$WORK/$name.exit" 2>/dev/null)"
  [ "$got" = 0 ] || fail "$name: keploy exited ${got:-nothing}, want 0"
  grep -c "^ok /" "$WORK/$name.log" | grep -qx 2 || fail "$name: the test command did not get both answers"
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

phase online 0
[ "$(grep -c '^kind: DNS' "$WORK/keploy/online/mocks.yaml" 2>/dev/null)" -ge 1 ] ||
  fail "online: no DNS lookup was recorded"
[ "$(grep -c '^kind: Http' "$WORK/keploy/online/mocks.yaml" 2>/dev/null)" -eq 2 ] ||
  fail "online: want 2 HTTP mocks"
phase offline 1
proven_offline online

phase hosts-record 0
[ "$(grep -c '^kind: DNS' "$WORK/keploy/hosts/mocks.yaml" 2>/dev/null)" -eq 0 ] ||
  fail "hosts-record: a DNS lookup was recorded for a name /etc/hosts answers"
[ "$(grep -c '^kind: Http' "$WORK/keploy/hosts/mocks.yaml" 2>/dev/null)" -eq 2 ] ||
  fail "hosts-record: want 2 HTTP mocks"
phase hosts-replay 1
grep -aq 'synthesized A fallback.*"query": "dep.keploy.test.".*"proxy_ip4": "127.0.0.1"' "$WORK/hosts-replay.log" ||
  fail "hosts-replay: keploy's DNS server did not answer dep.keploy.test with loopback"
# Not proven_offline: the lookups have no recorded answer, so the receipt
# counts them as misses and the set as not isolated. What this phase can
# prove is that the calls made at the answer reached both recorded mocks.
grep -qx 'consumed: 2' "$WORK/keploy/hosts/last-replay.yaml" 2>/dev/null ||
  fail "hosts-replay: the calls did not reach both recorded mocks"

phase loopback-record 0
[ "$(grep -c '^kind: Http' "$WORK/keploy/loopback/mocks.yaml" 2>/dev/null)" -eq 2 ] ||
  fail "loopback-record: want 2 HTTP mocks"
phase loopback-replay 1
proven_offline loopback

if [ "$FAIL" -ne 0 ]; then
  echo "mock offline e2e FAILED; logs, mocks and receipts are kept in $WORK"
  exit 1
fi
sudo rm -rf "$WORK"
echo "mock offline e2e passed"
