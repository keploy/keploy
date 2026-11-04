#!/usr/bin/env bash
# E2E: two `keploy record -c "docker run ..."` runs on one Docker host at once
# each keep their own CA.
#
# Usage: docker-run-two-runs-linux.sh   (env: RECORD_BIN, LANE_WORK; needs the
# build's agent image loaded, ghcr.io/keploy/keploy:v3-dev, which is also the
# apps' image: it has sh, curl and sha256sum; and python3 and openssl on the
# host)
#
# A docker-run run's agent writes its CA bundle to a named volume its app
# mounts at /tmp/keploy-tls, and the app trusts the CA it finds there. Every
# run used one volume, keploy-tls-certs: a second run on the host wrote its
# agent's CA over the first's while the first ran, and the first run's app
# then refused its own agent's certificates (curl: unable to get local issuer
# certificate), so its HTTPS calls failed and went unrecorded.
#
# Ordered by events, not a clock: run A's app calls an HTTPS server once; run
# B starts and its app calls once, so B's agent has written its CA; then A's
# app calls again. Every call must verify the certificate its own agent
# presents with the CA in its own volume, and A's CA must not change under it.
# Each app must mount its own agent's volume, and the two agents two volumes.
# Before the runs the script makes an earlier run's leftover volume
# (keploy-tls-certs-<16 hex digits>), a compose project's
# (<project>_keploy-tls-certs) and an older keploy's (keploy-tls-certs): run
# A's start must remove the first and keep the other two. Once both runs end
# on Ctrl+C no run's volume is left.
#
# The HTTPS server is local: python3 on the host, with a self-signed
# certificate (the agent does not check an upstream's certificate unless
# asked). The apps reach it at the default bridge's gateway under a name
# their curl resolves to it (--resolve), so the agent makes its certificate
# for that name. Every wait is for a line in a log or a file, and ends with a
# failure when the process that would write it has ended. The script stops
# only the processes it started. Port: 8443 (the HTTPS server).

# keploy runs under sudo, which hands it only the variables its command line
# names (sudo-rs, Ubuntu 26.04's sudo, ignores -E): SUDO_ENV names the
# switches a workflow sets for keploy, each only while it is set.
# shellcheck source=sudo_env.sh
source "$(dirname "${BASH_SOURCE[0]}")/sudo_env.sh" || exit 1

set -uo pipefail
RECORD_BIN="${RECORD_BIN:-keploy}"
IMG=ghcr.io/keploy/keploy:v3-dev
UP_PORT=8443
UP_HOST=upstream.keploy-e2e.invalid
VOL=keploy-tls-certs
LEFTOVER="$VOL-0badc0de0badc0de"
KEPT=("shop_$VOL" "$VOL")
FAIL=0
fail() {
    echo "::error::two docker runs: $*"
    FAIL=$((FAIL + 1))
}
ok() { echo "ok: $*"; }

# LANE_WORK: where the run keeps its logs (CI uploads it).
WORK="${LANE_WORK:-$(mktemp -d)}"
mkdir -p "$WORK/runA" "$WORK/runB" "$WORK/sync" || exit 1
echo "work dir: $WORK"

