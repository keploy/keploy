#!/usr/bin/env bash
# End-to-end guard: a `keploy test` during which the agent's PID map overflows
# fails its test set as capture incomplete, and leaves the recording as it was.
#
# The agent tracks the processes it may intercept in a BPF map,
# target_namespace_pids (10240 entries), one entry for each live process that
# called socket() in the agent's PID namespace (for a native agent, the whole
# host). With the map full the kernel refuses the next process a place, and
# that process's calls are not intercepted: they reach the real dependency.
# While the dependency is up its tests pass all the same, and the mocks
# recorded for those calls are consumed by nobody. So `keploy test` used to
# exit 0 with the test set PASSED, and with --remove-unused-mocks it deleted
# those mocks from the recording as unused; a first run also wrote a
# mappings.yaml whose tests lacked them. Now the BPF programs count the
# refusal, the test set is FAILED saying why, and nothing is pruned or mapped
# from a run that did not see every call.
#
# The app answers GET /order/<item> by starting a NEW process that asks the
# dependency for the price, so that the test's dependency call is made by a
# process the full map refuses. /order/item1 it asks for itself: the app has
# its place in the map from before it filled, so that one call is still
# mocked, and the run has something to write a mappings.yaml from. The live
# processes that fill the map are clone(CLONE_VM) children of one small C
# program, so 10500 of them cost little memory. Nothing here waits for a time:
# the app starts that program only once its own socket exists, keploy sends
# the first test once the app's /healthz says the map is full, and the
# recording is stopped with Ctrl+C once every request has had its answer.
#   1. no overflow: PASSED, every recorded mock still there, the dependency
#      not called;
#   2. overflow: FAILED as capture incomplete, exit non-zero, the prune
#      skipped for that reason, no mock deleted, no mappings.yaml written;
#   3. overflow under KEPLOY_PROVEN=1 with --keep-app-alive and a second test
#      set: one application serves both sets, so a process the full map
#      refused in the first lives on into the second and taints its tests
#      too. The second set is item1's test alone, the call the app makes
#      itself from the place it took before the map filled, so its own calls
#      lose nothing: it fails as capture incomplete for the first set's loss,
#      and its reason says so, and why (the application kept alive).
#
# Nothing in 2 depends on WHICH process the full map refused. The holder's own
# 10500 processes are more than the map's 10240 places, so the kernel refuses
# at least 260 of them during the test set whatever else runs: that alone
# fails it. But whether a new process the app starts is refused too depends on
# whether a place is free when it calls socket(), and on a busy machine a
# place frees whenever a process that took one before the map filled exits.
# So the calls that reach the dependency are printed, not asserted. For the
# prune to have something to delete whichever calls were mocked, the app
# answers the last test (item$N, outside the startup window) itself when the
# map overflows, without calling the dependency: its mock is consumed by
# nobody, deterministically, and only the capture-incomplete guard keeps it.
# Uses the PR `build` binary for record and replay (mock_linux.yml).
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-keploy}"
WORK="$(mktemp -d)" || exit 1
cd "$WORK" || exit 1
APP=18873
DEP=18874
# target_namespace_pids holds 10240; 260 more than that, so that it overflows
# whatever else on the machine holds a place in it.
HOLD=10500
# More tests than the startup window (models.StartupMockTestCaseWindow, 5),
# whose mocks no prune deletes.
N=8
FAIL=0

fail() {
  echo "FAIL: $*"
  FAIL=1
}

# The dependency: answers every GET with a price and appends its path to dep.log.
cat > dep.py <<'PY'
import http.server, json, socketserver, sys
PORT, LOG = int(sys.argv[1]), sys.argv[2]
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        with open(LOG, "a") as f:
            f.write(self.path + "\n")
        item = self.path.rsplit("/", 1)[-1]
        body = json.dumps({"item": item, "price": len(item)}).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def log_message(self, *a): pass
socketserver.TCPServer.allow_reuse_address = True
with socketserver.TCPServer(("127.0.0.1", PORT), H) as s: s.serve_forever()
PY

