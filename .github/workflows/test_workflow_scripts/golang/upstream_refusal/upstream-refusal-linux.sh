#!/usr/bin/env bash
# Guard: under `keploy record`, an application connecting to a dependency that
# refuses, is unreachable or never answers gets the connect result it gets
# without keploy — not a connect that succeeds and then reads EOF.
#
# Runs in a network namespace of its own, so every case is fixed by this
# script rather than by the runner's network:
#   127.0.0.1:<unused>   nothing listens           -> ECONNREFUSED
#   198.18.0.2           on-link, ARP unanswered   -> EHOSTUNREACH
#   198.18.0.3           on-link, never answers    -> TIMEOUT
#   203.0.113.1, [2001:db8::1]  no route at all    -> ENETUNREACH
#   127.0.0.1:<http>     a server that accepts     -> connected
#
# and connected UDP, which keploy redirects only for DNS (port 53):
#   udp:127.0.0.1:<echo>  a UDP echo server        -> echoed
#   udp:203.0.113.1, udp:[2001:db8::1]  no route   -> ENETUNREACH
# with getaddrinfo's order for a name with an IPv4 and an IPv6 address: glibc
# UDP-connects to each to sort them, so the unroutable IPv6 one comes last
# (ORDER v4,v6), as it does without keploy; and a UDP socket that connect()s
# to the echo server after a connect() or sendto() to a nameserver's port 53
# names the echo server as its peer, not the nameserver keploy stored for it;
#
# and two connections whose destination must see exactly the one connection
# the application made — the connection opened while the handshake was held
# is the one the proxy uses:
#   https://localhost:18443   the TLS path dials the server name sent
#   [::ffff:127.0.0.1]:18081  an IPv6 socket to an IPv4 destination
#                             (v4-mapped, as a JVM connects)
#
# and a connection that sends nothing while its server closes it for being
# idle: the close must reach the application as it does without keploy;
# and a TLS connection used 2s after its handshake, to a server that closes
# connections whose handshake has not started within 1s: under keploy the
# handshake is with keploy, and the upstream connection opened at connect
# time must not take the application's connection down with it.
#
# Then once more with --disable-handshake-hold: a refused connect connects,
# as it did before handshakes were held.
#
# Needs: RECORD_BIN (the keploy binary under test), go (to build the probe),
# python3, openssl, curl, nft, iproute2, unshare, sudo.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$HERE/common.sh"
RECORD_BIN="${RECORD_BIN:?RECORD_BIN must name the keploy binary under test}"
WORK="$(mktemp -d)"
# Keep keploy's log for the artifact upload, whatever happens below.
trap 'cp "$WORK/record.log" "$HERE/record_logs.txt" 2>/dev/null || true; sudo rm -rf "$WORK"' EXIT

(cd "$HERE/probe" && CGO_ENABLED=0 go build -o "$WORK/probe" .)
cp "$HERE/count_server.py" "$HERE/idle_client.py" "$HERE/tls_idle_client.py" "$HERE/addr_order.py" \
  "$HERE/udp_reconnect.py" "$WORK/"
# The name addr_order.py looks up, bind-mounted over /etc/hosts in a mount
# namespace of its own: on-link IPv4, IPv6 with no route.
printf '198.18.0.4 dual.e2e.test\n2001:db8::1 dual.e2e.test\n' > "$WORK/hosts"
openssl req -x509 -newkey rsa:2048 -nodes -keyout "$WORK/key.pem" -out "$WORK/cert.pem" \
  -days 1 -subj /CN=localhost -addext subjectAltName=DNS:localhost >/dev/null 2>&1
# keploy reads its installation id from here instead of asking the network.
# (test-iid.sh's mkdir fails on a runner that already has the directory.)
set +e
source "$HERE/../../test-iid.sh"
set -e