pid_running() {
    local st
    case $1 in "" | *[!0-9]*) return 1 ;; esac
    { read -r st <"/proc/$1/stat"; } 2>/dev/null || return 1
    st=${st##*) }
    [ "${st%% *}" != Z ]
}
# stop_run PID: Ctrl+C to the keploy that sudo (PID) runs: to sudo's child
# (sudo does not pass on a signal sent from its command's process group,
# which is this script's when sudo makes no terminal), else to PID.
stop_run() {
    local child
    pid_running "$1" || return 0
    child="$(pgrep -P "$1" 2>/dev/null | head -n1)"
    sudo kill -INT "${child:-$1}" 2>/dev/null
}
# waitlog FILE PATTERN PID: wait for PATTERN in FILE; fails once PID has ended
# without it.
waitlog() {
    until grep -a -q "$2" "$1" 2>/dev/null; do
        pid_running "$3" || grep -a -q "$2" "$1" 2>/dev/null || return 1
        sleep 0.5
    done
}
REC_A="" REC_B="" UP_PID=""
cleanup() {
    stop_run "$REC_A"
    stop_run "$REC_B"
    [ -n "$REC_A" ] && wait "$REC_A" 2>/dev/null
    [ -n "$REC_B" ] && wait "$REC_B" 2>/dev/null
    docker rm -f two-runs-app-A two-runs-app-B >/dev/null 2>&1
    docker volume rm "$LEFTOVER" "${KEPT[@]}" >/dev/null 2>&1
    [ -n "$UP_PID" ] && kill "$UP_PID" 2>/dev/null
    sudo chown -R "$(id -u):$(id -g)" "$WORK" 2>/dev/null
    true
}
trap cleanup EXIT

# The HTTPS server.
GW="$(docker network inspect bridge -f '{{range .IPAM.Config}}{{.Gateway}}{{end}}')"
[ -n "$GW" ] || { echo "::error::no gateway on docker's default bridge"; exit 1; }
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj "/CN=$UP_HOST" \
    -keyout "$WORK/upstream.key" -out "$WORK/upstream.crt" >/dev/null 2>&1 ||
    { echo "::error::openssl could not make the server's certificate"; exit 1; }
python3 -c '
import http.server, ssl, sys
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Length", "2")
        self.end_headers()
        self.wfile.write(b"ok")