# fetch.py: the new process that makes the dependency call.
cat > fetch.py <<'PY'
import sys, urllib.request
with urllib.request.urlopen(sys.argv[1], timeout=5) as r:
    sys.stdout.write(r.read().decode())
PY

# The app: GET /order/<item> has a new process ask the dependency for the
# price; for item1 it asks itself. With OVERFLOW=1 it answers the last item
# (argv[4]) from what it knows, the price being the item's length, so that
# its mock goes unused in that run whatever the map did. With OVERFLOW=1 it starts the process
# holder once its own server socket exists, so it has its place in the map
# before the holder's processes fill it, and GET /healthz answers 503 until the
# holder says they all called socket() (writing that line to hold.out), 200
# from then on: `keploy test --health-url` sends the first test once the map is
# full, however long that takes, never before.
cat > app.py <<'PY'
import http.server, json, os, socketserver, subprocess, sys, threading, urllib.request
PORT, DEP, HOLD, LOCAL = int(sys.argv[1]), int(sys.argv[2]), sys.argv[3], sys.argv[4]
OVERFLOW = os.environ.get("OVERFLOW") == "1"
held = threading.Event()
if not OVERFLOW:
    held.set()
def wait_held(p):
    line = p.stdout.readline()  # "held N", or "" when hold exited without
    with open("hold.out", "w") as f:
        f.write(line)
    if line.startswith("held "):
        held.set()
        return
    # The holder died without holding (a task-limit EAGAIN, say). /healthz would
    # never go green, so keploy would wait out its whole --health-poll window on
    # an app that will never be ready. Exit now so keploy sees the app go and
    # fails the run at once, with the holder's output in hold.out to say why.
    os._exit(1)
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/healthz":
            code = 200 if held.is_set() else 503
            self.send_response(code); self.send_header("Content-Length", "0"); self.end_headers()
            return
        item = self.path.rsplit("/", 1)[-1]
        url = "http://127.0.0.1:%d/price/%s" % (DEP, item)
        try:
            if OVERFLOW and item == LOCAL:
                out = json.dumps({"item": item, "price": len(item)})
            elif item == "item1":
                with urllib.request.urlopen(url, timeout=5) as r:
                    out = r.read().decode()
            else:
                out = subprocess.run([sys.executable, "fetch.py", url],
                                     capture_output=True, text=True, timeout=10, check=True).stdout
            code, obj = 200, {"item": item, "total": json.loads(out)["price"] * 2}
        except Exception:
            code, obj = 502, {"error": "the dependency did not answer"}
        body = json.dumps(obj).encode()
        self.send_response(code); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def log_message(self, *a): pass
socketserver.TCPServer.allow_reuse_address = True
with socketserver.TCPServer(("127.0.0.1", PORT), H) as s:
    if not held.is_set():
        # From this, the main thread, which lives as long as the app: hold
        # dies with the thread that started it (PR_SET_PDEATHSIG).
        p = subprocess.Popen(["./hold", HOLD], stdout=subprocess.PIPE, text=True)
        threading.Thread(target=wait_held, args=(p,), daemon=True).start()
    s.serve_forever()
PY

# hold N: N live processes that have each called socket(). Each is a
# clone(CLONE_VM) child sharing hold's memory, so each costs a kernel task and
# a stack page. Prints "held N" once all have, then waits; its children die
# with it (PR_SET_PDEATHSIG).
cat > hold.c <<'C'
#define _GNU_SOURCE
#include <sched.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <sys/mman.h>
#include <sys/prctl.h>
#include <sys/socket.h>
#include <unistd.h>

static int held;
static pid_t parent;

static int child(void *arg) {
    (void)arg;
    prctl(PR_SET_PDEATHSIG, SIGKILL);
    if (getppid() != parent)
        _exit(0);
    socket(AF_INET, SOCK_DGRAM, 0);
    __atomic_add_fetch(&held, 1, __ATOMIC_SEQ_CST);
    for (;;)
        pause();
}

