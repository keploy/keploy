#!/usr/bin/env bash
# E2E: `keploy mock record` ended by a hang-up stops as it does on SIGTERM.
#
# The VS Code extension's Stop button ends a run by terminating its task:
# VS Code sends the process on the task's terminal SIGHUP (node-pty's kill())
# and then disposes of the terminal, and the kernel hangs up on its session
# leader, a second SIGHUP. keploy gets both when the shell ran it in its own
# place, as `zsh -c` does. Closing the window of an interactive shell sends
# its job two as well. keploy handled only SIGINT and SIGTERM, so the first
# SIGHUP killed it where it stood: the test command it had started ran on, and
# none of the cleanup keploy defers ran.
#
# Each case runs `keploy mock record` as the session leader of a terminal of
# its own (forkpty, as node-pty starts a task) whose test command calls a
# dependency on loopback and then runs until it is stopped. Once that call is
# made, the harness:
#   term       sends keploy SIGTERM and keeps the terminal open: what a
#              hang-up must match
#   hup-gap    sends it SIGHUP, waits 100ms and closes the terminal. The gap
#              makes the kernel's SIGHUP arrive while keploy is stopping, which
#              a handler that stopped listening after the first SIGHUP would
#              die of
#   hup-nogap  the same with no gap, so the kernel's SIGHUP lands at once
#   hup-user   hup-gap for keploy run as this user rather than root, as the
#              extension runs it, with sudo credentials it can use without a
#              prompt: keploy starts its agent with `sudo -n`
#   hup-pty    hup-user for a user sudo would ask for a password: keploy
#              starts its agent under sudo on a terminal of its own, and stops
#              it by closing that terminal if asking it to stop fails
# and then wants keploy to exit by itself within 60s, with SIGTERM's exit
# code, leaving no keploy or test-command process behind and no
# keploy-logs.txt.
#
# Every case runs with HOME in the case's own directory, in a network
# namespace of its own (unshare -n) with only loopback, as in
# mock-offline-linux.sh, and a mount namespace of its own (unshare -m) in
# which root's home is an empty tmpfs, for the agents hup-user and hup-pty
# start through sudo. keploy tells those two apart by whether `sudo -n -v`
# succeeds, so each has sudoers.d to itself: the user may run anything
# without a password, and `sudo -v` asks for none (hup-user) or always asks
# for one (hup-pty), so no password is ever typed.
#
# Run it as a user with passwordless sudo, not as root.
#
# Env: RECORD_BIN (the keploy binary under test).
set -uo pipefail

BIN="${RECORD_BIN:-keploy}"
BIN="$(command -v "$BIN" || echo "$BIN")"
# Resolved: keploy starts its agent at the path its own binary resolves to.
WORK="$(cd "$(mktemp -d)" && pwd -P)"
FAIL=0
PORT=18999

fail() {
  echo "FAIL: $*"
  FAIL=1
}

if [ "$(id -u)" = 0 ]; then
  echo "FAIL: run this as a user with passwordless sudo, not as root: hup-user and hup-pty run keploy as that user"
  exit 1
fi

# A path of its own, so what is left running can be told from anything else
# on the machine.
mkdir -p "$WORK/bin"
cp "$BIN" "$WORK/bin/keploy" || { echo "FAIL: no keploy binary at $BIN"; exit 1; }

cat > "$WORK/depserver.py" <<'PY'
import http.server, json, socketserver, sys
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = json.dumps({"path": self.path}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a): pass
socketserver.TCPServer.allow_reuse_address = True
with socketserver.TCPServer((sys.argv[1], int(sys.argv[2])), H) as s:
    s.serve_forever()
PY

# The test command: leaves its pid in <dir>/test.pid, calls the dependency,
# says so in <dir>/started, and runs until it is stopped.
cat > "$WORK/slowtest.py" <<'PY'
import os, sys, time, urllib.request
d, url = sys.argv[1], sys.argv[2]
with open(os.path.join(d, "test.pid"), "w") as f:
    f.write(str(os.getpid()))
with urllib.request.urlopen(url + "/users?id=1", timeout=5) as r:
    r.read()
open(os.path.join(d, "started"), "w").close()
time.sleep(600)
PY

