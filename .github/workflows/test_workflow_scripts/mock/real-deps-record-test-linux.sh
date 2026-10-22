#!/usr/bin/env bash
# e2e for mock.real in keploy record and keploy test (the proven model,
# KEPLOY_PROVEN=1, design v14 §4, §3.4 F3, §8.2 rule 4).
#
# An app serves two routes: /both calls a REAL server (listed in keploy.yml
# mock.real as localhost:<port>) and an ordinary dependency, /dep the
# dependency alone.
#   1. keploy record: the call to REAL is reached for real and never recorded;
#      the test case of /both lists localhost:<port> (real:), the one of /dep
#      lists nothing; both calls to the dependency are recorded.
#   2. keploy test, the dependency stopped and REAL up: every test case passes
#      (the dependency answered from the recording, REAL for real), and the
#      report has /both's REAL row and the destination reached.
#   3. keploy test, REAL stopped too: the run fails, and the report says the
#      destination its recordings list was never reached.
#   5. Two test sets replayed by one app (--keep-app-alive) over one pooled
#      connection to REAL: the second set reaches REAL by its answers alone
#      (no dial), and when REAL is a sink that takes requests and never
#      answers, a set that only wrote to it over that connection is told
#      "used but never answered", not "never reached".
# Without the switch the list is not read: both calls are recorded.
#
# Env: RECORD_BIN (the keploy binary under test).
set -uo pipefail
# The proven switch, stated for every keploy this lane runs: the steps that
# say KEPLOY_PROVEN=1 run under it, and every other one with it off
# (KEPLOY_PROVEN=0, the open-source build's default, and keploy enterprise's
# on Windows and macOS), as this lane always ran them. Unset, keploy
# enterprise runs the proven model on Linux.
export KEPLOY_PROVEN=0
export GITHUB_WORKSPACE="${GITHUB_WORKSPACE:-$(pwd)}"

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true
python3 -c "import yaml" 2>/dev/null ||
  python3 -m pip install --quiet --disable-pip-version-check pyyaml ||
  python3 -m pip install --quiet --disable-pip-version-check --break-system-packages pyyaml
python3 -c "import yaml" || { echo "FAIL: pyyaml could not be installed"; exit 1; }

RECORD_BIN="${RECORD_BIN:-keploy}"
WORK="$(mktemp -d)"
cd "$WORK"
FAIL=0
# The servers are this shell's own children, stopped by their pids, and so is
# each keploy run (its sudo).
declare -A SERVER
finish() {
  for p in "${SERVER[@]}"; do kill "$p" 2>/dev/null; wait "$p" 2>/dev/null; done
  cd / && sudo rm -rf "$WORK"
}
trap finish EXIT

# server.py WHO: answers every GET with who it is and the path, notes each in
# hits.<WHO> and each connection in conns.<WHO>, keeps connections alive
# (HTTP/1.1, one thread each), and prints "ready <port>" once it listens on
# 127.0.0.1.
cat > server.py <<'PY'
import http.server, json, sys
who = sys.argv[1]
class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def setup(self):
        super().setup()
        with open("conns." + who, "a") as f:
            f.write("conn\n")
    def do_GET(self):
        with open("hits." + who, "a") as f:
            f.write(self.path + "\n")
        b = json.dumps({"who": who, "path": self.path}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)
    def log_message(self, *a): pass
s = http.server.ThreadingHTTPServer(("127.0.0.1", 0), H)
s.daemon_threads = True
print("ready", s.server_address[1], flush=True)
s.serve_forever()
PY

# start WHO: starts a server and sets PORT to its port once it said ready
# (its first line: a read that ends with the server, never a clock).
start() {
  local fifo="$WORK/$1.fifo" line
  rm -f "$fifo" && mkfifo "$fifo"
  python3 server.py "$1" >"$fifo" 2>&1 &
  SERVER[$1]=$!
  exec 3<"$fifo"
  IFS= read -r line <&3 || { echo "FAIL: server $1 exited before it listened"; exit 1; }
  exec 3<&-
  case "$line" in ready\ *) PORT=${line#ready } ;; *) echo "FAIL: server $1 said $line"; exit 1 ;; esac
}
stop() {
  kill -CONT "${SERVER[$1]}" 2>/dev/null
  kill "${SERVER[$1]}" 2>/dev/null
  wait "${SERVER[$1]}" 2>/dev/null
  unset "SERVER[$1]"
}
start real; REAL_PORT=$PORT
start dep; DEP_PORT=$PORT
APP_PORT=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')