int main(int argc, char **argv) {
    int n = argc > 1 ? atoi(argv[1]) : 0;
    size_t stack = 32 * 1024;
    prctl(PR_SET_PDEATHSIG, SIGKILL); /* it, and so its children, end with the app */
    parent = getpid();
    for (int i = 0; i < n; i++) {
        char *s = mmap(NULL, stack, PROT_READ | PROT_WRITE, MAP_PRIVATE | MAP_ANONYMOUS | MAP_STACK, -1, 0);
        if (s == MAP_FAILED) { perror("mmap"); return 1; }
        if (clone(child, s + stack, CLONE_VM | SIGCHLD, NULL) == -1) { perror("clone"); return 1; }
    }
    while (__atomic_load_n(&held, __ATOMIC_SEQ_CST) < n)
        usleep(10000);
    printf("held %d\n", n);
    fflush(stdout);
    for (;;)
        pause();
}
C
if ! cc -O2 -o hold hold.c; then
  echo "FAIL: could not compile the process holder (needs a C compiler)"
  exit 1
fi

# The app command. With OVERFLOW=1 (from keploy's environment) the app starts
# the held processes itself (app.py).
cat > app.sh <<SH
#!/usr/bin/env bash
exec python3 app.py $APP $DEP $HOLD item$N
SH

: > dep.log
python3 dep.py "$DEP" "$WORK/dep.log" > dep.out 2>&1 &
DP=$!
# On exit also the app keploy started and its process holder, which outlive a
# keploy that `timeout` killed and would keep the PID map full for every later
# step: matched by their own command lines, anchored, never by "keploy".
reap_app() {
  sudo pkill -KILL -f "^python3 app\.py $APP " 2>/dev/null
  sudo pkill -KILL -f "^\./hold $HOLD\$" 2>/dev/null
}
trap 'kill "$DP" 2>/dev/null; reap_app' EXIT
# Wait until the dependency answers, for as long as its process is alive; a
# fixed deadline is not a readiness signal (and must not be a verdict). The
# bound is only a hang guard, and it says what it waited for.
UP=0
for _ in $(seq 1200); do
  curl -fsS -o /dev/null "http://127.0.0.1:$DEP/up" 2>/dev/null && { UP=1; break; }
  kill -0 "$DP" 2>/dev/null || { echo "FAIL: the dependency process on :$DEP exited before it listened"; cat dep.out; exit 1; }
  sleep 0.1
done
if [ "$UP" != 1 ]; then
  echo "FAIL: the dependency on :$DEP was alive but never answered (waited ~120s)"
  cat dep.out
  exit 1
fi

# keploy runs in a scope of its own with no task limit where systemd can make
# one: the cgroup a shell or runner service runs in may cap its tasks below
# the $HOLD processes the app command holds, and clone() then fails with EAGAIN.
SCOPE=()
if command -v systemd-run >/dev/null 2>&1 && sudo systemd-run --scope --quiet -p TasksMax=infinity true >/dev/null 2>&1; then
  SCOPE=(systemd-run --scope --quiet -p TasksMax=infinity)
fi

echo "== record $N tests, one dependency call each =="
# No --record-timer: the recording is stopped as a user stops one, with
# Ctrl+C, once every request has had its answer. timeout only ends a keploy
# that hangs.
sudo -E "${SCOPE[@]}" env PATH="$PATH" timeout -k 10 600 "$RECORD_BIN" record -c "bash app.sh" \
  --generateGithubActions=false --disable-tele > record.log 2>&1 &
KP=$!
# Until the app listens (a connect, which records no test): keploy starts it
# once its agent is up. The bound is only for an agent that never comes up.
UP=0
for _ in $(seq 3600); do
  (exec 3<>"/dev/tcp/127.0.0.1/$APP") 2>/dev/null && { UP=1; break; }
  [ -d "/proc/$KP" ] || break
  sleep 0.1
