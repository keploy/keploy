#!/usr/bin/env bash
# E2E: `keploy test --base-path <url>` replays a recording against an app that
# is already running. Keploy starts neither the app nor an agent and mocks
# nothing: each recorded request goes to the base path, the answers are
# compared, the report is written and the exit code says how it went.
#
# No test ran this mode, and it had stopped working. The replay stored the
# test set's mocks on an agent it never started, so it failed before the first
# test with
#   Post "/storemocks": unsupported protocol scheme ""
# wrote no report, and exited 0. Under that, every request went to localhost
# on the port the test was recorded on, whatever the base path said, asked for
# the app by the name it was recorded under, and had its URL unescaped on the
# way (%20 to a space, %2F to a path separator, %23 to the start of a
# fragment); and a streaming test was never given the base path at all.
#
# Records six tests of an app that prices orders from a dependency -- one with
# escapes in its path and query that the app answers with what it was asked,
# one streamed -- then replays them with --base-path, not as root:
#   1. the app where it was recorded: all pass, exit 0;
#   2. the app at another address (127.0.0.2, on the dependency's old port),
#      with the dependency now answering at the recorded address: all pass,
#      exit 0. Sent to the recorded address they get the price list's 404, and
#      asked for under the recorded name the app refuses them as a front that
#      routes by name does. The run says, once, that the recorded name is not
#      the one it asks under;
#   3. the app answering one test differently: that test fails, the report
#      says FAILED and the exit code is 1;
#   4. nothing listening at the base path: every test fails, exit code 1;
#   5. a base path keploy cannot read, with the app up where it was
#      recorded: no scheme, and a scheme with no host. No test is sent, no
#      report, exit code 1. The first was left out of the URLs behind an
#      ERROR line each, and the second read as naming no host, and the tests
#      passed against the app at the recorded address;
#   6. as 2, with a seventh test whose URL the base path cannot go in (its
#      origin is a template value): that test fails and is not sent, exit
#      code 1. It was sent to the recorded address, where the dependency is;
#   7. the base path naming a port nothing listens on, and --port the app's:
#      all pass, and the run says, once, that the base path's port is not
#      used;
#   8. the run stopped (SIGINT) as its second test waits for an answer, with
#      --must-pass: the first test's pass is kept, no test fails, none is
#      deleted, nothing is logged as an ERROR, exit 0. Every test after the
#      stop was started, failed at once and reported FAILED, and --must-pass
#      deleted the whole test set.
#
# It uses ports 18911 and 18912 on 127.0.0.1 and 127.0.0.2, and refuses to
# start while something else listens on one.
#
# Env: RECORD_BIN and REPLAY_BIN, the PR `build` binary for both (mock_linux.yml).
set -uo pipefail

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-$RECORD_BIN}"
WORK="$(mktemp -d)"
cd "$WORK" || exit 1
APP_PORT=18911
DEP_PORT=18912
TESTS=6
FAIL=0
fail() { # FAIL counts failures, so each replay can tell whether it added one
  echo "FAIL: $*"
  FAIL=$((FAIL + 1))
}

# The dependency: a price list. It answers anything else with a 404 that says
# what it is, which is what a test sent to the wrong server gets in leg 2, and
# its log keeps each request it was sent, for leg 6.
cat > dep.py <<'PY'
import http.server, json, socketserver, sys
PORT = int(sys.argv[1]); PRICES = {"widget": 5, "gadget": 7}
class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def answer(self):
        self.rfile.read(int(self.headers.get("Content-Length") or 0))
        item = self.path.rsplit("/", 1)[-1]
        if self.path.startswith("/price/") and item in PRICES: code, obj = 200, {"item": item, "price": PRICES[item]}
        else: code, obj = 404, {"error": "this is the price list, not the app"}
        body = json.dumps(obj).encode()
        self.send_response(code); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    do_GET = do_POST = answer
socketserver.ThreadingTCPServer.allow_reuse_address = True
with socketserver.ThreadingTCPServer(("127.0.0.1", PORT), H) as s: s.serve_forever()
PY