# The app: /both calls REAL then the dependency, /dep the dependency alone,
# each on its own connection. /pooled asks REAL over one connection kept for
# the app's life; /fire writes a request to REAL over another such connection
# and never reads it (a write-only use). The ports come from its environment,
# which keploy passes on.
cat > app.py <<'PY'
import http.client, http.server, json, os, socket
REAL, DEP, APP = (int(os.environ[k]) for k in ("REAL_PORT", "DEP_PORT", "APP_PORT"))
POOL = FIRE = None
FIREBUF = b""
def get(port, path):
    c = http.client.HTTPConnection("127.0.0.1", port)
    c.request("GET", path)
    b = c.getresponse().read()
    c.close()
    return json.loads(b)
def pooled(path):
    global POOL
    if POOL is None:
        POOL = http.client.HTTPConnection("127.0.0.1", REAL)
    POOL.request("GET", path)
    return json.loads(POOL.getresponse().read())
def fire(path):
    # Writes a request to REAL over one connection kept for the app's life,
    # and answers once REAL took it: by reading REAL's answer (FIRE_READ, a
    # recording, so the use comes before the test case's response), or, from
    # a sink that never answers, by the sink's word on SINK_FIFO. Either way
    # what keploy relayed is relayed before the app answers.
    global FIRE
    if FIRE is None:
        FIRE = socket.create_connection(("127.0.0.1", REAL))
    FIRE.sendall(("GET %s HTTP/1.1\r\nHost: localhost\r\n\r\n" % path).encode())
    if os.environ.get("FIRE_READ"):
        read_answer()
    elif os.environ.get("SINK_FIFO"):
        with open(os.environ["SINK_FIFO"]) as f:
            f.readline()
def read_answer():
    global FIREBUF
    while b"\r\n\r\n" not in FIREBUF:
        b = FIRE.recv(4096)
        if not b:
            raise IOError("REAL closed the connection")
        FIREBUF += b
    head, FIREBUF = FIREBUF.split(b"\r\n\r\n", 1)
    n = 0
    for line in head.split(b"\r\n")[1:]:
        k, _, v = line.partition(b":")
        if k.strip().lower() == b"content-length":
            n = int(v)
    while len(FIREBUF) < n:
        b = FIRE.recv(4096)
        if not b:
            raise IOError("REAL closed the connection")
        FIREBUF += b
    FIREBUF = FIREBUF[n:]
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        try:
            if self.path == "/both":
                out = {"real": get(REAL, "/r")["who"], "dep": get(DEP, "/d")["who"]}
            elif self.path == "/dep":
                out = {"dep": get(DEP, "/d")["who"]}
            elif self.path == "/pooled":
                out = {"real": pooled("/p")["who"]}
            elif self.path == "/fire":
                fire("/f")
                out = {"fired": True}
            else:
                out = {"ok": True}
            code = 200
        except Exception as e:
            out, code = {"error": str(e)}, 502
        b = json.dumps(out).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)
    def log_message(self, *a): pass
http.server.HTTPServer(("127.0.0.1", APP), H).serve_forever()
PY
export REAL_PORT DEP_PORT APP_PORT

cat > keploy.yml <<EOF
mock:
  real:
    - localhost:${REAL_PORT}
EOF

# app_up PID: waits until the app accepts a connection, or keploy (PID)
# ends. A bare connect sends no request, so it records no test case.
app_up() {
  until (exec 3<>"/dev/tcp/127.0.0.1/${APP_PORT}") 2>/dev/null; do
    kill -0 "$1" 2>/dev/null || return 1
    sleep 0.2
  done
}

