#!/usr/bin/env bash
# E2E: keploy record --disable-tele resolves no telemetry host, natively and
# in Docker.
#
# Usage: disable-tele-linux.sh native|docker   (env: RECORD_BIN; docker mode
# needs the build's agent image loaded, ghcr.io/keploy/keploy:v3-dev)
#
# Until the fix, `keploy record --disable-tele` still looked up
# telemetry.keploy.io (A and AAAA) and posted to it: the agent is a keploy
# process of its own that pings telemetry at its start from its own config,
# and none of its launchers handed it the CLI's --disable-tele.
#
# A watcher (app/, dnswatch) logs every DNS query of this network namespace,
# the ones dockerd forwards for its containers included, without touching
# resolv.conf. Two legs, each one keploy record session of one GET /hello,
# judged on the same window: from keploy's start to its exit (the agent pings
# at its start, before it serves), closed by a marker query the watcher must
# log after every query captured before it.
#   1. --disable-tele: no query names telemetry.keploy.io.
#   2. Control, no flag: some query does, so the watcher sees the lookups the
#      flag leg is judged on. In Docker the CLI's own telemetry goes to
#      KEPLOY_TELEMETRY_URL (a local port; the agent container is not given
#      it), so only the agent container's lookup, forwarded by dockerd, can
#      count. This leg sends keploy's usual telemetry.
# Readiness is the app's answer, keploy's test file and the watcher's marker.
# The script stops only the processes it started. Port: 8098 (app).
set -uo pipefail
MODE="${1:-native}"
RECORD_BIN="${RECORD_BIN:-keploy}"
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP=8098
TELE_HOST="telemetry.keploy.io"
FAIL=0
fail() {
    echo "::error::disable-tele ($MODE): $*"
    FAIL=$((FAIL + 1))
}
case "$MODE" in native | docker) ;; *) echo "usage: $0 native|docker" >&2; exit 2 ;; esac

WORK="$(mktemp -d)"
echo "work dir: $WORK"
(cd "$SRC" && CGO_ENABLED=0 go build -o "$WORK/tele-app" ./app) || { echo "::error::build failed"; exit 1; }
cd "$WORK" || exit 1