done
[ "$UP" = 1 ] || fail "record: the app never listened on :$APP"
for i in $(seq $N); do
  curl -fsS -o /dev/null "http://127.0.0.1:$APP/order/item$i" || fail "record: GET /order/item$i failed"
done
# Ctrl+C to keploy itself, timeout's child, whose command line starts with
# the binary (the agent it starts says "agent", not "record").
sudo pkill -INT -f "^$RECORD_BIN record -c bash app\.sh" || fail "record: found no keploy record to stop"
wait "$KP"
echo "record: keploy exited $?"

# mocks <file>: how many mocks it holds; 0 for a file that is not there.
mocks() {
  local n
  n=$(sudo grep -c '^name: mock-' "$1" 2>/dev/null)
  echo "${n:-0}"
}
REC=$(mocks keploy/test-set-0/mocks.yaml)
TESTS=$(sudo sh -c 'ls keploy/test-set-0/tests 2>/dev/null | wc -l')
echo "recorded $TESTS tests and $REC mocks"
if [ "$TESTS" != "$N" ] || [ "$REC" -lt "$N" ]; then
  echo "FAIL: recorded $TESTS tests and $REC mocks, want $N tests and at least $N mocks (one dependency call a test)"
  tail -n 40 record.log
  exit 1
fi
: > dep.log
sudo cp -a keploy keploy.rec

# ktest <log> [VAR=value...]: one replay, with the prune on, of the recording
# as it was recorded; prints keploy's exit code. The dependency stays UP: a
# call that is not intercepted is answered, and its test passes.
ktest() {
  local log=$1
  shift
  sudo rm -rf keploy hold.out && sudo cp -a keploy.rec keploy
  : > dep.log
  # --health-url, not --delay: the first test goes once the app says the map
  # is full (app.py), not after a time that a loaded runner may not keep to.
  # --health-poll-timeout and the outer `timeout` are both 600s on purpose: on
  # the poll ceiling the gate would warn and fire anyway (before the map is
  # full), so that fallback must stay unreachable -- the outer timeout, the
  # same 600s, is the real hang guard, and app.py exits the app if the holder
  # dies, so a stuck /healthz never burns the window on a dead app.
  sudo -E "${SCOPE[@]}" env PATH="$PATH" "$@" timeout -k 30 600 "$REPLAY_BIN" test -c "bash app.sh" \
    --health-url "http://127.0.0.1:$APP/healthz" --health-poll-timeout 600s \
    --remove-unused-mocks --generateGithubActions=false --disable-tele > "$log" 2>&1
  echo $?
}
latest_report() { sudo sh -c "ls -1dt keploy/reports/test-run-*/${1:-test-set-0}-report.yaml 2>/dev/null | head -n1"; }

