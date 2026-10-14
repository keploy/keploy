#!/usr/bin/env bash
# E2E: strict HTTP matching under the proven switch (design v14 row S1), on
# keploy #4686's request-drift app.
#
# The app answers GET /order with {"order":"ok"} whatever its dependency
# returns, after it charges a payments service with
# {"amount": AMOUNT, "currency": "USD"}. Its response never shows where or
# what it sent, so only the dependency call can show a change. Replayed with
# the payments service down:
#
# By host (an ordinary recording, no switch, at http://127.0.0.1:9099):
#   1. KEPLOY_PROVEN=1, unchanged: the set passes (no false red).
#   2. KEPLOY_PROVEN=1, the same call to http://localhost:9099: the set fails,
#      though every response matches, and the report names the host.
#   3. Switch off, the same: the set passes, as on main.
#   4. KEPLOY_PROVEN=1, AMOUNT=250 (recorded 100): the set fails, naming
#      body.amount as what differs, with no value in the reason's field list
#      (differs).
#   5. Switch off, AMOUNT=250: the set passes, as on main (the lenient matcher
#      serves the recording).
#
# A container on a random published port, as Testcontainers runs one
# (recorded under the switch):
#   6. The recording carries the container's key (containerkey); nothing
#      compares it yet.
#   7. KEPLOY_PROVEN=1, a new container of the image on another random port:
#      the set fails, naming the host. Its authority changed, and until the
#      key names a container by its own configuration it is not matched by
#      its key: the known limit of this step.
#   8. Switch off, the same: the set passes, as on main.
#   9. No data race in any log.
#
# Readiness is the app's listening port while recording, a container's own
# "ready" line, and keploy's own probe of the app (--health-path, any HTTP
# answer) while replaying. The script stops only the processes and
# containers it started. Ports: 8090 (app), 9099 (payments), as the app has
# them. Env: RECORD_BIN, REPLAY_BIN; DEP_IMAGE (default python:3.11-slim).
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-$RECORD_BIN}"
IMAGE="${DEP_IMAGE:-python:3.11-slim}"
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP=8090
DEP=9099
FAIL=0
fail() {
    echo "::error::$*"
    FAIL=$((FAIL + 1))
}

WORK="$(mktemp -d)"
echo "work dir: $WORK"
(cd "$SRC" && go build -o "$WORK/order-app" ./app && go build -o "$WORK/payments-svc" ./payments) || { echo "::error::build failed"; exit 1; }
cd "$WORK" || exit 1

listening() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
# wait_listen <port> <pid>: until something listens on port; fails at once
# when pid (the process that should start it) has exited.
wait_listen() {
    until listening "$1"; do
        kill -0 "$2" 2>/dev/null || return 1
        sleep 0.2
    done
}
test_files() { find keploy/test-set-0/tests -maxdepth 1 -name '*.yaml' -size +0c 2>/dev/null | wc -l; }
# wait_tests <n> <pid>: until n test cases are written; fails at once when
# keploy (pid) has exited.
wait_tests() {
    until [ "$(test_files)" -ge "$1" ]; do
        kill -0 "$2" 2>/dev/null || return 1
        sleep 0.2
    done
}
own() { sudo chown -R "$(id -u):$(id -g)" . 2>/dev/null || true; }
stop_pid() {
    local sudo_pid="$1" child
    child="$(pgrep -P "$sudo_pid" 2>/dev/null | head -n1)"
    [ -n "$child" ] && sudo kill -INT "$child" 2>/dev/null
    wait "$sudo_pid" 2>/dev/null
}
newest_run() { ls -1d keploy/reports/test-run-* 2>/dev/null | sort -t- -k3 -n | tail -n1; }
REPORT() { echo "$(newest_run)/test-set-0-report.yaml"; }
set_status() { grep -m1 '^status:' "$(REPORT)" 2>/dev/null | awk '{print $2}'; }