# hangup.py <hup|term> <gap seconds> <marker> <output> <command...> runs the
# command as the session leader of a new terminal and, once <marker> exists,
# ends it as the case says (see the top of this file), reading the terminal
# until it is closed. Exits with the command's exit code, 128+N for a death by
# signal N, 124 if it ran on 60s after it was told to stop, or 125 if it had
# exited by itself before a hup case closed its terminal, so that the case
# never sent it the terminal's SIGHUP.
cat > "$WORK/hangup.py" <<'PY'
import os, pty, select, signal, sys, time

mode, gap, marker, output, cmd = sys.argv[1], float(sys.argv[2]), sys.argv[3], sys.argv[4], sys.argv[5:]
pid, term = pty.fork()
if pid == 0:
    os.execvp(cmd[0], cmd)
out = open(output, "wb")


def read(timeout):
    if not select.select([term], [], [], timeout)[0]:
        return True
    try:
        data = os.read(term, 65536)
    except OSError:
        return False
    out.write(data)
    return bool(data)


deadline = time.time() + 120
while not os.path.exists(marker) and time.time() < deadline and read(0.2):
    pass
if not os.path.exists(marker):
    print("hangup.py: the test command never started", file=sys.stderr)
os.kill(pid, signal.SIGHUP if mode == "hup" else signal.SIGTERM)
if mode == "hup":
    until = time.time() + gap
    while time.time() < until and read(max(0, until - time.time())):
        pass
    done, status = os.waitpid(pid, os.WNOHANG)
    if done:
        sys.exit(128 + os.WTERMSIG(status) if os.WIFSIGNALED(status) else 125)
    os.close(term)
deadline = time.time() + 60
while True:
    done, status = os.waitpid(pid, os.WNOHANG)
    if done:
        break
    if time.time() > deadline:
        os.kill(pid, signal.SIGKILL)
        os.waitpid(pid, 0)
        sys.exit(124)
    if mode == "hup" or not read(0.2):
        time.sleep(0.2)
out.close()
sys.exit(os.WEXITSTATUS(status) if os.WIFEXITED(status) else 128 + os.WTERMSIG(status))
PY

# Runs inside the namespaces as root: $1 is the case, $2 the mode, $3 the
# gap, $4, if set, the user to run keploy as, and $5, if set, anything else
# keploy is given. Leaves keploy's exit code in <case>/exit and what it
# printed in <case>/out.
cat > "$WORK/inside.sh" <<'SH'
#!/bin/sh
CASE=$1 MODE=$2 GAP=$3 AS=$4 EXTRA=$5
D="$WORK/$CASE"
mkdir -p "$D/home" "$D/app"
export HOME="$D/home"
ip link set lo up || exit 1
mount -t tmpfs -o mode=0700 tmpfs /root || exit 1
python3 "$WORK/depserver.py" 127.0.0.1 "$PORT" &
DEP=$!
trap 'kill $DEP 2>/dev/null' EXIT
cd "$D/app" || exit 1
set --
if [ -n "$AS" ]; then
  chown -R "$AS" "$D"
  # keploy runs `sudo -n -v` to learn whether sudo would ask it for a
  # password.
  VERIFYPW=never
  [ "$CASE" = hup-pty ] && VERIFYPW=always
  mount -t tmpfs -o mode=0755 tmpfs /etc/sudoers.d || exit 1
  printf '%s ALL=(ALL:ALL) NOPASSWD: ALL\nDefaults:%s verifypw=%s\n' "$AS" "$AS" "$VERIFYPW" > /etc/sudoers.d/keploy-hangup
  chmod 0440 /etc/sudoers.d/keploy-hangup
  # Without what sudo tells the program it runs: keploy reads SUDO_USER as
  # the user who ran it under sudo, and it was not run under sudo.
  set -- sudo -u "$AS" env -u SUDO_USER -u SUDO_UID -u SUDO_GID -u SUDO_COMMAND HOME="$HOME" PATH="$PATH"
fi
# shellcheck disable=SC2086 # EXTRA is flags, split on purpose
"$@" python3 "$WORK/hangup.py" "$MODE" "$GAP" "$D/started" "$D/out" \
  "$WORK/bin/keploy" mock record -c "python3 $WORK/slowtest.py $D http://127.0.0.1:$PORT" \
  --name "$CASE" --disable-tele $EXTRA
echo $? > "$D/exit"
exit 0
SH
chmod +x "$WORK/inside.sh"