cat > "$WORK/inside.sh" <<'INSIDE'
#!/usr/bin/env bash
set -euo pipefail
cd "$WORK"
ip link set lo up
ip link add e2e0 type veth peer name e2e1
ip addr add 198.18.0.1/24 dev e2e0
ip link set e2e0 up
ip link set e2e1 up
ip neigh add 198.18.0.3 lladdr 02:00:00:00:00:03 dev e2e0 nud permanent

python3 -m http.server 18080 --bind 127.0.0.1 >http.log 2>&1 &
HTTP_PID=$!
python3 count_server.py tls 18443 tls_conns.txt cert.pem key.pem >tls.log 2>&1 &
TLS_PID=$!
python3 count_server.py plain 18081 mapped_conns.txt >mapped.log 2>&1 &
MAPPED_PID=$!
python3 count_server.py idleclose 18083 idle_conns.txt >idle.log 2>&1 &
IDLE_PID=$!
python3 count_server.py tlsdeadline 18444 tlsidle_conns.txt cert.pem key.pem >tlsidle.log 2>&1 &
TLSIDLE_PID=$!
./probe -udpecho 127.0.0.1:18082 >udpecho.log 2>&1 &
UDPECHO_PID=$!
trap 'kill $HTTP_PID $TLS_PID $MAPPED_PID $IDLE_PID $TLSIDLE_PID $UDPECHO_PID 2>/dev/null || true' EXIT
for p in 18080 18443 18081 18083 18444; do
  for _ in $(seq 50); do
    python3 -c "import socket; socket.create_connection(('127.0.0.1', $p), 1)" 2>/dev/null && break
    sleep 0.1
  done
done
for _ in $(seq 50); do
  case "$(./probe udp:127.0.0.1:18082)" in *' echoed '*) break ;; esac
  sleep 0.1
done

DESTS="127.0.0.1:59999 198.18.0.2:80 198.18.0.3:80 203.0.113.1:80 [2001:db8::1]:80 127.0.0.1:18080"
DESTS="$DESTS udp:127.0.0.1:18082 udp:203.0.113.1:80 udp:[2001:db8::1]:80"
# shellcheck disable=SC2086
./probe $DESTS > baseline.txt
python3 idle_client.py 18083 > idle_baseline.txt
python3 tls_idle_client.py 18444 > tlsidle_baseline.txt
# shellcheck disable=SC2016
ORDER='mount --bind "$1" /etc/hosts && exec python3 "$2" dual.e2e.test'
unshare --mount sh -c "$ORDER" sh hosts addr_order.py > order_baseline.txt
python3 udp_reconnect.py 127.0.0.1 18082 > reconnect_baseline.txt
cat > app.sh <<APP
#!/bin/sh
../probe $DESTS
echo "HTTPS \$(curl -sk -m 20 -o /dev/null -w '%{http_code}' https://localhost:18443/)"
python3 -c "
import socket
s = socket.socket(socket.AF_INET6)
s.settimeout(20)
s.connect(('::ffff:127.0.0.1', 18081))
s.sendall(b'hello-from-a-v4-mapped-ipv6-client\\n')
print('V4MAPPED', s.recv(100).decode().strip())
"
python3 ../idle_client.py 18083
python3 ../tls_idle_client.py 18444
unshare --mount sh -c '$ORDER' sh ../hosts ../addr_order.py
python3 ../udp_reconnect.py 127.0.0.1 18082
APP
chmod +x app.sh
: > tls_conns.txt
: > mapped_conns.txt
mkdir rec && cd rec
# keploy exits non-zero when the application exits on its own; the probe's
# lines are the result.
"$RECORD_BIN" record -c "../app.sh" --disableANSI > ../record.log 2>&1 || true
cd ..
grep '^PROBE ' record.log > recorded.txt || true
grep '^HTTPS ' record.log > https.txt || true
grep '^V4MAPPED ' record.log > mapped.txt || true
grep '^IDLE ' record.log > idle_recorded.txt || true
grep '^TLSIDLE ' record.log > tlsidle_recorded.txt || true
grep '^ORDER ' record.log > order_recorded.txt || true
grep '^RECONNECT ' record.log > reconnect_recorded.txt || true