# The app. argv: BIND PORT DEP_PORT [bug|hold]. With "bug" an order costs one
# more than its items, the regression leg 3 replays against. With "hold" it
# takes the request for order 1, says so in the file app.holding, and never
# answers it, for leg 8. It answers only a request that asks for it by its own
# address, as an app behind a front that routes by name is only reached under
# its name; /echo/ answers with the request target as it came over the wire;
# /events streams three lines.
cat > app.py <<'PY'
import http.server, json, socketserver, sys, time, urllib.request
BIND, PORT, DEP = sys.argv[1], int(sys.argv[2]), "http://127.0.0.1:%s" % sys.argv[3]
MODE = sys.argv[4] if len(sys.argv) > 4 else ""
ME = "%s:%d" % (BIND, PORT)
def price(item):
    with urllib.request.urlopen(DEP + "/price/" + item, timeout=5) as r: return json.loads(r.read())["price"]
class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def send(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def misdirected(self):
        asked = self.headers.get("Host")
        if asked == ME: return False
        self.send(421, {"error": "asked for %s, this is %s" % (asked, ME)}); return True
    def stream(self):
        self.send_response(200); self.send_header("Content-Type", "application/x-ndjson")
        self.send_header("Transfer-Encoding", "chunked"); self.end_headers()
        for n in (1, 2, 3):
            line = (json.dumps({"n": n, "price": price("widget")}) + "\n").encode()
            self.wfile.write(b"%x\r\n%s\r\n" % (len(line), line)); self.wfile.flush(); time.sleep(0.1)
        self.wfile.write(b"0\r\n\r\n")
    def hold(self):
        open("app.holding", "w").close()
        while True: time.sleep(60)
    def do_GET(self):
        if self.misdirected(): return
        if self.path == "/health": return self.send(200, {"ok": True})
        if self.path == "/orders/1" and MODE == "hold": return self.hold()
        if self.path == "/orders/1": return self.send(200, {"id": 1, "item": "widget", "price": price("widget")})
        if self.path.startswith("/echo/"): return self.send(200, {"asked": self.path})
        if self.path == "/events": return self.stream()
        self.send(404, {"error": "no such order"})
    def do_POST(self):
        order = json.loads(self.rfile.read(int(self.headers.get("Content-Length") or 0)))
        if self.misdirected(): return
        total = price(order["item"]) * order["qty"] + (1 if MODE == "bug" else 0)
        self.send(201, {"item": order["item"], "qty": order["qty"], "total": total})
    def log_message(self, *a): pass
socketserver.ThreadingTCPServer.allow_reuse_address = True
with socketserver.ThreadingTCPServer((BIND, PORT), H) as s: s.serve_forever()
PY

PIDS=()
down() {
  [ "${#PIDS[@]}" -eq 0 ] && return
  kill "${PIDS[@]}" 2>/dev/null
  wait "${PIDS[@]}" 2>/dev/null
  PIDS=()
}
trap down EXIT

# listening HOST PORT: something accepts a connection there. A connection and
# no request: under `keploy record` a request is a test, and the recording is
# to hold the six below and no probe.
listening() { (exec 3<>"/dev/tcp/$1/$2") 2>/dev/null; }

# unused HOST PORT: nothing listens there yet. A listener that is not this
# script's answers wait_for_port as its own would, and the tests are then
# recorded from it or replayed against it.
unused() {
  listening "$1" "$2" || return 0
  echo "FAIL: $1:$2 is in use by something else, and this script needs it"
  return 1
}

# wait_for_port HOST PORT SECONDS: until something listens there.
wait_for_port() {
  local i
  for ((i = 0; i < $3 * 5; i++)); do
    listening "$1" "$2" && return 0
    sleep 0.2
  done
  echo "nothing listened on $1:$2 within $3s"
  return 1
}

# serve LOG HOST PORT COMMAND...: starts a server of this script's in the
# background and waits until it listens on HOST:PORT. It has to be the one
# listening: python ends at once when the port is not its to have.
serve() {
  local log=$1 host=$2 port=$3 pid
  shift 3
  unused "$host" "$port" || return 1
  "$@" > "$log" 2>&1 &
  pid=$!
  PIDS+=("$pid")
  wait_for_port "$host" "$port" 120 || return 1
  kill -0 "$pid" 2>/dev/null && return 0
  echo "FAIL: '$*' did not stay up: $(cat "$log")"
  return 1
}

start_dep() { # start_dep PORT
  serve dep.log 127.0.0.1 "$1" python3 dep.py "$1"
}

# up BIND PORT DEP_PORT [bug|hold]: the dependency, and the app in front of it,
# as a user runs them before pointing keploy at the app.
up() {
  start_dep "$3" || return 1
  serve app.log "$1" "$2" python3 app.py "$@"
}

# The recording's six requests, once keploy has the app up. That takes as long
# as keploy gives its agent to come up, which is the 330s this outwaits.
drive() {
  local at="http://127.0.0.1:$APP_PORT" rc=0
  wait_for_port 127.0.0.1 "$APP_PORT" 360 || return 1
  : > app.up
  curl -sS --max-time 30 "$at/health" || rc=1
  curl -sS --max-time 30 "$at/orders/1" || rc=1
  curl -sS --max-time 30 -H 'Content-Type: application/json' -d '{"item":"gadget","qty":2}' "$at/orders" || rc=1
  curl -sS --max-time 30 "$at/orders/9" || rc=1
  curl -sS --max-time 30 "$at/echo/a%2Fb/?q=a%20b%23c%26d%3De" || rc=1
  curl -sS --max-time 30 "$at/events" || rc=1
  return "$rc"
}

echo "== record $TESTS tests of the app =="
# Every address a leg listens on, before anything is started.
for at in "127.0.0.1 $APP_PORT" "127.0.0.1 $DEP_PORT" "127.0.0.2 $DEP_PORT"; do
  # shellcheck disable=SC2086 # a host and a port, split on purpose
  unused $at || exit 1
done
start_dep "$DEP_PORT" || exit 1
drive > drive.log 2>&1 &
DRIVER=$!
# The timer stops the recording: the app is a server and would not end. It
# runs from when keploy starts the app, so a slow agent does not use it up.
# shellcheck disable=SC2024 # the log is this user's, so the shell redirects
sudo -E env PATH="$PATH" timeout -k 10 480 "$RECORD_BIN" record -c "python3 app.py 127.0.0.1 $APP_PORT $DEP_PORT" \
  --record-timer 20s --generateGithubActions=false --disable-tele > record.log 2>&1
echo "record exit status: $?"
# Once keploy is gone nothing will answer a driver still waiting for the app.
[ -e app.up ] || kill "$DRIVER" 2>/dev/null
wait "$DRIVER" || fail "the requests to record did not all get an answer"
down
grep -q '"total": 14' drive.log || fail "the app did not price the order while recording: $(cat drive.log)"
grep -q '"asked": "/echo/a%2Fb/?q=a%20b%23c%26d%3De"' drive.log || fail "the app was not asked the escaped request while recording: $(cat drive.log)"
grep -q 'WARNING: DATA RACE' record.log && fail "data race while recording: $(grep -m1 -A12 'WARNING: DATA RACE' record.log)"
grep -q 'ERROR' record.log && fail "the recording logged an ERROR: $(grep -m3 'ERROR' record.log)"
RECORDED=$(find keploy/test-set-0/tests -type f 2>/dev/null | wc -l)
if [ "$RECORDED" -ne "$TESTS" ]; then
  echo "FAIL: recorded $RECORDED tests, want $TESTS"
  tail -40 record.log
  exit 1
fi

# report_of RUN: the status, passed and failed counts of that run's report.
report_of() {
  awk '/^status:/{s=$2} /^success:/{p=$2} /^failure:/{f=$2} END{print s, p, f}' "keploy/reports/test-run-$1/test-set-0-report.yaml"
}

# replay NAME BASE EXIT STATUS PASSED FAILED [FLAG...]: one `keploy test
# --base-path BASE FLAG...`, which must exit EXIT and write a report with that
# status and those counts, or no report at all for the status NONE. A run that
# is meant to pass must not log an ERROR either.
RUN=0
replay() {
  local name=$1 base=$2 want_exit=$3 want="$4 $5 $6" rc got
  local report="keploy/reports/test-run-$RUN/test-set-0-report.yaml"
  shift 6
  [ "${want%% *}" = NONE ] || RUN=$((RUN + 1))
  timeout -k 10 120 "$REPLAY_BIN" test --base-path "$base" --generateGithubActions=false --disable-tele "$@" > "$name.log" 2>&1 </dev/null
  rc=$?
  sed -i 's/\x1b\[[0-9;]*m//g' "$name.log"
  local before=$FAIL
  [ "$rc" -eq "$want_exit" ] || fail "$name: exit code $rc, want $want_exit"
  if grep -qE 'unsupported protocol scheme|failed to store mocks' "$name.log"; then
    fail "$name: the replay asked an agent it never started"
  fi
  grep -q 'WARNING: DATA RACE' "$name.log" && fail "$name: data race"
  if [ "$want_exit" -eq 0 ] && grep -q 'ERROR' "$name.log"; then
    fail "$name: a passing replay logged an ERROR: $(grep -m1 'ERROR' "$name.log")"
  fi
  if [ "${want%% *}" = NONE ]; then
    [ ! -e "$report" ] || fail "$name: the replay went on to its tests and wrote $report"
  elif [ -f "$report" ]; then
    got=$(report_of "$((RUN - 1))")
    [ "$got" = "$want" ] || fail "$name: the report says '$got' (status, passed, failed), want '$want'"
    # The streamed test runs after the others, in a pass of its own.
    grep -q 'Now executing streaming tests' "$name.log" || fail "$name: the streamed test was not replayed as a stream"
  else
    fail "$name: no report at $report"
  fi
  if [ "$FAIL" -ne "$before" ]; then
    tail -40 "$name.log"
  else
    echo "ok: $name"
  fi
}

# said NAME TIMES TEXT: the replay NAME logged TEXT that many times.
said() {
  local got
  got=$(grep -c -- "$3" "$1.log")
  [ "$got" -eq "$2" ] || fail "$1: said '$3' $got time(s), want $2"
}

HOST_NOT_SENT='not under the Host header they were recorded with'

echo "== 1. the app where it was recorded =="
up 127.0.0.1 "$APP_PORT" "$DEP_PORT" || exit 1
replay same-place "http://127.0.0.1:$APP_PORT" 0 PASSED "$TESTS" 0
said same-place 0 "$HOST_NOT_SENT"
down

echo "== 2. the app at another address, the recorded one answered by the dependency =="
up 127.0.0.2 "$DEP_PORT" "$APP_PORT" || exit 1
replay elsewhere "http://127.0.0.2:$DEP_PORT" 0 PASSED "$TESTS" 0
said elsewhere 1 "$HOST_NOT_SENT"
down

echo "== 3. the app answers one test differently =="
up 127.0.0.1 "$APP_PORT" "$DEP_PORT" bug || exit 1
replay regression "http://127.0.0.1:$APP_PORT" 1 FAILED "$((TESTS - 1))" 1
down

echo "== 4. nothing at the base path =="
replay nothing-there "http://127.0.0.1:$APP_PORT" 1 FAILED 0 "$TESTS"

echo "== 5. a base path that is not a URL =="
up 127.0.0.1 "$APP_PORT" "$DEP_PORT" || exit 1
replay not-a-url "127.0.0.1:$APP_PORT" 1 NONE 0 0
replay no-host "http:127.0.0.1:$APP_PORT" 1 NONE 0 0
down

echo "== 6. a test that cannot be pointed at the base path =="
# The health test again, its origin a template value: the base path has no
# scheme and host to replace until the value is filled in, which is after it
# is applied. The value is the recorded address, where leg 2 puts the
# dependency.
HEALTH=$(grep -l "url: http://127.0.0.1:$APP_PORT/health" keploy/test-set-0/tests/*.yaml)
sed -e 's|^name: .*|name: unpointable-1|' -e "s|^    url: .*|    url: '{{string .origin}}/unpointable'|" \
  "$HEALTH" > keploy/test-set-0/tests/unpointable-1.yaml
# The test set's own config.yaml, when the replays before have written one,
# is put back after.
[ -e keploy/test-set-0/config.yaml ] && cp -p keploy/test-set-0/config.yaml set-config.yaml
printf 'template:\n  origin: http://127.0.0.1:%s\n' "$APP_PORT" > keploy/test-set-0/config.yaml
up 127.0.0.2 "$DEP_PORT" "$APP_PORT" || exit 1
replay unpointable "http://127.0.0.2:$DEP_PORT" 1 FAILED "$TESTS" 1
said unpointable 1 'failed the test without sending it'
down
if grep -q '/unpointable' dep.log; then
  fail "unpointable: the test was sent to the address it was recorded at: $(grep -m1 '/unpointable' dep.log)"
fi
rm -f keploy/test-set-0/tests/unpointable-1.yaml keploy/test-set-0/config.yaml
[ -e set-config.yaml ] && mv set-config.yaml keploy/test-set-0/config.yaml

echo "== 7. --port over the port in the base path =="
up 127.0.0.1 "$APP_PORT" "$DEP_PORT" || exit 1
replay other-port "http://127.0.0.1:1" 0 PASSED "$TESTS" 0 --port "$APP_PORT"
said other-port 1 'the port in the base path is not used'
down

echo "== 8. the run is stopped as a test waits for its answer =="
up 127.0.0.1 "$APP_PORT" "$DEP_PORT" hold || exit 1
# --must-pass, which deletes the tests a run reports as failed, and longer for
# the app to answer than this leg takes, so that the stop is what ends the wait.
timeout -k 30 120 "$REPLAY_BIN" test --base-path "http://127.0.0.1:$APP_PORT" --must-pass --test-sets test-set-0 \
  --api-timeout 100 --generateGithubActions=false --disable-tele > stopped.log 2>&1 </dev/null &
KEPLOY=$!
for ((i = 0; i < 600; i++)); do
  [ -e app.holding ] && break
  sleep 0.2
done
BEFORE=$FAIL
[ -e app.holding ] || fail "stopped: the app was never asked for the order it holds"
# To keploy itself, which is timeout's child: a script's background job is
# started with SIGINT ignored, and timeout does not hand on what it ignores.
pkill -INT -P "$KEPLOY"
wait "$KEPLOY"
rc=$?
down
sed -i 's/\x1b\[[0-9;]*m//g' stopped.log
[ "$rc" -eq 0 ] || fail "stopped: exit code $rc, want 0: the run was stopped with no test failed"
got=$(report_of "$RUN" 2>/dev/null)
[ "$got" = "USER_ABORT 1 0" ] || fail "stopped: the report says '$got' (status, passed, failed), want 'USER_ABORT 1 0'"
grep -q 'Now executing streaming tests' stopped.log && fail "stopped: the run went on to its streamed test"
grep -q 'deleting failing testcases' stopped.log && fail "stopped: --must-pass deleted tests the stop cut short"
grep -q 'ERROR' stopped.log && fail "stopped: a stop logged an ERROR: $(grep -m1 'ERROR' stopped.log)"
LEFT=$(find keploy/test-set-0/tests -type f | wc -l)
[ "$LEFT" -eq "$TESTS" ] || fail "stopped: $LEFT of the $TESTS tests are left in the test set"
if [ "$FAIL" -ne "$BEFORE" ]; then
  tail -40 stopped.log
else
  echo "ok: stopped"
fi

if [ "$FAIL" -ne 0 ]; then
  echo "BASE PATH E2E: FAILED; its output is kept in $WORK"
  exit 1
fi
cd / || exit 1
# With sudo: the recording ran as root.
sudo rm -rf "$WORK"
echo "BASE PATH E2E: PASSED"
