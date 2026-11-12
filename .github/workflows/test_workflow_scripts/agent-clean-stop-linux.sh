#!/usr/bin/env bash
# E2E: a clean stop of the keploy agent forces nothing.
#
# keploy waits for a stopping agent while its teardown makes progress, and
# kills it -- saying "Stopping the keploy agent by force: ..." at WARN -- only
# once it has stopped making any, or has detached everything and still not
# exited. The agent's report that it has detached everything was followed by
# beats while the process exited, and a race-detector build (the PR `build`
# binary) spends a second on its way out waiting for races at exit (GORACE's
# atexit_sleep_ms). keploy read a beat there as an agent that had not exited,
# and killed it: most clean record and replay stops of a race build logged the
# WARN, which hid the real ones, and the kill cut short the race detector's
# own checks at exit.
#
# Records an app that serves a fixed page, twice: once ended by
# --record-timer, once by Ctrl+C (SIGINT); then replays the recording. Each of
# the three runs must have watched its agent's teardown (it logged that it
# waits for the agent to finish shutting down: the agent's exit takes a race
# build a second, past the stop's first read of its progress), and none may
# log that it stopped the agent by force, or a data race. The whole is done
# ROUNDS times: one stop of the race build was forced about two times in three.
#
# It uses port 18941 on 127.0.0.1, and refuses to start while something else
# listens there.
#
# Env: RECORD_BIN and REPLAY_BIN, the PR `build` binary for both
# (mock_linux.yml).

# keploy runs under sudo, which hands it only the variables its command line
# names (sudo-rs, Ubuntu 26.04's sudo, ignores -E): SUDO_ENV names the
# switches a workflow sets for keploy, each only while it is set.
# shellcheck source=sudo_env.sh
source "$(dirname "${BASH_SOURCE[0]}")/sudo_env.sh" || exit 1
# shellcheck source=hang_guard.sh
source "$(dirname "${BASH_SOURCE[0]}")/hang_guard.sh" || exit 1

set -uo pipefail

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-$RECORD_BIN}"
ROUNDS="${ROUNDS:-2}"
PORT=18941
# BOUND: the seconds a run may take before the hang guard ends it. Each takes
# about 15; a stop that waits on an agent that never exits takes them all.
BOUND=120
WORK="$(mktemp -d)"
cd "$WORK" || exit 1
FAIL=0
fail() {
  echo "::error::$*"
  FAIL=$((FAIL + 1))
}

if (exec 3<>"/dev/tcp/127.0.0.1/$PORT") 2>/dev/null; then
  echo "::error::something already listens on 127.0.0.1:$PORT"
  exit 1
fi

mkdir site && echo '<html><body>clean stop</body></html>' > site/index.html
APP="python3 -m http.server $PORT --bind 127.0.0.1 --directory $WORK/site"

# traffic sends a request every half second until it is killed.
traffic() {
  while :; do
    curl -s -o /dev/null "http://127.0.0.1:$PORT/index.html"
    sleep 0.5
  done
}

# check LOG RC: the run exited 0 by itself, watched its agent's teardown,
# and forced nothing. A stop that never ends is ended by the hang guard
# (HANG_FIRED), which a stop waiting on an agent that never exits would be.
check() {
  local log=$1 rc=$2
  if [ "$HANG_FIRED" != 0 ]; then
    fail "$log: keploy still ran after ${BOUND}s; the hang guard ended it"
  elif [ "$rc" != 0 ]; then
    fail "$log: keploy exited $rc"
  fi
  if ! grep -q "Waiting for the keploy agent to finish shutting down" "$log"; then
    fail "$log: the stop never read the agent's teardown progress, so it checked nothing"
  fi
  if grep -q "Stopping the keploy agent by force" "$log"; then
    fail "$log: a clean stop forced the agent: $(grep -m1 "Stopping the keploy agent by force" "$log")"
  fi
  if grep -q "WARNING: DATA RACE" "$log"; then
    fail "$log: a data race"
  fi
}

for round in $(seq 1 "$ROUNDS"); do
  dir="$WORK/round-$round"
  mkdir -p "$dir" && cd "$dir" || exit 1
  echo "== round $round: record ended by --record-timer =="
  traffic & tp=$!
  # shellcheck disable=SC2024 # the log is this user's, so the shell redirects
  hang_start -k 10 "$BOUND" sudo env PATH="$PATH" "${SUDO_ENV[@]}" "$RECORD_BIN" record -c "$APP" \
    --record-timer 10s --generateGithubActions=false --disable-tele >timer.log 2>&1 </dev/null
  hang_wait "$HANG_PID"; rc=$?
  echo "exit $rc"
  kill "$tp" 2>/dev/null; wait "$tp" 2>/dev/null
  check timer.log "$rc"

  echo "== round $round: record ended by Ctrl+C =="
  traffic & tp=$!
  # shellcheck disable=SC2024 # the log is this user's, so the shell redirects
  hang_start -k 10 "$BOUND" sudo env PATH="$PATH" "${SUDO_ENV[@]}" "$RECORD_BIN" record -c "$APP" \
    --generateGithubActions=false --disable-tele >int.log 2>&1 </dev/null
  rec=$HANG_PID # sudo: it passes the signal on to keploy
  sleep 10
  sudo kill -INT "$rec" 2>/dev/null
  hang_wait "$rec"; rc=$?
  echo "exit $rc"
  kill "$tp" 2>/dev/null; wait "$tp" 2>/dev/null
  check int.log "$rc"

  echo "== round $round: replay =="
  # shellcheck disable=SC2024 # the log is this user's, so the shell redirects
  hang_start -k 10 "$BOUND" sudo env PATH="$PATH" "${SUDO_ENV[@]}" "$REPLAY_BIN" test -c "$APP" --delay 5 \
    --generateGithubActions=false --disable-tele >test.log 2>&1 </dev/null
  hang_wait "$HANG_PID"; rc=$?
  echo "exit $rc"
  check test.log "$rc"
  cd "$WORK" || exit 1
done

if [ "$FAIL" -ne 0 ]; then
  echo "$FAIL check(s) failed; the logs are in $WORK"
  exit 1
fi
echo "every check passed"
cd / && sudo rm -rf "$WORK"
