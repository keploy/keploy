#!/usr/bin/env bash
# End-to-end guard: a `keploy mock record|replay` during which the agent's PID
# map overflows fails, as capture incomplete, instead of passing.
#
# The agent tracks the processes it may intercept in a BPF map,
# target_namespace_pids (10240 entries), which sys_enter_socket fills with
# every process that calls socket() in the agent's PID namespace (for a native
# agent, the whole host). Exited processes leave it, so it fills only with more
# LIVE such processes than it holds. Then the kernel refuses the next process
# a place, and that process's calls are not intercepted: in record they are
# not recorded, in replay they are not mocked and reach the real dependency.
# Both runs used to exit 0 with nothing logged. Now the BPF programs count the
# refusal, the agent logs it at WARN, and record keeps nothing and replay
# fails, both exiting non-zero.
#
# The test command calls the dependency, then, with OVERFLOW=1, holds more
# live processes than the map holds, each having called socket(), and calls
# the dependency again from a new process (curl), which the full map refuses.
# The live processes are clone(CLONE_VM) children of one small C program, so
# 10500 of them cost little memory; the test command waits for the program to
# say they all called socket(), not for a time. Runs without OVERFLOW check that
# a run the map does not overflow still records and replays cleanly.
# Uses the PR `build` binary for record and replay (mock_linux.yml).
set -uo pipefail
# This lane guards OSS keploy mock record and replay with the proven switch
# off (KEPLOY_PROVEN=0, the default on Windows and macOS): under it OSS keploy
# mock replay refuses to run without a build that judges each test, and
# record keeps nothing it cannot read per test. Unset, a Linux keploy runs the
# proven model.
export KEPLOY_PROVEN=0

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-keploy}"
WORK="$(mktemp -d)" || exit 1
cd "$WORK" || exit 1
PORT=18867
# target_namespace_pids holds 10240; 260 more than that, so that it overflows
# whatever else on the machine holds a place in it.
HOLD=10500
FAIL=0

fail() {
  echo "FAIL: $*"
  FAIL=1
}

# The dependency: answers every GET and appends its path to dep.log.
cat > dep.py <<'PY'
import http.server, socketserver, sys
PORT, LOG = int(sys.argv[1]), sys.argv[2]
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        with open(LOG, "a") as f:
            f.write(self.path + "\n")
        body = b"ok"
        self.send_response(200); self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def log_message(self, *a): pass
socketserver.TCPServer.allow_reuse_address = True
with socketserver.TCPServer(("127.0.0.1", PORT), H) as s: s.serve_forever()
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

# The test command.
cat > overflow.sh <<SH
#!/usr/bin/env bash
sleep 3  # let keploy warm up eBPF redirect so the FIRST call is intercepted.
# FIXME(#4472): this covers a real product race -- a dependency call made in
# the first ~0.1s of a mock session can go un-intercepted. The sleep hides it
# in CI; the product fix is tracked separately.
curl -fsS -o /dev/null -w "before-overflow %{http_code}\n" http://127.0.0.1:$PORT/before-overflow
if [ "\${OVERFLOW:-0}" = 1 ]; then
  # hold writes "held N" into the FIFO once all N have called socket(), or
  # closes it by exiting when it cannot: the read returns on either, however
  # long holding takes. The line goes to the log for the check below.
  rm -f held.fifo && mkfifo held.fifo
  ./hold $HOLD > held.fifo &
  HP=\$!
  trap 'kill \$HP 2>/dev/null' EXIT
  IFS= read -r held < held.fifo
  echo "\$held"
fi
curl -fsS -o /dev/null -w "after-overflow %{http_code}\n" http://127.0.0.1:$PORT/after-overflow
SH

: > dep.log
python3 dep.py "$PORT" "$WORK/dep.log" > dep.out 2>&1 &
DP=$!
trap 'kill "$DP" 2>/dev/null' EXIT
# Wait until the dependency answers, for as long as its process is alive; the
# bound is only a hang guard (and says what it waited for), not a readiness
# deadline.
UP=0
for _ in $(seq 1200); do
  curl -fsS -o /dev/null "http://127.0.0.1:$PORT/up" 2>/dev/null && { UP=1; break; }
  kill -0 "$DP" 2>/dev/null || { echo "FAIL: the dependency process on :$PORT exited before it listened"; cat dep.out; exit 1; }
  sleep 0.1
done
if [ "$UP" != 1 ]; then
  echo "FAIL: the dependency on :$PORT was alive but never answered (waited ~120s)"
  cat dep.out
  exit 1
fi

# keploy runs in a scope of its own with no task limit where systemd can make
# one: the cgroup a shell or runner service runs in may cap its tasks below
# the $HOLD processes the test command holds (a systemd default, TasksMax, is
# a share of the kernel's thread limit), and clone() then fails with EAGAIN.
SCOPE=()
if command -v systemd-run >/dev/null 2>&1 && sudo systemd-run --scope --quiet -p TasksMax=infinity true >/dev/null 2>&1; then
  SCOPE=(systemd-run --scope --quiet -p TasksMax=infinity)