# The payments service in a container: answers POST /charge, says "ready"
# once it listens.
SERVER='import http.server, sys
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length") or 0))
        b = b"{\"charged\":true}"
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    def log_message(self, *a): pass
s = http.server.HTTPServer(("0.0.0.0", 9099), H)
print("ready", flush=True)
s.serve_forever()'
CONTAINERS=()
# wait_ready <container>: until the container prints "ready"; fails at once
# when it exits first.
wait_ready() {
    local line
    while IFS= read -r line; do
        [ "$line" = ready ] && return 0
    done < <(docker logs -f "$1" 2>&1)
    return 1
}
# start_payments <image> <cmd...>: a payments container on a new random
# published port; sets PORT and CID.
start_payments() {
    local image="$1"
    shift
    CID=$(docker run -d --init -p "127.0.0.1::$DEP" "$image" "$@") || { echo "::error::cannot start a container of $image"; exit 1; }
    CONTAINERS+=("$CID")
    wait_ready "$CID" || { echo "::error::the $image container exited before it was ready"; docker logs "$CID"; exit 1; }
    PORT=$(docker port "$CID" "$DEP/tcp" | grep -m1 '^127\.0\.0\.1:' | cut -d: -f2)
    [ -n "$PORT" ] || { echo "::error::the $image container has no published port"; exit 1; }
}
stop_containers() {
    [ "${#CONTAINERS[@]}" -gt 0 ] && docker rm -f "${CONTAINERS[@]}" >/dev/null 2>&1
    CONTAINERS=()
}

# record <proven: 1 or ""> <payments url>: records two GET /order.
record() {
    local proven="$1" url="$2"
    rm -rf keploy
    sudo -E env PATH="$PATH" KEPLOY_PROVEN="$proven" "$RECORD_BIN" record -c "env AMOUNT=100 PAYMENTS_URL=$url ./order-app" \
        --generateGithubActions=false --disable-tele >"record-${proven:-off}.log" 2>&1 </dev/null &
    REC_PID=$!
    wait_listen "$APP" "$REC_PID" || { echo "::error::the app did not start under keploy record"; tail -30 "record-${proven:-off}.log"; exit 1; }
    for _ in 1 2; do
        out="$(curl -s "http://127.0.0.1:$APP/order")"
        [ "$out" = '{"order":"ok"}' ] || fail "GET /order answered '$out' while recording"
    done
    wait_tests 2 "$REC_PID" || fail "keploy record wrote $(test_files) test cases, want 2"
    stop_pid "$REC_PID"
    REC_PID=""
    own
    [ "$(grep -c '^kind: Http$' keploy/test-set-0/mocks.yaml 2>/dev/null)" -ge 2 ] || { echo "::error::the charge calls were not recorded"; exit 1; }
}

# replay <name> <amount> <payments url> [env...]: keploy test of the
# recording; RC is its exit code.
replay() {
    local name="$1" amount="$2" url="$3" before
    shift 3
    before="$(newest_run)"
    sudo -E env PATH="$PATH" "$@" "$REPLAY_BIN" test -c "env AMOUNT=$amount PAYMENTS_URL=$url ./order-app" \
        --delay 1 --health-path /healthz --generateGithubActions=false --disable-tele >"$name.log" 2>&1 </dev/null
    RC=$?
    own
    if [ "$(newest_run)" = "$before" ] || [ ! -f "$(REPORT)" ]; then
        fail "$name: keploy test wrote no report (exit $RC)"
        tail -30 "$name.log"
    fi
}
expect_pass() {
    if [ "$RC" -ne 0 ] || [ "$(set_status)" != PASSED ]; then
        fail "$1: the set must pass (exit $RC, status $(set_status))"
        grep -nE 'status:|match_phase|path:|differs' "$(REPORT)" | head -30
    fi
}
# expect_miss <step> <field>: the set failed on a call that got no recorded
# answer, and the report names field as what differs.
expect_miss() {
    if [ "$RC" -eq 0 ] || [ "$(set_status)" != FAILED ]; then
        fail "$1: the set must fail (exit $RC, status $(set_status))"
    fi
    grep -q 'unmatched_calls:' "$(REPORT)" || fail "$1: the report lists no call that got no recorded answer"
    grep -A40 'unmatched_calls:' "$(REPORT)" | grep -qE "path: $2\$" || fail "$1: the report's field diffs do not name $2"
    grep -A60 'unmatched_calls:' "$(REPORT)" | grep -A6 'differs:' | grep -qE -- "- $2\$" || fail "$1: the report's differs do not name $2"
}

