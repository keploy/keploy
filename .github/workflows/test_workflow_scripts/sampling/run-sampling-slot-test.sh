#!/usr/bin/env bash
# keploy runs under sudo, which hands it only the variables its command line
# names (sudo-rs, Ubuntu 26.04's sudo, ignores -E): SUDO_ENV names the
# switches a workflow sets for keploy, each only while it is set.
# shellcheck source=../sudo_env.sh
source "$(dirname "${BASH_SOURCE[0]}")/../sudo_env.sh" || exit 1

# The sampling slot-release check: with --enable-sampling=1, an exchange
# that is never captured gives its slot back as that is known, so a request
# sent on another connection while it is still in flight is captured.
#
#   1. `keploy record --enable-sampling=1 -c "python3 slot_app.py"`, with a
#      keploy.yml filter that leaves out requests whose User-Agent is a
#      kubelet's (^kube-probe/).
#   2. A left-out request in flight: a kube-probe GET /hold, held by the app
#      ($SLOT_DIR/hold.in once it is there) until the check lets it go. A
#      GET /order?case=probe is sent meanwhile.
#   3. A stream in flight: GET /events, whose first event the client has
#      before a GET /order?case=stream is sent; the stream then ends.
#   4. SIGINT keploy. Assert: every request was answered 200; both /order
#      requests were recorded; nothing of /hold, /events or /ready was.
#
# Each step waits for what the app or the client has seen, not for a time,
# so the check reads the same on any runner. Holding the slot past the point
# that shows the exchange is never captured, keploy passes each /order
# through uncaptured.
#
# Expects (env): KEPLOY_BIN. Optional: SLOT_WORK_DIR (default
# $RUNNER_TEMP/sampling-slot or ./sampling-slot), SLOT_APP_PORT (default 8081).

set -Eeuo pipefail

: "${KEPLOY_BIN:?KEPLOY_BIN must point to the keploy binary}"
if [ ! -x "$KEPLOY_BIN" ]; then
    echo "::error::keploy binary $KEPLOY_BIN is not executable"
    exit 1
fi
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK_DIR="${SLOT_WORK_DIR:-${RUNNER_TEMP:-$PWD}/sampling-slot}"
APP_PORT="${SLOT_APP_PORT:-8081}"
BASE="http://localhost:${APP_PORT}"

section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

# keploy (root) writes $WORK_DIR/keploy: a rerun in the same directory removes it as root.
sudo rm -rf "$WORK_DIR"
mkdir -p "$WORK_DIR/slot" "$WORK_DIR/out"
chmod 0777 "$WORK_DIR/slot"
cp "$SCRIPT_DIR/slot_app.py" "$WORK_DIR/slot_app.py"
cd "$WORK_DIR"

sudo mkdir -p ~/.keploy
sudo touch ~/.keploy/installation-id.yaml
echo "ObjectID('123456789')" | sudo tee ~/.keploy/installation-id.yaml > /dev/null

cat > keploy.yml <<'EOF'
record:
  filters:
    - headers:
        User-Agent: "^kube-probe/"
      filterPolicy: "exclude"
EOF

KEPLOY_PID=""
cleanup() {
    rc=$?
    section "Cleanup: stopping keploy and the slot app"
    # Let anything still held go, then stop only what this check started.
    touch slot/hold.release slot/events.release 2>/dev/null || true
    if [ -n "$KEPLOY_PID" ] && sudo kill -0 "$KEPLOY_PID" 2>/dev/null; then
        sudo kill -INT "$KEPLOY_PID" 2>/dev/null || true
        sleep 3
        sudo kill -KILL "$KEPLOY_PID" 2>/dev/null || true
    fi
    # The bracket keeps the pattern from matching pkill's own command line.
    sudo pkill -KILL -f "$WORK_DIR/[s]lot_app.py" 2>/dev/null || true
    endsec
    section "slot-record.log tail"
    tail -n 100 slot-record.log 2>/dev/null || echo "(no slot-record.log)"
    endsec
    exit $rc
}
trap cleanup EXIT

# wait_file FILE WHAT: wait (up to 60 s, a bound for a check that failed)
# until FILE exists and is not empty.
wait_file() {
    for _ in $(seq 1 600); do
        [ -s "$1" ] && return 0
        sleep 0.1
    done
    echo "::error::$2 never happened ($1 is missing)"
    exit 1
}

section "Start keploy record (--enable-sampling=1, kube-probe filtered out)"
# The log is the runner's own file, so it is opened before sudo.
# shellcheck disable=SC2024
sudo env PATH="$PATH" "${SUDO_ENV[@]}" SLOT_DIR="$WORK_DIR/slot" PORT="$APP_PORT" \
    "$KEPLOY_BIN" record \
        -c "python3 $WORK_DIR/slot_app.py" \
        --enable-sampling=1 \
        --generate-github-actions=false \
        > slot-record.log 2>&1 &
