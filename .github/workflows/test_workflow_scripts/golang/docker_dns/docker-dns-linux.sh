#!/usr/bin/env bash
# E2E: keploy record -c "docker run --dns ..." starts the app with the app's
# own resolver settings.
#
# Usage: docker-dns-linux.sh   (env: RECORD_BIN; needs the build's agent
# image loaded, ghcr.io/keploy/keploy:v3-dev, and busybox from the registry)
#
# Under keploy the app container joins the agent container's network
# namespace (--network=container:<agent>). Docker refuses an app's own --dns
# there ("conflicting options: dns and the network mode", exit 125), and
# ignores its --dns-search and --dns-option: the app shares the agent
# container's /etc/resolv.conf. Until the fix, keploy record failed at the
# app's start. keploy now starts the agent container with them.
#
# One keploy record session of a busybox httpd serving the /etc/resolv.conf
# it sees, its settings in both spellings docker takes (--flag value,
# --flag=value): the app must answer, its resolv.conf must hold them, and
# keploy must record the call (the app ran under keploy's network setup).
# The name server is a documentation address (RFC 5737) nothing is asked:
# the app resolves nothing. Telemetry is off, so keploy asks it nothing
# either. Readiness is the app's answer. The script stops only the processes
# it started. Port: 8099 (app).
set -uo pipefail
RECORD_BIN="${RECORD_BIN:-keploy}"
APP=8099
NS=192.0.2.53
SEARCH=keploy-e2e.invalid
FAIL=0
fail() {
    echo "::error::docker --dns: $*"
    FAIL=$((FAIL + 1))
}

WORK="$(mktemp -d)"
echo "work dir: $WORK"
cd "$WORK" || exit 1
# httpd is given no -p of its own (httpd -p sets its port): keploy takes
# every -p in a docker run command for a published port (ExtractDockerFlags),
# so httpd listens on its default port 80, which -p $APP:80 publishes.
APP_CMD="docker run --dns $NS --dns-search=$SEARCH --dns-option ndots:3 --dns-option=timeout:1 -p $APP:80 --rm --name docker-dns-app busybox:1.36 sh -c 'mkdir /www && cp /etc/resolv.conf /www/resolv && exec httpd -f -h /www'"

pid_running() {
    local st
    case $1 in "" | *[!0-9]*) return 1 ;; esac
    { read -r st <"/proc/$1/stat"; } 2>/dev/null || return 1
    st=${st##*) }
    [ "${st%% *}" != Z ]
}
REC_PID=""
cleanup() {
    local child
    if [ -n "$REC_PID" ]; then
        child="$(pgrep -P "$REC_PID" 2>/dev/null | head -n1)"
        [ -n "$child" ] && sudo kill -INT "$child" 2>/dev/null
        wait "$REC_PID" 2>/dev/null
    fi
    docker rm -f docker-dns-app >/dev/null 2>&1
    sudo chown -R "$(id -u):$(id -g)" "$WORK" 2>/dev/null
    true
}
trap cleanup EXIT

docker pull -q busybox:1.36 >/dev/null || { echo "::error::busybox pull failed"; exit 1; }
sudo -E env -u KEPLOY_PROVEN PATH="$PATH" "$RECORD_BIN" record -c "$APP_CMD" --container-name docker-dns-app \
    --disable-tele --generateGithubActions=false >record.log 2>&1 </dev/null &
REC_PID=$!
resolv=""
until resolv="$(curl -sf --max-time 5 "http://127.0.0.1:$APP/resolv" 2>/dev/null)" && [ -n "$resolv" ]; do
    pid_running "$REC_PID" || { fail "the app did not answer under keploy record"; break; }
    sleep 0.5
done
echo "-- the app's /etc/resolv.conf:"
echo "$resolv"
if [ -n "$resolv" ]; then
    grep -qx "nameserver $NS" <<<"$resolv" || fail "the app's resolv.conf names no nameserver $NS"
    grep -qE "^search( .*)? $SEARCH( |$)" <<<"$resolv" || fail "the app's resolv.conf searches no $SEARCH"
    grep -qE '^options( .*)? ndots:3( |$)' <<<"$resolv" || fail "the app's resolv.conf has no option ndots:3"
    grep -qE '^options( .*)? timeout:1( |$)' <<<"$resolv" || fail "the app's resolv.conf has no option timeout:1"
fi
# The call was keploy's to record: the app ran in the agent container's
# network namespace, not on its own.
if [ -n "$resolv" ]; then
    until [ -n "$(find keploy -path '*/tests/*.yaml' -size +0c 2>/dev/null | head -n1)" ]; do
        pid_running "$REC_PID" || { fail "keploy record wrote no test case of the app's call"; break; }
        sleep 0.5
    done
fi
if grep -a -q 'conflicting options' record.log; then
    fail "docker refused the app's settings under keploy's network mode:"
    grep -a -m3 'conflicting options' record.log
fi
[ "$FAIL" = 0 ] || { echo "-- keploy record's log (last lines):"; tail -n 30 record.log; }
echo "docker --dns: $FAIL failure(s)"
[ "$FAIL" = 0 ]