# The switch: with the hold turned off, a refused connect connects again.
mkdir rec_off && cd rec_off
"$RECORD_BIN" record -c "../probe 127.0.0.1:59999" --disable-handshake-hold --disableANSI > ../record_off.log 2>&1 || true
cd ..
grep '^PROBE ' record_off.log > recorded_off.txt || true
# Whatever keploy added to the namespace's ruleset is gone once it exits.
nft list tables > tables_after.txt
wc -l < tls_conns.txt > tls_conn_count.txt
wc -l < mapped_conns.txt > mapped_conn_count.txt
INSIDE
chmod +x "$WORK/inside.sh"

sudo -E env WORK="$WORK" RECORD_BIN="$RECORD_BIN" PATH="$PATH" unshare --net -- "$WORK/inside.sh"

cp "$WORK/record.log" "$HERE/record_logs.txt" 2>/dev/null || true
expect "$WORK/baseline.txt" 127.0.0.1:59999 ECONNREFUSED
expect "$WORK/baseline.txt" 198.18.0.2:80 EHOSTUNREACH
expect "$WORK/baseline.txt" 198.18.0.3:80 TIMEOUT
expect "$WORK/baseline.txt" 203.0.113.1:80 ENETUNREACH
expect "$WORK/baseline.txt" '[2001:db8::1]:80' ENETUNREACH
expect "$WORK/baseline.txt" 127.0.0.1:18080 connected
expect "$WORK/baseline.txt" udp:127.0.0.1:18082 echoed
expect "$WORK/baseline.txt" udp:203.0.113.1:80 ENETUNREACH
expect "$WORK/baseline.txt" 'udp:[2001:db8::1]:80' ENETUNREACH
fail=0
compare "$WORK/baseline.txt" "$WORK/recorded.txt" || fail=1
order_want=$(cut -d' ' -f2 "$WORK/order_baseline.txt")
order_got=$(cut -d' ' -f2 "$WORK/order_recorded.txt")
if [ "$order_want" != "v4,v6" ]; then
  echo "::error::fixture: without keploy getaddrinfo ordered dual.e2e.test '${order_want:-<none>}', expected v4,v6 (the IPv6 address has no route)"
  fail=1
elif [ "$order_got" != "$order_want" ]; then
  echo "::error::getaddrinfo ordered dual.e2e.test '${order_got:-<none>}' under keploy record, '$order_want' without: its UDP probe of the unroutable IPv6 address connected"
  fail=1
else
  echo "ok: getaddrinfo puts the unroutable IPv6 address last, as without keploy"
fi
# reconnects FILE: FILE's RECONNECT results, on one line.
reconnects() { awk '$1 == "RECONNECT" { $1 = ""; sub(/^ /, ""); printf "%s%s", sep, $0; sep = "; " }' "$1"; }
reconnect_want="connect peer=127.0.0.1:18082 echoed; sendto peer=127.0.0.1:18082 echoed"
reconnect_base=$(reconnects "$WORK/reconnect_baseline.txt")
reconnect_got=$(reconnects "$WORK/reconnect_recorded.txt")
if [ "$reconnect_base" != "$reconnect_want" ]; then
  echo "::error::fixture: without keploy the reconnected UDP socket gave '${reconnect_base:-<none>}', expected '$reconnect_want'"
  fail=1
elif [ "$reconnect_got" != "$reconnect_want" ]; then
  echo "::error::a UDP socket that connect()ed elsewhere after a DNS connect()/sendto() gave '${reconnect_got:-<none>}' under keploy record, '$reconnect_want' without: getpeername kept naming the nameserver keploy stored for the DNS exchange"
  fail=1
else
  echo "ok: a UDP socket that reconnects after a DNS exchange names its new peer, as without keploy"
