#!/usr/bin/env bash
# End-to-end guard: `keploy mock record|replay` keeps intercepting a test
# command's calls after more processes have come and gone than the agent's
# PID map holds.
#
# The agent tracks the processes it may intercept in a BPF map,
# target_namespace_pids (10240 entries), which sys_enter_socket fills with
# every process that calls socket() (for a native agent, every process on the
# host). Nothing took exited processes out, so once 10240 processes had called
# socket() in one session (shell loops, curl per request, test-runner workers,
# anything else on the host) it was full, and from then on every NEW process's
# calls went straight to the real dependency: in record nothing was recorded, in replay the call
# went live and passed, and nothing said so. The map now loses a process when
# its last thread exits (k_process_exit in keploy/ebpf).
#
# The test command calls the dependency, runs more short-lived processes than
# the map holds, each calling socket(), and calls the dependency again from a
# new process. Record must keep both calls; replay must serve both from mocks
# while the dependency, left UP, sees nothing: a call that escapes
# interception gets a real answer, so only the dependency's own log shows it.
# Uses the PR `build` binary for record and replay (mock_linux.yml).
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
sudo mkdir -p /root/.keploy && echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml >/dev/null || true

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-keploy}"
WORK="$(mktemp -d)"
cd "$WORK"
PORT=18866
# target_namespace_pids holds 10240; 2048 more than that.
CHURN=12288
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

# The test command. The churn is bash subshells opening a UDP socket to the
# discard port: socket() and connect(), which nothing redirects, and nothing
# sent. Each curl is a new process.
cat > churn.sh <<SH
#!/usr/bin/env bash
sleep 3  # let keploy warm up eBPF redirect so the FIRST call is intercepted
curl -fsS -o /dev/null -w "before-churn %{http_code}\n" http://127.0.0.1:$PORT/before-churn
for ((i = 0; i < $CHURN; i++)); do ( : > /dev/udp/127.0.0.1/9 ) 2>/dev/null; done
echo "churned $CHURN processes"
curl -fsS -o /dev/null -w "after-churn %{http_code}\n" http://127.0.0.1:$PORT/after-churn
SH

: > dep.log
python3 dep.py "$PORT" "$WORK/dep.log" > dep.out 2>&1 &
DP=$!
trap 'kill "$DP" 2>/dev/null' EXIT
UP=0
for _ in $(seq 100); do curl -fsS -o /dev/null "http://127.0.0.1:$PORT/up" 2>/dev/null && { UP=1; break; }; sleep 0.1; done
if [ "$UP" != 1 ]; then
  echo "FAIL: the dependency on :$PORT did not come up within 10s"
  cat dep.out
  exit 1
fi
: > dep.log

# answered <log>: both calls of churn.sh got a 200, after the churn ran.
answered() {
  grep -qx "before-churn 200" "$1" || fail "$2: the call before the churn did not get a 200"
  grep -qx "churned $CHURN processes" "$1" || fail "$2: the churn did not finish"
  grep -qx "after-churn 200" "$1" || fail "$2: the call after the churn did not get a 200"
}

echo "== record: one call, $CHURN short-lived processes, another call =="
sudo -E env PATH="$PATH" timeout -k 30 300 "$RECORD_BIN" mock record -c "bash churn.sh" --name churn --disable-tele > rec.log 2>&1
rc=$?
[ "$rc" = 0 ] || fail "record: keploy exited $rc, want 0"
answered rec.log record
for p in before-churn after-churn; do
  sudo grep -q "url: /$p\$" keploy/churn/mocks.yaml 2>/dev/null ||
    fail "record: the call to /$p was not recorded"
done

: > dep.log
echo "== replay: the same command, the dependency up and logging =="
sudo -E env PATH="$PATH" timeout -k 30 300 "$REPLAY_BIN" mock replay -c "bash churn.sh" --name churn --disable-tele > rep.log 2>&1
rc=$?
[ "$rc" = 0 ] || fail "replay: keploy exited $rc, want 0"
answered rep.log replay
if [ -s dep.log ]; then
  fail "replay: these calls went to the real dependency instead of a mock: $(tr '\n' ' ' < dep.log)"
fi

if [ "$FAIL" -ne 0 ]; then
  for l in rec rep; do
    echo "--- the last 40 lines of $l.log ---"
    tail -n 40 "$l.log"
  done
  echo "MOCK PROCESS-CHURN E2E: FAILED; logs and mocks are kept in $WORK"
  exit 1
fi
sudo rm -rf "$WORK"
echo "MOCK PROCESS-CHURN E2E: PASSED"