echo "sudo pid: $!"
endsec

section "Wait for the slot app on :$APP_PORT"
ready=0
for i in $(seq 1 60); do
    if curl -sf --max-time 2 -A 'kube-probe/1.31' "$BASE/ready" >/dev/null 2>&1; then
        echo "slot app ready after ${i}s"
        ready=1
        break
    fi
    sleep 1
done
if [ "$ready" -ne 1 ]; then
    echo "::error::the slot app failed to come up within 60s"
    exit 1
fi
KEPLOY_PID="$(pgrep -n -f "keploy record -c python3 $WORK_DIR/slot_app.py" || true)"
echo "keploy pid: ${KEPLOY_PID:-none}"
endsec

# get NAME URL [curl args...]: a GET from a curl of its own, so on a connection
# of its own, its status in out/NAME.status and its body in out/NAME.body.
get() {
    local name=$1 url=$2
    shift 2
    curl -s --max-time 90 -o "out/$name.body" -w '%{http_code}' "$@" "$url" \
        > "out/$name.status" || echo 000 > "out/$name.status"
}

section "A left-out request in flight: kube-probe GET /hold, then GET /order?case=probe"
get hold "$BASE/hold" -A 'kube-probe/1.31' &
HOLD=$!
wait_file slot/hold.in "the kube-probe request reaching the app"
get order-probe "$BASE/order?case=probe"
touch slot/hold.release
wait "$HOLD"
endsec

section "A stream in flight: GET /events, then GET /order?case=stream"
get events "$BASE/events" -N &
EVENTS=$!
wait_file slot/events.in "the stream's first event at the app"
for _ in $(seq 1 600); do
    grep -q 'data: one' out/events.body 2>/dev/null && break
    sleep 0.1
done
grep -q 'data: one' out/events.body || { echo "::error::the client never had the stream's first event"; exit 1; }
get order-stream "$BASE/order?case=stream"
touch slot/events.release
wait "$EVENTS"
endsec

section "Stop keploy (SIGINT)"
if [ -n "$KEPLOY_PID" ]; then
    sudo kill -INT "$KEPLOY_PID" || true
    for _ in $(seq 1 60); do
        sudo kill -0 "$KEPLOY_PID" 2>/dev/null || break
        sleep 1
    done
fi
KEPLOY_PID=""
endsec

section "Check the answers and slot-record.log"
for name in hold order-probe events order-stream; do
    status=$(cat "out/$name.status")
    echo "$name: $status"
    if [ "$status" != 200 ]; then
        echo "::error::$name was answered $status, want 200"
        exit 1
    fi
done
if grep -E "panic:|WARNING: DATA RACE" slot-record.log >/dev/null 2>&1; then
    echo "::error::a panic or a data race in slot-record.log"
    exit 1
fi
endsec

section "Count recorded test cases"
# Nothing recorded counts 0 of each, so that every check below is reported.
tests=()
for f in ./keploy/test-set-0/tests/*.yaml; do
    [ -e "$f" ] && tests+=("$f")
done
echo "${#tests[@]} test case(s) recorded"
count() {
    if [ "${#tests[@]}" -eq 0 ]; then echo 0; return; fi
    { grep -l -- "$1" "${tests[@]}" || true; } | wc -l | tr -d ' '
}
probe=$(count 'case=probe')
stream=$(count 'case=stream')
leftout=$(count '/hold')
streams=$(count '/events')
readies=$(count '/ready')
echo "recorded: /order?case=probe $probe, /order?case=stream $stream, /hold $leftout, /events $streams, /ready $readies"
endsec

# Each check is reported, so that one failing does not hide another.
failed=0
if [ "$probe" -ne 1 ]; then
    echo "::error::GET /order?case=probe, sent while a request the filters leave out was in flight, was recorded $probe time(s), want 1: the left-out request held the one sampling slot"
    failed=1
fi
if [ "$stream" -ne 1 ]; then
    echo "::error::GET /order?case=stream, sent while a stream was open, was recorded $stream time(s), want 1: the stream held the one sampling slot"
    failed=1
fi
if [ "$leftout" -ne 0 ] || [ "$streams" -ne 0 ] || [ "$readies" -ne 0 ]; then
    echo "::error::recorded what is never to be: /hold $leftout, /events $streams, /ready $readies"
    failed=1
fi
if [ "$failed" -ne 0 ]; then
    exit 1
fi
echo "✅ sampling slot-release check passed"