s = http.server.ThreadingHTTPServer(("0.0.0.0", int(sys.argv[1])), H)
c = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
c.load_cert_chain(sys.argv[2], sys.argv[3])
s.socket = c.wrap_socket(s.socket, server_side=True)
s.serve_forever()
' "$UP_PORT" "$WORK/upstream.crt" "$WORK/upstream.key" >"$WORK/upstream.log" 2>&1 &
UP_PID=$!
until [ "$(curl -sk -o /dev/null -w '%{http_code}' "https://127.0.0.1:$UP_PORT/")" = 200 ]; do
    pid_running "$UP_PID" || { echo "::error::the HTTPS server ended:"; cat "$WORK/upstream.log"; exit 1; }
    sleep 0.2
done

for v in "$LEFTOVER" "${KEPT[@]}"; do
    docker volume create "$v" >/dev/null || { echo "::error::could not make volume $v"; exit 1; }
done

# The app: wait for its agent's CA (ca.crt is written whole before
# keploy-ca.crt), call once; A then waits for B's call and calls again. Each
# call prints the CA's digest and curl's status (or its error). It then
# stays up until keploy stops it.
APP='trap "exit 0" INT TERM; f=/tmp/keploy-tls/ca.crt;
until [ -s /tmp/keploy-tls/keploy-ca.crt ]; do sleep 0.2; done;
call() { s=$(sha256sum $f | cut -c1-12); c=$(curl -sS -o /dev/null -w "%{http_code}" --resolve '"$UP_HOST:$UP_PORT:$GW"' --cacert $f https://'"$UP_HOST:$UP_PORT"'/ 2>&1); echo "APP_${NAME}_$1 ca=$s call=$c"; };
call 1; touch /sync/${NAME}1;
if [ "$NAME" = A ]; then until [ -e /sync/B1 ]; do sleep 0.2; done; call 2; fi;
sleep infinity & wait'
# run NAME: start run NAME's keploy record in the background ($! is its
# sudo's pid).
run() {
    (cd "$WORK/run$1" && exec sudo env -u KEPLOY_PROVEN PATH="$PATH" "${SUDO_ENV[@]}" "$RECORD_BIN" record \
        -c "docker run --rm --name two-runs-app-$1 -v $WORK/sync:/sync -e NAME=$1 --entrypoint /bin/sh $IMG -c '$APP'" \
        --container-name "two-runs-app-$1" --path "$WORK/run$1" --disable-tele --generateGithubActions=false --debug \
        >"$WORK/run$1.log" 2>&1 </dev/null) &
}
# app_line RUN_N: the line run RUN's app printed for its call N.
app_line() { grep -a -h -o "APP_$1 ca=[0-9a-f]* call=.*" "$WORK/run${1%%_*}.log" | head -n1; }
# vol_at CONTAINER: the volume CONTAINER mounts at /tmp/keploy-tls.
vol_at() { docker inspect -f '{{range .Mounts}}{{if eq .Destination "/tmp/keploy-tls"}}{{.Name}}{{end}}{{end}}' "$1" 2>/dev/null; }

run A
REC_A=$!
waitlog "$WORK/runA.log" 'APP_A_1 ' "$REC_A" || fail "run A's app never made its first call"
if docker volume inspect "$LEFTOVER" >/dev/null 2>&1; then
    fail "run A's start left the volume an earlier run left ($LEFTOVER)"
else
    ok "run A's start removed the volume an earlier run left"
fi
for v in "${KEPT[@]}"; do
    if docker volume inspect "$v" >/dev/null 2>&1; then
        ok "volume $v kept"
    else
        fail "run A's start removed $v, which is no run's"
    fi
done
run B
REC_B=$!
waitlog "$WORK/runB.log" 'APP_B_1 ' "$REC_B" || fail "run B's app never made its first call"
waitlog "$WORK/runA.log" 'APP_A_2 ' "$REC_A" || fail "run A's app never made its second call"

declare -A AGENT_VOL
for n in A B; do
    app="two-runs-app-$n"
    agent="$(docker inspect -f '{{.HostConfig.NetworkMode}}' "$app" 2>/dev/null)"
    agent="${agent#container:}"
    av="$(vol_at "$agent")" apv="$(vol_at "$app")"
    AGENT_VOL[$n]="$av"
    echo "run $n: agent ${agent:0:12} mounts [$av], app mounts [$apv]"
    [[ "$av" =~ ^$VOL-[0-9a-f]{16}$ ]] || fail "run $n's agent mounts [$av] at /tmp/keploy-tls, not a run's own CA volume"
    [ -n "$av" ] && [ "$av" = "$apv" ] || fail "run $n's app mounts [$apv], not its agent's CA volume [$av]"
done
if [ -n "${AGENT_VOL[A]}" ] && [ "${AGENT_VOL[A]}" != "${AGENT_VOL[B]}" ]; then
    ok "the two agents mount two CA volumes"
else
    fail "the two agents do not mount two CA volumes: [${AGENT_VOL[A]}] and [${AGENT_VOL[B]}]"
fi

a1="$(app_line A_1)" b1="$(app_line B_1)" a2="$(app_line A_2)"
echo "A's first call: [$a1]; B's call: [$b1]; A's second call: [$a2]"
for c in "$a1" "$b1" "$a2"; do
    case "$c" in *' call=200') ;; *) fail "a call did not verify its own agent's certificate: [$c]" ;; esac
done
ca() { sed -n 's/.* ca=\([0-9a-f]*\) .*/\1/p' <<<"$1"; }
if [ -n "$(ca "$a1")" ] && [ "$(ca "$a1")" = "$(ca "$a2")" ]; then
    ok "run A's CA did not change while run B ran"
else
    fail "run A's CA changed while run B ran: [$a1] then [$a2]"
fi

stop_run "$REC_A"
stop_run "$REC_B"
wait "$REC_A" 2>/dev/null
wait "$REC_B" 2>/dev/null
REC_A="" REC_B=""
left="$(docker volume ls -q | grep -E "^$VOL-[0-9a-f]{16}$")"
if [ -z "$left" ]; then
    ok "no run's CA volume is left once both runs end"
else
    fail "run CA volumes left once both runs ended: $left"
fi

if [ "$FAIL" != 0 ]; then
    for n in A B; do echo "-- run $n's keploy record log (last lines):"; tail -n 30 "$WORK/run$n.log"; done
fi
echo "two docker runs: $FAIL failure(s)"
[ "$FAIL" = 0 ]
