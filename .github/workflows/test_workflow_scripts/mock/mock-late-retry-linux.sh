#!/usr/bin/env bash
# A call keploy refuses is made once, in the window of the test that made it.
#
# keploy answers an outgoing HTTP call no mock matches itself. One process is
# both the app and its test driver here, as a Playwright suite's one app
# process serves every test. Its HTTP client retries what client retry
# policies retry (500, 502, 503, 504, 408, 429 and a dropped connection), up
# to 4 attempts, as the AWS SDKs and .NET's resilience handler do; a retry
# waits until a later test has begun, the way a backoff outlasts a test that
# timed out (ordered by the begin, never by the clock). keploy answered a
# miss 502 Bad Gateway, so the client made the refused call again in a later
# test's window; it answers 400 Bad Request, which no such policy repeats.
#   request  test a's call changed since the record (a real change: keploy
#            must refuse it, and test a must fail); test a ends once its
#            first attempt is answered. Test b makes no dependency call.
#   consumer a consumer thread started at boot handles one job at a time,
#            each job's call made with the same client. Test c1's job call
#            changed since the record; c2 and c3 each queue a job whose call
#            did not change and wait for it to be handled.
# Checked: the refused call is answered once, 400, and nothing is made in
# b's window; c2's and c3's calls are each answered 200 at their first
# attempt (a repeat of c1's call held the consumer before). Under the proven
# switch (SWITCH=1, a build that judges each test: keploy enterprise, with
# LR_PROJECT=1 and KEPLOY_EXTRA_FLAGS=--local) also the verdicts: a and c1
# refused, b, c2 and c3 passed. Without it a test has no verdict of its own.
#
# Env: RECORD_BIN, REPLAY_BIN (the keploy binaries under test); SWITCH
# (KEPLOY_PROVEN's value, empty by default).
set -uo pipefail
MOCK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-keploy}"
SWITCH="${SWITCH:-}"
read -r -a EXTRA <<< "${KEPLOY_EXTRA_FLAGS:-}"
WORK="$(mktemp -d)"
cd "$WORK" || exit 1
PORT=18879
FAIL=0
cp "$MOCK_DIR/keploy_v1.py" keploy_v1.py || { echo "FAIL: no keploy_v1.py beside this lane"; exit 1; }

