#!/usr/bin/env bash
# Tests for free-port.sh. The last case needs Docker. Run by
# linux-ci-scripts.yml, or locally (it leaves nothing behind):
#
#   bash .github/workflows/test_workflow_scripts/free-port.tests.sh
#
# window: every pick is a port of 28000-32767 that the host lets bind.
# order: the window's ports are tried in a random order, so that lanes side
#   by side on one host rarely pick the same one: over a two-port window,
#   picks land on both ports.
# busy: a port another process listens on is never picked, whether it holds
#   it on 0.0.0.0 or on [::] alone (as Docker publishes on both); with every
#   port of the window held, free_port fails and says so.
# skip: a port given as an argument is never picked.
# ephemeral: a window that reaches into the ephemeral range is cut below it,
#   and one that lies wholly in it has no port, which free_port says.
# host-range: on a host whose own ephemeral range is not the default
#   (FREE_PORT_RANGE_FILE stands for it), the window is cut below both that
#   range and a new network namespace's, which starts at 32768 whatever the
#   host's: '40000 60999' gives picks of at most 32767, '30000 60999' of at
#   most 29999.
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

# hold [-6] PORT...: a process listens on each PORT, on 0.0.0.0 or, with
# -6, on [::] alone (IPV6_V6ONLY), until release; hold returns once it does.
hold() {
  local family=4
  [ "$1" != -6 ] || { family=6; shift; }
  coproc HOLDER {
    python3 -c 'import socket, sys
held = []
for p in sys.argv[2:]:
    if sys.argv[1] == "6":
        s = socket.socket(socket.AF_INET6)
        s.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 1)
        s.bind(("::", int(p)))
    else:
        s = socket.socket()
        s.bind(("0.0.0.0", int(p)))
    s.listen()
    held.append(s)
print("held", flush=True)
sys.stdin.read()' "$family" "$@"
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

echo "== order =="
seen=""
for _ in $(seq 1 40); do
  p=$(FREE_PORT_LO=$lo FREE_PORT_HI=$hi free_port) || { fail "order: free_port failed in $lo-$hi"; break; }
  seen="$seen $p"
done
n=$(tr ' ' '\n' <<<"$seen" | grep -c . )
distinct=$(tr ' ' '\n' <<<"$seen" | grep . | sort -u | wc -l)
# Both ports free, tried in a random order: 40 picks all on one port happen
# with a chance of 2 in 2^40.
[ "$n" -eq 40 ] && [ "$distinct" -eq 2 ] ||
  fail "order: 40 picks in $lo-$hi gave $distinct distinct port(s), want both:$seen"

echo "== busy =="
hold "$lo"
for _ in $(seq 1 20); do
  p=$(FREE_PORT_LO=$lo FREE_PORT_HI=$hi free_port)
  [ "$p" = "$hi" ] || fail "busy: picked '$p' in $lo-$hi with $lo held, want $hi"
done
release
# A port held on [::] alone still binds on 0.0.0.0: only free_port's [::]
# check can turn it down. A host with no IPv6 cannot hold one, nor can
# Docker publish on it there.
if python3 -c 'import socket; socket.socket(socket.AF_INET6).bind(("::", 0))' 2>/dev/null; then
  hold -6 "$lo"
  bindable "$lo" || { echo "busy: $lo, held on [::] alone, does not bind on 0.0.0.0"; exit 2; }
  for _ in $(seq 1 20); do
    p=$(FREE_PORT_LO=$lo FREE_PORT_HI=$hi free_port)
    [ "$p" = "$hi" ] || fail "busy: picked '$p' in $lo-$hi with $lo held on [::] alone, want $hi"
  done
  release
else
  echo "busy: this host has no IPv6; the [::] case does not apply"
fi
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
if p=$(FREE_PORT_LO=32768 FREE_PORT_HI=40000 free_port 2>"$TMP/err"); then
  fail "ephemeral: picked $p from a window wholly in the ephemeral range"
fi
grep -q "has no port below the ephemeral ranges" "$TMP/err" ||
  fail "ephemeral: did not say the window 32768-40000 has no port below the ephemeral ranges: $(cat "$TMP/err")"

echo "== host-range =="
# FREE_PORT_RANGE_FILE RANGE LO HI MAX: 20 picks from LO-HI, on a host whose
# ephemeral range is RANGE, are each a port of LO-MAX.
host_range() {
  local range=$1 wlo=$2 whi=$3 max=$4 p
  echo "$range" >"$TMP/range"
  for _ in $(seq 1 20); do
    p=$(FREE_PORT_RANGE_FILE="$TMP/range" FREE_PORT_LO=$wlo FREE_PORT_HI=$whi free_port) ||
      { fail "host-range: free_port failed in $wlo-$whi on a host whose range is $range"; return; }
    if [ "$p" -lt "$wlo" ] || [ "$p" -gt "$max" ]; then
      fail "host-range: picked $p in $wlo-$whi on a host whose range is $range, want $wlo-$max"
    fi
  done
}
# A host range above 32768 leaves the namespace's start, 32768, as the cut.
host_range "40000 60999" 28000 39999 32767
# A host range below 32768 is the cut.
host_range "30000 60999" 28000 32767 29999

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