# make_kept_set: test-set-1, item1's test alone with its mock, beside the
# recording's test-set-0 (ktest copied it back).
make_kept_set() {
  local t
  t=$(sudo grep -l 'url: http://.*/order/item1$' keploy/test-set-0/tests/*.yaml 2>/dev/null | head -n1)
  [ -n "$t" ] || { echo "FAIL: found no recorded test of /order/item1"; return 1; }
  sudo mkdir -p keploy/test-set-1/tests
  sudo cp "$t" keploy/test-set-1/tests/
  # mocks.yaml is one YAML document per mock: keep item1's.
  sudo python3 - keploy/test-set-0/mocks.yaml keploy/test-set-1/mocks.yaml <<'PY'
import sys
docs = open(sys.argv[1]).read().split("\n---\n")
keep = [d for d in docs if "/price/item1" in d]
open(sys.argv[2], "w").write("\n---\n".join(keep) + "\n")
PY
  [ "$(mocks keploy/test-set-1/mocks.yaml)" = 1 ] || { echo "FAIL: test-set-1 holds $(mocks keploy/test-set-1/mocks.yaml) mocks, want item1's one"; return 1; }
}

# ktest_kept <log> [VAR=value...]: one replay, under KEPLOY_PROVEN=1, of
# test-set-0 and test-set-1 (make_kept_set) with --keep-app-alive; prints
# keploy's exit code. Under the switch the first test waits for the
# application's readiness however long it takes (no --health-poll-timeout:
# the switch refuses one), so `timeout` is the only bound, a hang guard.
ktest_kept() {
  local log=$1
  shift
  sudo rm -rf keploy hold.out && sudo cp -a keploy.rec keploy
  make_kept_set || { echo 99; return; }
  : > dep.log
  sudo -E "${SCOPE[@]}" env PATH="$PATH" KEPLOY_PROVEN=1 "$@" timeout -k 30 900 "$REPLAY_BIN" test -c "bash app.sh" --keep-app-alive \
    --health-url "http://127.0.0.1:$APP/healthz" --generateGithubActions=false --disable-tele > "$log" 2>&1
  echo $?
}

echo "== keploy test --remove-unused-mocks, no overflow: must pass and keep every mock =="
rc=$(ktest test.log)
rep=$(latest_report)
echo "test: keploy exited $rc; $(sudo grep -m1 '^status:' "$rep" 2>/dev/null); mocks $(mocks keploy/test-set-0/mocks.yaml) of $REC; the dependency saw $(wc -l < dep.log) call(s)"
[ "$rc" = 0 ] || fail "test: keploy exited $rc, want 0"
sudo grep -q '^status: PASSED' "$rep" 2>/dev/null || fail "test: the test set's report is not PASSED"
[ "$(mocks keploy/test-set-0/mocks.yaml)" = "$REC" ] ||
  fail "test: mocks.yaml holds $(mocks keploy/test-set-0/mocks.yaml) mocks after a run that used them all, want the $REC recorded"
[ ! -s dep.log ] || fail "test: these calls went to the real dependency instead of a mock: $(tr '\n' ' ' < dep.log)"
if grep -q "PID map\|capture incomplete" test.log; then
  fail "test: keploy reported a PID map refusal on a run that held no extra processes: $(grep -m1 'PID map\|capture incomplete' test.log | cut -c1-300)"
fi

echo "== keploy test --remove-unused-mocks, the PID map overflowing: must fail and leave the recording as it was =="
rc=$(ktest test-overflow.log OVERFLOW=1)
rep=$(latest_report)
AFTER=$(mocks keploy/test-set-0/mocks.yaml)
echo "test (overflow): keploy exited $rc; $(sudo grep -m1 '^status:' "$rep" 2>/dev/null); $(sudo grep -E '^(success|failure):' "$rep" 2>/dev/null | tr '\n' ' '); mocks $AFTER of $REC; the dependency saw $(wc -l < dep.log) call(s)"
grep -qx "held $HOLD" hold.out 2>/dev/null ||
  fail "test (overflow): the app command did not hold $HOLD live processes: $(head -n 2 hold.out 2>/dev/null)"
# Which of the new processes the map refused is the machine's to decide (see
# the top): printed, not asserted. The app's own call is mocked either way,
# its place having been taken before the map filled, and kept while it lives.
echo "test (overflow): the calls the full map let reach the real dependency: $(tr '\n' ' ' < dep.log)"
if grep -qx "/price/item1" dep.log; then
  fail "test (overflow): the app's own call, /price/item1, went to the real dependency although the app had its place in the map"
fi
[ "$rc" != 0 ] || fail "test (overflow): keploy exited 0 although the agent could not track $HOLD live processes"
sudo grep -q '^status: FAILED' "$rep" 2>/dev/null || fail "test (overflow): the test set's report is not FAILED"
sudo grep -q '^failure_reason: .*capture incomplete' "$rep" 2>/dev/null ||
  fail "test (overflow): the report's failure_reason does not say capture incomplete: $(sudo grep -m1 '^failure_reason' "$rep" 2>/dev/null | cut -c1-200)"
grep -q "refused a process a place in the agent's PID map target_namespace_pids" test-overflow.log ||
  fail "test (overflow): the agent did not warn that target_namespace_pids refused a process"
# item$N's mock went unused in this run (app.py): unguarded, the prune deletes
# it, and the mock of every call that reached the dependency.
[ "$AFTER" = "$REC" ] ||
  fail "test (overflow): mocks.yaml holds $AFTER mocks, the recording had $REC: the prune deleted mocks after a run that did not see every call"
grep -E 'skipping mock pruning.*"reason": ?"capture_incomplete"' test-overflow.log >/dev/null ||
  fail "test (overflow): keploy did not say it skipped the prune because the capture was incomplete: $(grep -m1 'skipping mock pruning' test-overflow.log | cut -c1-300)"
if sudo test -e keploy/test-set-0/mappings.yaml; then
  fail "test (overflow): a mappings.yaml was written from a run that did not see every call"
fi
echo "--- test (overflow): what keploy said"
grep -E "PID map|could not track|skipping mock pruning|not writing mappings" test-overflow.log | cut -c1-400

echo "== keploy test --keep-app-alive, KEPLOY_PROVEN=1, two test sets, the PID map overflowing in the first: the second must fail as capture incomplete too, saying why =="
rc=$(ktest_kept test-kept.log OVERFLOW=1)
rep0=$(latest_report test-set-0)
rep1=$(latest_report test-set-1)
echo "test (kept): keploy exited $rc; test-set-0 $(sudo grep -m1 '^status:' "$rep0" 2>/dev/null); test-set-1 $(sudo grep -m1 '^status:' "$rep1" 2>/dev/null); the dependency saw: $(tr '\n' ' ' < dep.log)"
grep -qx "held $HOLD" hold.out 2>/dev/null ||
  fail "test (kept): the app command did not hold $HOLD live processes: $(head -n 2 hold.out 2>/dev/null)"
[ "$rc" != 0 ] || fail "test (kept): keploy exited 0 although the agent could not track $HOLD live processes"
sudo grep -q '^status: FAILED' "$rep0" 2>/dev/null || fail "test (kept): test-set-0's report is not FAILED"
if grep -qx "/price/item1" dep.log; then
  fail "test (kept): the app's own call, /price/item1, went to the real dependency although the app had its place in the map"
fi
# test-set-1's own call was mocked; what fails it is the process the first
# set lost, which runs on with the application.
sudo grep -q '^status: FAILED' "$rep1" 2>/dev/null ||
  fail "test (kept): test-set-1's report is not FAILED although the application it ran against lost a process in test-set-0: $(sudo grep -m1 '^status:' "$rep1" 2>/dev/null)"
sudo grep -q 'kind: pid_map_refused$' "$rep1" 2>/dev/null ||
  fail "test (kept): test-set-1's test was not charged pid_map_refused"
for want in "capture incomplete" "before this test-set began" "kept alive across test-sets (--keep-app-alive)"; do
  sudo grep -q "^failure_reason: .*$want" "$rep1" 2>/dev/null ||
    fail "test (kept): test-set-1's failure_reason does not say \"$want\": $(sudo grep -m1 '^failure_reason' "$rep1" 2>/dev/null | cut -c1-400)"
done
grep -q "a process of the application's that keploy could not capture before this test-set began runs on in it" test-kept.log ||
  fail "test (kept): keploy did not say, at ERROR, that a process lost before test-set-1 runs on in it"
echo "--- test (kept): what keploy said of test-set-1"
sudo grep -m1 '^failure_reason' "$rep1" 2>/dev/null | cut -c1-600

if [ "$FAIL" -ne 0 ]; then
  for l in record test test-overflow test-kept; do
    echo "--- the last 40 lines of $l.log ---"
    tail -n 40 "$l.log"
  done
  echo "TEST-MODE PID-MAP OVERFLOW E2E: FAILED; logs and the recording are kept in $WORK"
  exit 1
fi
sudo rm -rf "$WORK"
echo "TEST-MODE PID-MAP OVERFLOW E2E: PASSED"