cat > dep.py <<'PY'
import http.server, socketserver, sys
PORT = int(sys.argv[1])
class H(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def do_POST(self):
        n = int(self.headers.get("Content-Length") or 0)
        got = self.rfile.read(n)
        body = b'{"ok":true,"got":' + got + b'}'
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def log_message(self, *a): pass
socketserver.TCPServer.allow_reuse_address = True
with socketserver.ThreadingTCPServer(("127.0.0.1", PORT), H) as s:
    print("ready", flush=True)
    s.serve_forever()
PY

cat > suite.py <<'PY'
import json, os, queue, sys, threading, urllib.error, urllib.request
import keploy_v1 as k
SHAPE, MODE, PORT = sys.argv[1], os.environ.get("LR_MODE") or sys.argv[2], sys.argv[3]
URL = "http://127.0.0.1:%s" % PORT
MAX = 4
began = threading.Condition()
begins = {"n": 0}
attempts = []          # (job, status) per attempt, in order

def call(job, path, body):
    seen = begins["n"]
    for i in range(MAX):
        if i:
            with began:   # the backoff: until a later test has begun
                if not began.wait_for(lambda: begins["n"] > seen, timeout=20):
                    attempts.append((job, "gave-up-waiting")); return None
                seen = begins["n"]
        req = urllib.request.Request(URL + path, data=json.dumps(body).encode(),
                                     headers={"Content-Type": "application/json"}, method="POST")
        try:
            with urllib.request.urlopen(req, timeout=10) as r:
                attempts.append((job, r.status)); return r.status
        except urllib.error.HTTPError as e:
            attempts.append((job, e.code))
            if e.code not in (408, 429, 500, 502, 503, 504):
                return e.code
        except (urllib.error.URLError, ConnectionError, OSError) as e:
            attempts.append((job, "conn:" + type(e).__name__))
    return None

def begin(name):
    r = k.begin(name, "suite.py", pid=PID)
    print("BEGIN %s -> %r" % (name, r), flush=True)
    with began:
        begins["n"] += 1; began.notify_all()
    return None

verdicts = {}
def end(name):
    r = k.end(name, "passed", file="suite.py", pid=PID)
    verdicts[name] = "ok" if r is None else "refused: " + r.replace("\n", " ")[:300]

# The call of the first job changed since the record (its path), a real change.
first_path = "/put" if MODE == "record" else "/put-changed"
mode = k.claim(".", os.getcwd())
print("CLAIM=" + mode, flush=True)
# Without the proven switch the scope API as it was: this process's pid on
# each begin and end, inside a suite mark (as mock-stateful-linux.sh).
PID = os.getpid() if mode == k.LEGACY else None
def mark(edge):
    status, raw = k.post("/agent/scope/" + edge, {"name": "suite", "pid": os.getpid(), "suite": True})
    assert status == 200, "the suite mark's %s answered %d: %r" % (edge, status, raw[:512])
if PID:
    mark("begin")

if SHAPE == "request":
    assert begin("a") is None
    first = threading.Event()
    t = threading.Thread(target=lambda: (call("a", first_path, {"lead": "a"}), first.set()))
    n0 = len(attempts); t.start()
    while len(attempts) == n0 and t.is_alive():
        t.join(0.05)   # test a ends once its first attempt is answered
    end("a")
    assert begin("b") is None
    t.join(30)         # b makes no call; the request's retries, if any, run now
    end("b")
else:
    jobs = queue.Queue(); done = {}
    def consume():
        while True:
            job, path = jobs.get()
            call(job, path, {"lead": job})
            done[job].set()
    threading.Thread(target=consume, daemon=True).start()
    for name, path in (("c1", first_path), ("c2", "/put"), ("c3", "/put")):
        assert begin(name) is None
        done[name] = threading.Event(); n0 = len(attempts)
        jobs.put((name, path))
        if name == "c1":
            while len(attempts) == n0:
                done[name].wait(0.05)   # c1 ends after its job's first attempt
        else:
            done[name].wait(20)         # c2, c3 wait for their own job
        end(name)
if PID:
    mark("end")
print("ATTEMPTS=" + json.dumps(attempts), flush=True)
print("VERDICTS=" + json.dumps(verdicts, sort_keys=True), flush=True)
print("RUN_DONE", flush=True)
PY

DP=""
start_dep() {
  python3 dep.py "$PORT" > dep.log 2>&1 & DP=$!
  until grep -qx ready dep.log 2>/dev/null; do
    kill -0 "$DP" 2>/dev/null || { echo "FAIL: the dependency exited before it listened"; cat dep.log; exit 1; }
    sleep 0.1
  done
}
stop_dep() { [ -n "$DP" ] && { kill "$DP" 2>/dev/null; wait "$DP" 2>/dev/null; }; DP=""; }
trap stop_dep EXIT

for shape in request consumer; do
  # An enterprise build records and replays a keploy project: one folder per
  # shape, its keploy.yml naming the command (LR_PROJECT=1).
  mkdir -p "$WORK/$shape" && cd "$WORK/$shape" || exit 1
  cp ../keploy_v1.py ../suite.py ../dep.py .
  if [ "${LR_PROJECT:-0}" = 1 ]; then
    printf 'mock:\n  name: lr-%s\n  local: true\n  command: python3 suite.py %s env %s\n' "$shape" "$shape" "$PORT" > keploy.yml
  fi
  echo "== $shape: record with the dependency up =="
  start_dep
  # shellcheck disable=SC2024 # the log is this user's file, not root's
  sudo -E env PATH="$PATH" LR_MODE=record KEPLOY_PROVEN="$SWITCH" "$RECORD_BIN" mock record -c "python3 suite.py $shape record $PORT" --name "lr-$shape" --disable-tele "${EXTRA[@]}" > "rec-$shape.log" 2>&1
  RC=$?
  stop_dep
  grep -E '^(CLAIM|ATTEMPTS|VERDICTS)=' "rec-$shape.log"
  [ "$RC" -eq 0 ] || { echo "FAIL: $shape record exited $RC"; tail -30 "rec-$shape.log"; FAIL=1; continue; }
  echo "== $shape: replay with the dependency down =="
  # shellcheck disable=SC2024 # the log is this user's file, not root's
  sudo -E env PATH="$PATH" LR_MODE=replay KEPLOY_PROVEN="$SWITCH" "$REPLAY_BIN" mock replay -c "python3 suite.py $shape replay $PORT" --name "lr-$shape" --disable-tele "${EXTRA[@]}" > "rep-$shape.log" 2>&1
  echo "replay exit $?"
  grep -E '^(CLAIM|ATTEMPTS|VERDICTS)=' "rep-$shape.log"
  sed 's/\x1b\[[0-9;]*m//g' "rep-$shape.log" | grep -E 'no recorded mock matched|test is counted|ran: ' | cut -c1-400 | head -20
  grep -q '^VERDICTS=' "rep-$shape.log" || { echo "-- the suite did not finish; the replay's log:"; sed 's/\x1b\[[0-9;]*m//g' "rep-$shape.log" | grep -v -E 'DEBUG' | cut -c1-400 | tail -60; }
  python3 - "$shape" "rep-$shape.log" "$SWITCH" <<'PY' || FAIL=1
import json, sys
shape, log, switch = sys.argv[1], open(sys.argv[2]).read(), sys.argv[3]
lines = [l for l in log.splitlines() if l.startswith(("ATTEMPTS=", "VERDICTS="))]
if len(lines) != 2:
    print("FAIL: %s: the suite did not finish" % shape); sys.exit(1)
get = lambda key: json.loads(next(l for l in lines if l.startswith(key + "="))[len(key) + 1:])
v, a = get("VERDICTS"), get("ATTEMPTS")
first = "a" if shape == "request" else "c1"
later = ["b"] if shape == "request" else ["c2", "c3"]
bad = []
mine = [st for job, st in a if job == first]
if mine != [400]:
    bad.append("%s's refused call was answered %s, want once, 400" % (first, mine))
for t in later:
    got = [st for job, st in a if job == t]
    if got != ([] if t == "b" else [200]):
        bad.append("%s's calls were answered %s" % (t, got))
if switch:
    if v.get(first, "ok") == "ok":
        bad.append("%s passed though its call changed" % first)
    bad += ["%s was not passed: %s" % (t, v.get(t)) for t in later if v.get(t) != "ok"]
print(("FAIL: %s: " % shape + "; ".join(bad)) if bad else "OK: %s" % shape)
sys.exit(1 if bad else 0)
PY
done
[ "$FAIL" -eq 0 ] && echo "LANE PASS" || echo "LANE FAIL"
exit "$FAIL"
