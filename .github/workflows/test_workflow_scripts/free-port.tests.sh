#!/usr/bin/env bash
# Tests for free-port.sh. The last case needs Docker. Run by
# linux-ci-scripts.yml, or locally (it leaves nothing behind):
#
#   bash .github/workflows/test_workflow_scripts/free-port.tests.sh
#
# window: every pick is a port of 28000-32767 that the host lets bind.
# busy: a port another process listens on is never picked; with every port of
#   the window held, free_port fails and says so.
# skip: a port given as an argument is never picked.
# ephemeral: a window that reaches into the ephemeral range is cut below it,
#   and one that lies wholly in it has no port.
# docker-dns: the collision free_port exists for, on the real thing. A
#   container on a user-defined network has Docker's DNS server listening on
#   127.0.0.11 at a TCP port the kernel picked in the container's own network
#   namespace. That port lies in the ephemeral range, and a bind of 0.0.0.0
#   on it fails with EADDRINUSE there (the control: an app given that port
#   cannot start, as in job 124142018005); a port free_port picked binds.
set -uo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=free-port.sh
source "$DIR/free-port.sh"
FAILS=0
TMP=$(mktemp -d)
NET=""
cleanup() {
  [ -z "$NET" ] || docker network rm "$NET" >/dev/null 2>&1
  rm -rf "$TMP"
}
trap cleanup EXIT
fail() {
  echo "FAIL: $*"
  FAILS=$((FAILS + 1))
}

# bindable PORT: whether this host lets a listener bind 0.0.0.0:PORT now.
bindable() {
  python3 -c 'import socket, sys
s = socket.socket()
try:
    s.bind(("0.0.0.0", int(sys.argv[1])))
except OSError:
    sys.exit(1)' "$1"
}

# hold PORT...: a process listens on each PORT until release; hold returns
# once it does.
hold() {
  coproc HOLDER {
    python3 -c 'import socket, sys
held = []
for p in sys.argv[1:]:
    s = socket.socket()
    s.bind(("0.0.0.0", int(p)))
    s.listen()
    held.append(s)
print("held", flush=True)
sys.stdin.read()' "$@"
  }
  local line=""
  read -r line <&"${HOLDER[0]}"
  [ "$line" = held ] || { echo "hold: could not listen on $*"; exit 2; }
}
release() {
  local pid=$HOLDER_PID fd=${HOLDER[1]}
  eval "exec $fd>&-"
  wait "$pid" 2>/dev/null
}

echo "== window =="
for _ in $(seq 1 100); do
  p=$(free_port) || { fail "window: free_port failed"; break; }
  if [ "$p" -lt 28000 ] || [ "$p" -gt 32767 ]; then
    fail "window: picked $p, outside 28000-32767"
  elif ! bindable "$p"; then
    fail "window: picked $p, which the host does not let bind"
  fi
done

# Two adjacent ports of the window that are free now.
lo=$(free_port) || { echo "no free port to test with"; exit 2; }
[ "$lo" -lt 32767 ] || lo=$((lo - 1))
hi=$((lo + 1))
if ! bindable "$lo" || ! bindable "$hi"; then
  echo "ports $lo-$hi are not both free"
  exit 2
fi

echo "== busy =="
hold "$lo"
for _ in $(seq 1 20); do
  p=$(FREE_PORT_LO=$lo FREE_PORT_HI=$hi free_port)
  [ "$p" = "$hi" ] || fail "busy: picked '$p' in $lo-$hi with $lo held, want $hi"
done
release
hold "$lo" "$hi"
if p=$(FREE_PORT_LO=$lo FREE_PORT_HI=$hi free_port 2>"$TMP/err"); then
  fail "busy: picked $p with every port of $lo-$hi held"
fi
grep -q "in use on this host" "$TMP/err" || fail "busy: did not say why it found no port: $(cat "$TMP/err")"
release

echo "== skip =="
for _ in $(seq 1 20); do
  p=$(FREE_PORT_LO=$lo FREE_PORT_HI=$hi free_port "$lo")
  [ "$p" = "$hi" ] || fail "skip: picked '$p' in $lo-$hi skipping $lo, want $hi"
done

echo "== ephemeral =="
host_lo=$(awk '{print $1}' /proc/sys/net/ipv4/ip_local_port_range)
top=$((host_lo - 1))
[ "$top" -le 32767 ] || top=32767
if bindable "$top"; then
  for _ in $(seq 1 20); do
    p=$(FREE_PORT_LO=$top FREE_PORT_HI=40000 free_port)
    [ "$p" = "$top" ] || fail "ephemeral: picked '$p' in $top-40000, want $top, the window's only port below the ephemeral ranges"
  done
fi
if p=$(FREE_PORT_LO=32768 FREE_PORT_HI=40000 free_port 2>/dev/null); then
  fail "ephemeral: picked $p from a window wholly in the ephemeral range"
fi

echo "== docker-dns =="
IMAGE=python:3.11-alpine
source "$DIR/docker-build-retry.sh"
docker_pull_retry "$IMAGE" >/dev/null || { echo "could not pull $IMAGE"; exit 2; }
docker network create "free-port-test-$$" >/dev/null || { echo "could not create a network"; exit 2; }
NET="free-port-test-$$"
p=$(free_port) || fail "docker-dns: free_port failed"
out=$(docker run --rm --network "$NET" "$IMAGE" python -c 'import errno, socket, sys
dns = [int(l.split()[1].split(":")[1], 16) for l in open("/proc/net/tcp").read().splitlines()[1:]
       if l.split()[1].startswith("0B00007F:") and l.split()[3] == "0A"]
print("dns", *dns)
def bind(port):
    s = socket.socket()
    s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    try:
        s.bind(("0.0.0.0", port))
        return "ok"
    except OSError as e:
        return errno.errorcode.get(e.errno, str(e.errno))
for port in dns:
    print("bind-dns", port, bind(port))
print("bind-picked", sys.argv[1], bind(int(sys.argv[1])))' "$p" 2>&1)
echo "$out"
dns=$(awk '$1 == "dns" {print $2}' <<<"$out")
if [ -z "$dns" ]; then
  fail "docker-dns: no TCP listener on 127.0.0.11 in a container on a user-defined network: $out"
else
  if [ "$dns" -lt 32768 ] || [ "$dns" -gt 60999 ]; then
    fail "docker-dns: Docker's DNS server listens on $dns, outside the ephemeral range: free_port's window may not be clear of it"
  fi
  grep -qx "bind-dns $dns EADDRINUSE" <<<"$out" ||
    fail "docker-dns: a bind of 0.0.0.0:$dns, Docker's DNS port, did not fail with EADDRINUSE: the collision is not reproduced"
fi
grep -qx "bind-picked $p ok" <<<"$out" || fail "docker-dns: the port free_port picked, $p, does not bind in the container"

if [ "$FAILS" -ne 0 ]; then
  echo "::error::$FAILS free-port.sh test(s) failed"
  exit 1
fi
echo "free-port.sh tests passed"