fi

# keploy_mock <log> <record|replay> [VAR=value...]: one keploy mock run of
# overflow.sh with the given environment; prints keploy's exit code.
keploy_mock() {
  local log=$1 verb=$2 bin=$RECORD_BIN
  shift 2
  [ "$verb" = replay ] && bin=$REPLAY_BIN
  sudo -E "${SCOPE[@]}" env PATH="$PATH" "$@" timeout -k 30 300 "$bin" mock "$verb" -c "bash overflow.sh" --name overflow --disable-tele > "$log" 2>&1
  echo $?
}

# overflowed <log> <phase>: the test command held the processes, and both of
# its calls were answered (the second by whatever answered it).
overflowed() {
  grep -qx "before-overflow 200" "$1" || fail "$2: the call before the overflow did not get a 200"
  grep -qx "held $HOLD" "$1" || fail "$2: the test command did not hold $HOLD live processes: $(grep -m1 -i 'held\|clone\|mmap' "$1")"
  grep -qx "after-overflow 200" "$1" || fail "$2: the call after the overflow did not get a 200"
}

# warned <log> <phase>: the agent said, at WARN, that the kernel refused a
# process a place in target_namespace_pids. Prints what it and keploy said.
warned() {
  grep -q "refused a process a place in the agent's PID map target_namespace_pids" "$1" ||
    fail "$2: the agent did not warn that target_namespace_pids refused a process"
  echo "--- $2: what keploy said about the PID map"
  grep -E "PID map|could not track|recording is incomplete" "$1" | cut -c1-400
}

echo "== record, the PID map overflowing: must fail and keep nothing =="
rc=$(keploy_mock rec-overflow.log record OVERFLOW=1)
echo "record (overflow): keploy exited $rc"
overflowed rec-overflow.log "record (overflow)"
[ "$rc" != 0 ] || fail "record (overflow): keploy exited 0 although the call to /after-overflow was not recorded"
grep -q "the recording was not kept: the agent could not track every process" rec-overflow.log ||
  fail "record (overflow): keploy did not say the recording was not kept because a process was not tracked"
warned rec-overflow.log "record (overflow)"
if sudo test -e keploy/overflow/mocks.yaml; then
  fail "record (overflow): a recording missing the call to /after-overflow was kept"
fi

echo "== record, no overflow: must record both calls =="
rc=$(keploy_mock rec.log record)
[ "$rc" = 0 ] || fail "record: keploy exited $rc, want 0"
for p in before-overflow after-overflow; do
  sudo grep -q "url: /$p\$" keploy/overflow/mocks.yaml 2>/dev/null || fail "record: the call to /$p was not recorded"
done

: > dep.log
echo "== replay, the PID map overflowing: must fail as capture incomplete =="
rc=$(keploy_mock rep-overflow.log replay OVERFLOW=1)
echo "replay (overflow): keploy exited $rc; the dependency saw: $(tr '\n' ' ' < dep.log)"
overflowed rep-overflow.log "replay (overflow)"
[ "$rc" != 0 ] || fail "replay (overflow): keploy exited 0 although the call to /after-overflow went to the real dependency: $(tr '\n' ' ' < dep.log)"
grep -q "replay failed: the agent could not track every process the test command ran" rep-overflow.log ||
  fail "replay (overflow): keploy did not say the replay failed because a process was not tracked"
sudo grep -qx "failedBy: capture_incomplete" keploy/overflow/last-replay.yaml 2>/dev/null ||
  fail "replay (overflow): the receipt does not say capture_incomplete: $(sudo grep -h failedBy keploy/overflow/last-replay.yaml 2>/dev/null)"
warned rep-overflow.log "replay (overflow)"

: > dep.log
echo "== replay, no overflow: must pass from mocks =="
rc=$(keploy_mock rep.log replay)
[ "$rc" = 0 ] || fail "replay: keploy exited $rc, want 0"
if [ -s dep.log ]; then
  fail "replay: these calls went to the real dependency instead of a mock: $(tr '\n' ' ' < dep.log)"
fi
if grep -q "PID map" rep.log; then
  fail "replay: keploy reported a PID map refusal on a run that held no extra processes: $(grep -m1 'PID map' rep.log)"
fi

if [ "$FAIL" -ne 0 ]; then
  for l in rec-overflow rec rep-overflow rep; do
    echo "--- the last 40 lines of $l.log ---"
    tail -n 40 "$l.log"
  done
  echo "MOCK PID-MAP OVERFLOW E2E: FAILED; logs and mocks are kept in $WORK"
  exit 1
fi
sudo rm -rf "$WORK"
echo "MOCK PID-MAP OVERFLOW E2E: PASSED"