if [ "$MODE" = docker ]; then
    mkdir -p img && cp tele-app img/ &&
        printf 'FROM scratch\nCOPY tele-app /tele-app\nENTRYPOINT ["/tele-app", "serve", ":%s"]\n' "$APP" >img/Dockerfile &&
        docker build -q -t keploy-disable-tele-app:local img >/dev/null || { echo "::error::app image build failed"; exit 1; }
    APP_CMD="docker run -p $APP:$APP --rm --name disable-tele-app keploy-disable-tele-app:local"
    MODE_FLAGS=(--container-name disable-tele-app)
    CONTROL_ENV=(KEPLOY_TELEMETRY_URL=http://127.0.0.1:9/analytics)
else
    APP_CMD="$WORK/tele-app serve 127.0.0.1:$APP"
    MODE_FLAGS=()
    CONTROL_ENV=()
fi

# pid_running PID: whether PID still runs, by its state in /proc, which shows
# root's processes to this user too (kill -0 fails on a sudo that is alive).
pid_running() {
    local st
    case $1 in "" | *[!0-9]*) return 1 ;; esac
    { read -r st <"/proc/$1/stat"; } 2>/dev/null || return 1
    st=${st##*) }
    [ "${st%% *}" != Z ]
}
test_files() { find keploy/test-set-0/tests -maxdepth 1 -name '*.yaml' -size +0c 2>/dev/null | wc -l; }
own() { sudo chown -R "$(id -u):$(id -g)" . 2>/dev/null || true; }
stop_pid() {
    local sudo_pid="$1" child
    child="$(pgrep -P "$sudo_pid" 2>/dev/null | head -n1)"
    [ -n "$child" ] && sudo kill -INT "$child" 2>/dev/null
    wait "$sudo_pid" 2>/dev/null
}

# The watcher, as root (AF_PACKET), for both legs.
sudo "$WORK/tele-app" dnswatch "$WORK/dns.log" 2>"$WORK/dnswatch.err" &
WATCH_SUDO=$!
REC_PID=""
cleanup() {
    local child
    [ -n "$REC_PID" ] && stop_pid "$REC_PID"
    child="$(pgrep -P "$WATCH_SUDO" 2>/dev/null | head -n1)"
    [ -n "$child" ] && sudo kill "$child" 2>/dev/null
    wait "$WATCH_SUDO" 2>/dev/null
    [ "$MODE" = docker ] && docker rm -f disable-tele-app >/dev/null 2>&1
    true
}
trap cleanup EXIT
until grep -q '^ready$' "$WORK/dnswatch.err" 2>/dev/null; do
    pid_running "$WATCH_SUDO" || { echo "::error::the DNS watcher did not start: $(cat "$WORK/dnswatch.err")"; exit 1; }
    sleep 0.2
done

# mark NAME: sends a marker query and prints the number of the watcher's line
# that logs it; every query captured before it is logged above that line.
mark() {
    local name="$1.disable-tele.invalid" n
    "$WORK/tele-app" mark "$name" || { echo "::error::the marker query failed" >&2; exit 1; }
    until n="$(grep -n -m1 -F " $name " "$WORK/dns.log" 2>/dev/null | cut -d: -f1)" && [ -n "$n" ]; do
        pid_running "$WATCH_SUDO" || { echo "::error::the DNS watcher stopped: $(cat "$WORK/dnswatch.err")" >&2; exit 1; }
        sleep 0.2
    done
    echo "$n"
}
# window FROM TO: the watcher's lines after line FROM up to line TO.
window() { sed -n "$(($1 + 1)),$(($2))p" "$WORK/dns.log"; }
# tele_lookups FROM TO: the distinct queries (id, name, type) in the window
# that name the telemetry host.
tele_lookups() { window "$1" "$2" | awk -v h="$TELE_HOST" '$2 == h' | sort -u | wc -l; }

# record NAME [ENV=VALUE...] -- [FLAG...]: one keploy record session of one
# GET /hello; sets LEG_FROM and LEG_TO, the marker lines around it.
record() {
    local name="$1" env=()
    shift
    while [ "$#" -gt 0 ] && [ "$1" != -- ]; do env+=("$1"); shift; done
    shift
    mkdir -p "$WORK/$name" && cd "$WORK/$name" || exit 1
    LEG_FROM="$(mark "$name-start")" || exit 1
    sudo -E env -u KEPLOY_PROVEN "${env[@]}" PATH="$PATH" "$RECORD_BIN" record -c "$APP_CMD" "${MODE_FLAGS[@]}" "$@" \
        --generateGithubActions=false >"record.log" 2>&1 </dev/null &
    REC_PID=$!
    # Ready is the app's answer, not its port: in Docker the published port
    # listens (docker-proxy) before the app does, and a GET then is reset.
    # A GET that does not reach the app records nothing.
    until [ "$(curl -s --max-time 5 "http://127.0.0.1:$APP/hello" 2>/dev/null)" = hello ]; do
        pid_running "$REC_PID" || { fail "$name: the app did not answer under keploy record $*"; break; }
        sleep 0.5
    done
    until [ "$(test_files)" -ge 1 ]; do
        pid_running "$REC_PID" || { fail "$name: keploy record wrote no test case"; break; }
        sleep 0.2
    done
    stop_pid "$REC_PID"
    REC_PID=""
    LEG_TO="$(mark "$name-end")" || exit 1
    own
    cd "$WORK" || exit 1
    echo "-- $name: DNS queries seen during the session (name type):"
    window "$LEG_FROM" "$LEG_TO" | grep -v -F '.disable-tele.invalid ' | sort -u | awk '{print $2, $3}' | sort | uniq -c | head -20
}

echo "== $MODE: keploy record --disable-tele"
record flag -- --disable-tele
n="$(tele_lookups "$LEG_FROM" "$LEG_TO")"
echo "$TELE_HOST lookups: $n"
[ "$n" = 0 ] || {
    fail "keploy record --disable-tele looked $TELE_HOST up $n times"
    grep -a -m5 -iE 'error|tele' "$WORK/flag/record.log"
}

echo "== $MODE: keploy record (control, no flag) ${CONTROL_ENV[*]}"
record control "${CONTROL_ENV[@]}" --
n="$(tele_lookups "$LEG_FROM" "$LEG_TO")"
echo "$TELE_HOST lookups: $n"
[ "$n" -gt 0 ] || fail "the watcher saw no $TELE_HOST lookup from keploy record without --disable-tele, so the --disable-tele leg proves nothing"

if [ "$FAIL" -ne 0 ]; then
    echo "disable-tele ($MODE): $FAIL failure(s); logs in $WORK"
    for f in "$WORK"/*/record.log; do echo "--- $f (last lines)"; tail -n 15 "$f"; done
    exit 1
fi
echo "disable-tele ($MODE): passed"