for p in "$APP" "$DEP"; do
    if listening "$p"; then echo "::error::port $p is in use"; exit 1; fi
done
REC_PID=""
DEP_PID=""
cleanup() {
    [ -n "$REC_PID" ] && stop_pid "$REC_PID"
    [ -n "$DEP_PID" ] && kill "$DEP_PID" 2>/dev/null
    stop_containers
}
trap cleanup EXIT

echo "== record by host (no switch) =="
./payments-svc >payments.log 2>&1 &
DEP_PID=$!
wait_listen "$DEP" "$DEP_PID" || { echo "::error::the payments service did not start"; exit 1; }
record "" "http://127.0.0.1:$DEP"
kill "$DEP_PID" 2>/dev/null
wait "$DEP_PID" 2>/dev/null
DEP_PID=""

echo "== 1. KEPLOY_PROVEN=1, unchanged: passes =="
replay unchanged 100 "http://127.0.0.1:$DEP" KEPLOY_PROVEN=1
expect_pass "step 1"

echo "== 2. KEPLOY_PROVEN=1, the call to localhost: fails naming the host =="
replay host 100 "http://localhost:$DEP" KEPLOY_PROVEN=1
expect_miss "step 2" host

echo "== 3. switch off, the call to localhost: passes =="
replay hostoff 100 "http://localhost:$DEP"
expect_pass "step 3"

echo "== 4. KEPLOY_PROVEN=1, AMOUNT=250: fails naming body.amount =="
replay drift 250 "http://127.0.0.1:$DEP" KEPLOY_PROVEN=1
expect_miss "step 4" body.amount
grep -A60 'unmatched_calls:' "$(REPORT)" | grep -A6 'differs:' | grep -q '250' && fail "step 4: differs holds a value"

echo "== 5. switch off, AMOUNT=250: passes, as on main =="
replay driftoff 250 "http://127.0.0.1:$DEP"
expect_pass "step 5"

echo "== record a container on a random port (KEPLOY_PROVEN=1, $IMAGE) =="
docker pull -q "$IMAGE" >/dev/null || { echo "::error::cannot pull $IMAGE"; exit 1; }
start_payments "$IMAGE" python3 -c "$SERVER"
REC_CID=$CID
REC_PORT=$PORT
record 1 "http://127.0.0.1:$REC_PORT"

echo "== 6. the recording carries the container's key =="
grep -q "containerKey: container:${IMAGE}:${DEP}#1" keploy/test-set-0/mocks.yaml || fail "step 6: the charge calls were not recorded with their container's key"

# The new container starts while the recorded one still holds its port, so
# Docker gives it another.
start_payments "$IMAGE" python3 -c "$SERVER"
docker rm -f "$REC_CID" >/dev/null 2>&1
[ "$PORT" != "$REC_PORT" ] || { echo "::error::Docker published two running containers on port $PORT"; exit 1; }

echo "== 7. KEPLOY_PROVEN=1, a new container on another port: fails naming the host =="
replay newcontainer 100 "http://127.0.0.1:$PORT" KEPLOY_PROVEN=1
expect_miss "step 7" host

echo "== 8. switch off, the same: passes, as on main =="
replay newcontaineroff 100 "http://127.0.0.1:$PORT"
expect_pass "step 8"
stop_containers

echo "== 9. no data race =="
for f in record-off.log record-1.log unchanged.log host.log hostoff.log drift.log driftoff.log newcontainer.log newcontaineroff.log; do
    grep -q 'WARNING: DATA RACE' "$f" 2>/dev/null && fail "data race in $f"
done

if [ "$FAIL" -ne 0 ]; then
    echo "proven strict-HTTP e2e: $FAIL failure(s); work dir $WORK"
    exit 1
fi
echo "PASS: under KEPLOY_PROVEN=1 a call to another authority, or with another body value, gets no recorded answer and fails the test, naming the field; a container's key is recorded; without the switch nothing changes"