# record LOG PROVEN [PATH...]: records each PATH (/both then /dep when none),
# then stops keploy.
record() {
  local log="$1" proven="$2" kpid p
  shift 2
  [ $# -gt 0 ] || set -- /both /dep
  sudo -E env PATH="$PATH" KEPLOY_PROVEN="$proven" "$RECORD_BIN" record -c "python3 app.py" --disable-tele > "$log" 2>&1 &
  kpid=$!
  if ! app_up "$kpid"; then
    echo "FAIL: keploy record ended before the app answered ($log)"
    tail -60 "$log"
    FAIL=1
    return
  fi
  for p in "$@"; do
    curl -sf "http://127.0.0.1:${APP_PORT}${p}" && echo
  done
  # One stop, as a Ctrl+C in a terminal gives: sudo passes it to keploy's
  # CLI, which stops its agent (in its own process group, which a terminal's
  # Ctrl+C does not reach) after it read what the agent holds.
  sudo kill -INT "$kpid" 2>/dev/null
  wait "$kpid"
}

# replay LOG [FLAG...]: replays the recording under the switch; its exit code
# is keploy test's.
replay() {
  local log="$1"
  shift
  sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$RECORD_BIN" test -c "python3 app.py" --disable-tele "$@" > "$log" 2>&1
}

# cases: "<test case> <path> <real, comma-joined>" for each recorded test case.
cases() {
  python3 - keploy/test-set-0/tests <<'PY'
import os, sys, yaml
rows = []
d = sys.argv[1]
for f in sorted(os.listdir(d)) if os.path.isdir(d) else []:
    doc = yaml.safe_load(open(os.path.join(d, f)))
    url = doc["spec"]["req"]["url"]
    rows.append("%s %s" % (url.split("://", 1)[-1].split("/", 1)[-1], ",".join(doc.get("real") or []) or "-"))
print("\n".join(sorted(rows)))
PY
}

# mock_paths: the path of each HTTP mock of the recording, sorted.
mock_paths() {
  python3 - keploy/test-set-0/mocks.yaml <<'PY'
import sys, yaml
try:
    docs = list(yaml.safe_load_all(open(sys.argv[1])))
except FileNotFoundError:
    docs = []
out = [d["spec"]["req"]["url"].split("://", 1)[-1].split("/", 1)[-1] for d in docs if d and d.get("kind") == "Http"]
print("\n".join("/" + p for p in sorted(out)))
PY
}

expect() {
  local file="$1" what="$2"
  shift 2
  printf '%s\n' "$@" > "$file.want"
  if ! diff -u "$file.want" "$file"; then
    echo "FAIL: $what"
    FAIL=1
  fi
}

echo "== 1. keploy record under the proven switch =="
record rec.log 1
cases > cases.txt; cat cases.txt
expect cases.txt "the test case of /both does not list the REAL destination, or another lists one" \
  "both localhost:${REAL_PORT}" "dep -"
mock_paths > mocks.txt; cat mocks.txt
expect mocks.txt "a call to REAL was recorded, or a call to the dependency was not" "/d" "/d"
[ -s hits.real ] || { echo "FAIL: REAL was not reached while recording"; FAIL=1; }
# keploy reads what it could not capture while its agent still runs.
if grep -q "could not read from its agent which mock.real" rec.log; then
  echo "FAIL: keploy record read the run's mock.real after its agent stopped"
  FAIL=1
fi

echo "== 2. keploy test, the dependency stopped, REAL up =="
stop dep
rm -f hits.real
if ! replay test-up.log; then
  echo "FAIL: keploy test failed with REAL up"
  tail -80 test-up.log
  FAIL=1
fi
[ -s hits.real ] || { echo "FAIL: REAL was not reached while replaying"; FAIL=1; }
report=$(ls -t keploy/reports/*/test-set-0-report.yaml 2>/dev/null | head -1)
python3 - "$report" "localhost:${REAL_PORT}" <<'PY' || FAIL=1
import sys, yaml
r = yaml.safe_load(open(sys.argv[1]))
dest = sys.argv[2]
assert dest in ((r.get("real") or {}).get("reached") or []), "the report does not say %s was reached: %r" % (dest, r.get("real"))
assert r.get("real_listed") == [dest], "the report's listed REAL destinations are %r" % r.get("real_listed")
rows = {t["test_case_id"]: t.get("real") for t in r.get("tests") or []}
both = [k for k, v in rows.items() if v]
assert len(both) == 1 and rows[both[0]][0]["dest"] == dest, "REAL rows: %r" % rows
print("report: reached", dest, "and one test case's REAL row")
PY

echo "== 2b. keploy test against a running app (--base-path): no agent, nothing to read =="
# The app runs outside keploy and reaches its dependencies itself.
start dep; DEP_PORT=$PORT
export DEP_PORT
python3 app.py > app.log 2>&1 &
SERVER[app]=$!
app_up "${SERVER[app]}" || { echo "FAIL: the app did not come up"; FAIL=1; }
if ! sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$RECORD_BIN" test --base-path "http://127.0.0.1:${APP_PORT}" --disable-tele > test-base.log 2>&1; then
  echo "FAIL: keploy test --base-path with mock.real did not pass"
  tail -40 test-base.log
  FAIL=1
fi
stop app
stop dep

echo "== 3. keploy test, REAL stopped too =="
stop real
if replay test-down.log; then
  echo "FAIL: keploy test passed with the REAL destination down"
  FAIL=1
fi
report=$(ls -t keploy/reports/*/test-set-0-report.yaml 2>/dev/null | head -1)
grep -q "was never reached" "$report" ||
  { echo "FAIL: the report does not say the REAL destination was never reached"; grep -n "failure_reason" "$report"; FAIL=1; }

echo "== 4. without the switch the list is not read =="
start real; REAL_PORT=$PORT
start dep; DEP_PORT=$PORT
export REAL_PORT DEP_PORT
printf 'mock:\n  real:\n    - localhost:%s\n' "$REAL_PORT" > keploy.yml
sudo rm -rf keploy
record rec-off.log 0
mock_paths > mocks-off.txt; cat mocks-off.txt
expect mocks-off.txt "without the switch a call was not recorded" "/d" "/d" "/r"

echo "== 5. two test sets over one pooled connection to REAL (--keep-app-alive) =="
printf 'mock:\n  real:\n    - localhost:%s\n' "$REAL_PORT" > keploy.yml
sudo rm -rf keploy
record rec-p0.log 1 /pooled
record rec-p1.log 1 /pooled
# report SET: the latest report of SET.
report() { ls -t keploy/reports/*/"$1"-report.yaml 2>/dev/null | head -1; }
rm -f conns.real
if ! replay test-pooled.log --keep-app-alive; then
  echo "FAIL: keploy test failed over a pooled connection to REAL"
  tail -80 test-pooled.log
  FAIL=1
fi
# The leg is about one connection: a second would reach REAL by a dial.
[ "$(wc -l < conns.real 2>/dev/null)" = 1 ] ||
  { echo "FAIL: the app opened $(wc -l < conns.real 2>/dev/null) connection(s) to REAL, not one pooled"; FAIL=1; }
for set in test-set-0 test-set-1; do
  python3 - "$(report "$set")" "localhost:${REAL_PORT}" "$set" <<'PY' || FAIL=1
import sys, yaml
r = yaml.safe_load(open(sys.argv[1]))
dest, s = sys.argv[2], sys.argv[3]
assert dest in ((r.get("real") or {}).get("reached") or []), "%s: the report does not say %s was reached: %r" % (s, dest, r.get("real"))
print(s + ": reached", dest)
PY
done

echo "== 5b. REAL becomes a sink that never answers; the second set only wrote to it =="
sudo rm -rf keploy
export FIRE_READ=1
record rec-f0.log 1 /fire
record rec-f1.log 1 /fire
unset FIRE_READ
# The sink takes REAL's port, reads each request, says so on a fifo the app
# waits on, and never answers: the first set's dial connects (reached), the
# second set's request on that connection gets no answer.
cat > sink.py <<'PY'
import socket, sys, threading
port, fifo = int(sys.argv[1]), sys.argv[2]
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("127.0.0.1", port))
s.listen(16)
print("ready", port, flush=True)
def serve(c):
    buf = b""
    while True:
        b = c.recv(4096)
        if not b:
            return
        buf += b
        while b"\r\n\r\n" in buf:
            _, buf = buf.split(b"\r\n\r\n", 1)
            with open(fifo, "w") as f:
                f.write("took\n")
while True:
    c, _ = s.accept()
    threading.Thread(target=serve, args=(c,), daemon=True).start()
PY
stop real
rm -f "$WORK/sink.fifo" "$WORK/sink.ready" && mkfifo "$WORK/sink.fifo" "$WORK/sink.ready"
python3 sink.py "$REAL_PORT" "$WORK/sink.fifo" > "$WORK/sink.ready" 2>&1 &
SERVER[sink]=$!
exec 3<"$WORK/sink.ready"
IFS= read -r line <&3 || { echo "FAIL: the sink exited before it listened"; exit 1; }
exec 3<&-
[ "$line" = "ready $REAL_PORT" ] || { echo "FAIL: the sink said $line"; exit 1; }
export SINK_FIFO="$WORK/sink.fifo"
if replay test-fire.log --keep-app-alive; then
  echo "FAIL: keploy test passed with REAL answering nothing"
  FAIL=1
fi
unset SINK_FIFO
for set in test-set-0 test-set-1; do
  [ -s "$(report "$set")" ] || { echo "FAIL: no report for $set"; tail -60 test-fire.log; FAIL=1; }
done
grep -q "was used but never answered" "$(report test-set-1)" ||
  { echo "FAIL: test-set-1's report does not say REAL was used but never answered"; grep -n "failure_reason" "$(report test-set-1)"; FAIL=1; }
if grep -q "localhost:${REAL_PORT}.*never" "$(report test-set-0)"; then
  echo "FAIL: test-set-0, whose dial reached REAL, says it was not reached"
  FAIL=1
fi
stop sink

if [ "$FAIL" -ne 0 ]; then
  echo "record/test mock.real e2e failed"
  exit 1
fi
echo "record/test mock.real e2e passed"