fi
https=$(awk '{print $2}' "$WORK/https.txt")
conns=$(tr -d ' ' < "$WORK/tls_conn_count.txt")
if [ "$https" != "200" ]; then
  echo "::error::HTTPS request under keploy record returned '${https:-<none>}', want 200"
  fail=1
elif [ "$conns" != "1" ]; then
  echo "::error::the HTTPS server saw $conns connections for the application's one; the proxy dialled again instead of using the connection it opened while the handshake was held"
  fail=1
else
  echo "ok: HTTPS by name -> 200 over the application's one connection"
fi
mapped=$(cut -d' ' -f2- "$WORK/mapped.txt")
mconns=$(tr -d ' ' < "$WORK/mapped_conn_count.txt")
if [ "$mapped" != "ECHO:hello-from-a-v4-mapped-ipv6-client" ]; then
  echo "::error::v4-mapped IPv6 client under keploy record got '${mapped:-<none>}'"
  fail=1
elif [ "$mconns" != "1" ]; then
  echo "::error::the v4-mapped client's server saw $mconns connections for the application's one"
  fail=1
else
  echo "ok: v4-mapped IPv6 client -> echoed over the application's one connection"
fi
idle_want=$(cut -d' ' -f2- "$WORK/idle_baseline.txt")
idle_got=$(cut -d' ' -f2- "$WORK/idle_recorded.txt")
if [ "$idle_want" != "eof eof" ]; then
  echo "::error::fixture: without keploy the idle connections read '${idle_want:-<none>}', expected the server's close twice (eof eof)"
  fail=1
elif [ "$idle_got" != "$idle_want" ]; then
  echo "::error::a server's close of an idle connection reached the application as '${idle_got:-<none>}' under keploy record, '$idle_want' without"
  fail=1
else
  echo "ok: a server's close of an idle connection reaches the application, as without keploy"
fi
tls_want=$(cut -d' ' -f2 "$WORK/tlsidle_baseline.txt")
tls_got=$(cut -d' ' -f2 "$WORK/tlsidle_recorded.txt")
if [ "$tls_want" != "200" ]; then
  echo "::error::fixture: without keploy the late TLS request got '${tls_want:-<none>}', expected 200"
  fail=1
elif [ "$tls_got" != "$tls_want" ]; then
  echo "::error::a TLS request made 2s after its handshake got '${tls_got:-<none>}' under keploy record, '$tls_want' without"
  fail=1
else
  echo "ok: a TLS connection used after its upstream's handshake deadline still works, as without keploy"
fi
off=$(awk '$1 == "PROBE" { print $3 }' "$WORK/recorded_off.txt")
if [ "$off" != "connected" ]; then
  echo "::error::with --disable-handshake-hold a refused connect gave '${off:-<no result>}'; the switch must restore the old behaviour (connected)"
  fail=1
else
  echo "ok: --disable-handshake-hold turns the hold off"
fi
if ! grep -q "did not accept the application's connection" "$WORK/record.log" ||
   ! grep -q "the application gave up connecting to a dependency" "$WORK/record.log"; then
  echo "::error::keploy record did not log the refused and the unanswered dependency"
  fail=1
else
  echo "ok: keploy record logged the refused and the unanswered dependency"
fi
if grep -q 'keploy_synhold' "$WORK/tables_after.txt"; then
  echo "::error::keploy record left its nf_tables table behind:"
  cat "$WORK/tables_after.txt"
  fail=1
else
  echo "ok: keploy record removed its nf_tables table when it stopped"
fi
if grep -q 'ERROR' "$WORK/record.log"; then
  echo "::error::keploy record logged errors:"
  grep 'ERROR' "$WORK/record.log" | head -20
  fail=1
fi
# The amd64 PR build is race-enabled: a race in the hold is a failure even when the
# application's results look right.
if grep -q 'WARNING: DATA RACE' "$WORK/record.log"; then
  echo "::error::keploy record reported a data race"
  grep -A30 'WARNING: DATA RACE' "$WORK/record.log" | head -60
  fail=1
fi
exit $fail