# left_running <case>: what this case started that still runs, one ps line
# each: keploy (the CLI, or its agent, bare or under sudo) and the test
# command. ps, not kill -0, which cannot see root's processes from this user.
# The cases run one at a time, and whatever one leaves is killed before the
# next starts, so everything found is this case's.
left_running() {
  local test_pid
  test_pid=$(cat "$WORK/$1/test.pid" 2>/dev/null)
  {
    pgrep -f "^(sudo (-n )?)?$WORK/bin/keploy " || true
    [ -n "$test_pid" ] && echo "$test_pid"
  } | sort -u | while read -r p; do ps -o pid=,args= -p "$p"; done
}

# kill_left <case>: kills whatever left_running finds.
kill_left() {
  sudo pkill -KILL -f "^(sudo (-n )?)?$WORK/bin/keploy " 2>/dev/null
  [ -f "$WORK/$1/test.pid" ] && sudo kill -KILL "$(cat "$WORK/$1/test.pid")" 2>/dev/null
}

# run_case <case> <hup|term> <gap> [user [flags]]: runs the case and checks
# what it leaves; its exit code is left in EXIT_CODE.
run_case() {
  local name=$1 mode=$2 gap=$3 user=${4:-} extra=${5:-} left
  echo "--- $name ---"
  sudo env PATH="$PATH" WORK="$WORK" PORT="$PORT" \
    unshare -n -m --propagation private "$WORK/inside.sh" "$name" "$mode" "$gap" "$user" "$extra" ||
    fail "$name: the namespaces themselves failed"
  EXIT_CODE=$(cat "$WORK/$name/exit" 2>/dev/null)
  echo "keploy exited ${EXIT_CODE:-nothing}"
  if [ ! -e "$WORK/$name/started" ]; then
    fail "$name: the test command never called the dependency"
  fi
  case "$EXIT_CODE" in
    124) fail "$name: keploy was still running 60s after it was told to stop" ;;
    125) fail "$name: keploy had stopped before its terminal closed, so the case never sent it a second SIGHUP" ;;
    129) fail "$name: keploy died of a SIGHUP instead of stopping" ;;
  esac
  # keploy returns once it has asked its agent to stop: give what it stopped
  # a moment to be gone.
  for _ in $(seq 1 30); do
    left=$(left_running "$name")
    [ -z "$left" ] && break
    sleep 0.5
  done
  if [ -n "$left" ]; then
    fail "$name: left running after keploy exited:"
    echo "$left"
    kill_left "$name"
  fi
  if [ -e "$WORK/$name/app/keploy-logs.txt" ]; then
    fail "$name: keploy left its keploy-logs.txt behind"
  fi
}

# hup_case <case> <gap> [user [flags]]: run_case for a hang-up, whose exit
# code must be the one SIGTERM gave.
hup_case() {
  local name=$1
  run_case "$name" hup "${@:2}"
  [ "$EXIT_CODE" = "$TERM_EXIT" ] || fail "$name: keploy exited ${EXIT_CODE:-nothing} on a hang-up, and $TERM_EXIT on SIGTERM"
}

run_case term term 0
TERM_EXIT=$EXIT_CODE
[ "$TERM_EXIT" = 0 ] || fail "term: keploy exited ${TERM_EXIT:-nothing} on SIGTERM, want 0 (a stopped recording is not a failure)"
hup_case hup-gap 0.1
hup_case hup-nogap 0
# --debug: only its debug log says which way keploy started its agent.
hup_case hup-user 0.1 "$(id -un)" --debug
if ! grep -aq "Starting native agent with args" "$WORK/hup-user/out" ||
  grep -aq "Starting native agent with PTY" "$WORK/hup-user/out"; then
  fail "hup-user: keploy did not start its agent with sudo -n, so the case did not check what it is for"
fi
hup_case hup-pty 0.1 "$(id -un)" --debug
grep -aq "Starting native agent with PTY" "$WORK/hup-pty/out" ||
  fail "hup-pty: keploy did not start its agent on a terminal of its own, so the case did not check what it is for"

if [ "$FAIL" -ne 0 ]; then
  for c in term hup-gap hup-nogap hup-user hup-pty; do
    echo "--- $c: the last 40 lines keploy printed ---"
    tail -n 40 "$WORK/$c/out" 2>/dev/null
  done
  echo "mock hang-up e2e FAILED; its output is kept in $WORK"
  exit 1
fi
sudo rm -rf "$WORK"
echo "mock hang-up e2e passed"
