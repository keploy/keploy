#!/usr/bin/env bash
# E2E: the exit code of `keploy record` says how the recording ended (#4772),
# with the real binary, a real agent and real signals:
#   the app exits 0 on its own            -> 0
#   the app is stopped by SIGTERM         -> 0   (docker stop, kill: a stop)
#   the app is stopped by SIGINT          -> 0   (kill -INT on the app: a stop)
#   the app exits 42                      -> 42  (the app's own code, verbatim,
#                                                 as `keploy mock` mirrors its
#                                                 runner's: utils/exitcodes.go)
#   SIGKILL kills the app                 -> 137 (128 + SIGKILL: not a stop)
#   --record-timer ends the recording     -> 0
#   keploy is sent SIGINT while recording -> 0
# A stop is never a failure, and a recording that failed never exits 0. Before
# #4772 an app that died exited 0 too. A recording whose agent cannot start is
# mock-agent-cannot-start-linux.sh's.
#
# Env: RECORD_BIN (the keploy binary under test).
set -uo pipefail
# Job control: without it a background job of a script starts with SIGINT
# ignored, and sudo, finding it ignored, never relays the interrupt this lane
# sends it to keploy.
set -m
# shellcheck source=../sudo_env.sh
source "$(dirname "${BASH_SOURCE[0]}")/../sudo_env.sh" || exit 1
# shellcheck source=../hang_guard.sh
source "$(dirname "${BASH_SOURCE[0]}")/../hang_guard.sh" || exit 1

BIN="${RECORD_BIN:-keploy}"
BIN="$(command -v "$BIN" || echo "$BIN")"
WORK="$(mktemp -d)"
FAIL=0

fail() {
  echo "FAIL: $*"
  FAIL=1
}

# start <case> <keploy record args...>: starts `keploy record` as root, in a
# directory of its own, logging to $WORK/<case>.log, and sets PID to sudo's
# (sudo relays a signal sent to it to keploy, and exits with keploy's code,
# which expect reads by wait: not timeout(1)'s, whose code for a relayed
# interrupt is not keploy's on Ubuntu 26.04; see hang_guard.sh). The hang
# guard only bounds the run: when it fires it says so in the log, and expect
# fails the case on that line, whatever the exit code (a hung run's agent and
# app, each in a process group of its own, are left to the runner's
# teardown).
start() {
  local name=$1
  shift
  mkdir -p "$WORK/$name"
  cd "$WORK/$name" || exit 1
  hang_start -s KILL 120 sudo env PATH="$PATH" "${SUDO_ENV[@]}" "$BIN" record "$@" --disable-tele > "$WORK/$name.log" 2>&1
  PID=$HANG_PID
  cd - >/dev/null || exit 1
}

# expect <case> <exit code> <what its log has to say>: waits for the case's
# recording to end and checks how it did.
expect() {
  local name=$1 want=$2 says=$3 got
  hang_wait "$PID"
  got=$?
  echo "--- $name: exited $got, want $want ---"
  local bad=0
  if [ "$got" != "$want" ]; then
    fail "$name: keploy record exited $got, want $want"
    bad=1
  fi
  if ! grep -aqF "$says" "$WORK/$name.log"; then
    fail "$name: its log does not say \"$says\""
    bad=1
  fi
  if grep -aqF "$HANG_LINE" "$WORK/$name.log"; then
    fail "$name: keploy record hung, and the hang guard killed it"
    bad=1
  fi
  if [ "$want" = 0 ] && grep -aqF "failed to record" "$WORK/$name.log"; then
    fail "$name: a recording that ended as it should says it failed"
    bad=1
  fi
  [ "$bad" = 0 ] || tail -n 40 "$WORK/$name.log"
}

# Each case's log check is a line only that ending prints. keploy runs
# the command under sh -c, so the app that kills itself is that shell: the
# process keploy waits on dies of the signal whichever sh the runner has.
start app-exits-0 -c "sh -c 'exit 0'"
expect app-exits-0 0 '"error": "app stopped"'

start app-exits-42 -c "sh -c 'exit 42'"
expect app-exits-42 42 "exit status 42"

start app-terminated -c "kill -TERM \$\$"
expect app-terminated 0 '"app_exit_code": 143'

start app-interrupted -c "kill -INT \$\$"
expect app-interrupted 0 '"app_exit_code": 130'

start app-killed -c "kill -KILL \$\$"
expect app-killed 137 "signal: killed"

start record-timer --record-timer 5s -c "sleep 600"
expect record-timer 0 "Time up! Stopping keploy"

# The app says when it runs, which is once the recording is under way: the
# interrupt is sent then, not after a guessed delay.
MARK="$WORK/sigint/app-running"
start sigint -c "sh -c 'touch $MARK; exec sleep 600'"
until [ -e "$MARK" ]; do
  hang_running "$PID" || break
  sleep 0.2
done
if [ -e "$MARK" ]; then
  sudo kill -INT "$PID"
else
  fail "sigint: the recording ended before its app ran"
fi
expect sigint 0 "Signal received: interrupt"

sudo rm -rf "$WORK"
if [ "$FAIL" -ne 0 ]; then
  echo "record exit-codes e2e FAILED"
  exit 1
fi
echo "record exit-codes e2e passed"
