#!/usr/bin/env bash
# Guard: the docker leg of upstream-refusal-linux.sh. Under
# `keploy record -c "docker run ..."` the application runs in the keploy
# agent's network namespace and its connects are redirected to the agent's
# proxy; a dependency that refuses or is unreachable must still fail them the
# way it does for the same container run without keploy.
#
#   127.0.0.1:<unused>        nothing listens in the container -> ECONNREFUSED
#   <unused bridge address>   on the docker bridge, no such host -> EHOSTUNREACH
#   [2001:db8::1]             no IPv6 route in a default bridge -> ENETUNREACH
#   <server container>:9000   accepts and echoes                -> connected
#
# The server must see exactly one connection for the application's one, and
# keploy must log no errors.
#
# Needs: RECORD_BIN, the agent image loaded as ghcr.io/keploy/keploy:v3-dev,
# go, docker, sudo.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$HERE/common.sh"
RECORD_BIN="${RECORD_BIN:?RECORD_BIN must name the keploy binary under test}"
WORK="$(mktemp -d)"
IMAGE=keploy-e2e-refusal-probe:local
NAME=refusal-probe
SERVER=refusal-server
cleanup() {
  docker rm -f "$NAME" "$SERVER" >/dev/null 2>&1 || true
  sudo rm -rf "$WORK"
}
trap cleanup EXIT

(cd "$HERE/probe" && CGO_ENABLED=0 go build -o "$WORK/probe" .)
# The probe is static: no base image to pull.
cat > "$WORK/Dockerfile" <<'DOCKERFILE'
FROM scratch
COPY probe /probe
ENTRYPOINT ["/probe"]
DOCKERFILE
docker build -q -t "$IMAGE" "$WORK" >/dev/null

subnet=$(docker network inspect bridge -f '{{(index .IPAM.Config 0).Subnet}}')
# An address near the end of the bridge's subnet, which no container has.
unreach=$(python3 -c "import ipaddress,sys; print(ipaddress.ip_network(sys.argv[1]).broadcast_address - 5)" "$subnet")

mkdir -p "$WORK/srv" && chmod 777 "$WORK/srv"
# Created here, so the server (root in its container) appends to a file this
# script can still truncate.
: > "$WORK/srv/conns.txt" && chmod 666 "$WORK/srv/conns.txt"
docker run -d --name "$SERVER" -v "$WORK/srv:/out" "$IMAGE" -serve 9000 /out/conns.txt >/dev/null
server_ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$SERVER")
for _ in $(seq 50); do
  python3 -c "import socket,sys; socket.create_connection((sys.argv[1], 9000), 1)" "$server_ip" 2>/dev/null && break
  sleep 0.2
done
DESTS="127.0.0.1:59999 $unreach:80 [2001:db8::1]:80 $server_ip:9000"

# shellcheck disable=SC2086
docker run --rm -e PROBE_SEND=1 "$IMAGE" $DESTS > "$WORK/baseline.txt"
: > "$WORK/srv/conns.txt"

mkdir -p "$WORK/rec"
(cd "$WORK/rec" && sudo -E "$RECORD_BIN" record -c "docker run --rm --name $NAME -e PROBE_SEND=1 $IMAGE $DESTS" \
  --container-name "$NAME" --disableANSI > "$WORK/record.log" 2>&1) || true
grep '^PROBE ' "$WORK/record.log" > "$WORK/recorded.txt" || true
cp "$WORK/record.log" "$HERE/record_docker_logs.txt" 2>/dev/null || true

expect "$WORK/baseline.txt" 127.0.0.1:59999 ECONNREFUSED
expect "$WORK/baseline.txt" "$unreach:80" EHOSTUNREACH
expect "$WORK/baseline.txt" '[2001:db8::1]:80' ENETUNREACH
expect "$WORK/baseline.txt" "$server_ip:9000" connected
fail=0
compare "$WORK/baseline.txt" "$WORK/recorded.txt" || fail=1
reply=$(awk -v a="$server_ip:9000" '$1 == "REPLY" && $2 == a { print $3 }' "$WORK/record.log")
conns=$(wc -l < "$WORK/srv/conns.txt" | tr -d ' ')
if [ "$reply" != '"ECHO:hello-from-probe\n"' ]; then
  echo "::error::the connection to the server container got ${reply:-<no reply>} under keploy record"
  fail=1
elif [ "$conns" != "1" ]; then
  echo "::error::the server container saw $conns connections for the application's one"
  fail=1
else
  echo "ok: $server_ip:9000 -> echoed over the application's one connection"
fi
if grep -q 'ERROR' "$WORK/record.log"; then
  echo "::error::keploy record logged errors:"
  grep 'ERROR' "$WORK/record.log" | head -20
  fail=1
fi
if grep -q 'WARNING: DATA RACE' "$WORK/record.log"; then
  echo "::error::keploy record reported a data race"
  grep -A30 'WARNING: DATA RACE' "$WORK/record.log" | head -60
  fail=1
fi
exit $fail
