#!/usr/bin/env bash
# End-to-end guard for the UI-join annotation.
#
# WHAT THIS ASSERTS, through the real `keploy record` binary and the real
# on-disk store -- not a fake:
#
#   1. With KEPLOY_UI_CAPTURE_ID / KEPLOY_UI_SESSION_NONCE /
#      KEPLOY_APP_ORIGINS set, a recording that captures test cases writes
#      `io.keploy.ui-join/v1` into the test-set's config.yaml, carrying the
#      ingress port traffic was OBSERVED on and a t0/t1 span bounded at
#      both ends by moments this script watched for: the recorder's own
#      "Keploy agent is ready" log line, and the SIGINT it sent to stop.
#
#      NOT a clock this script stamps before launch. That design is
#      rejected further down, in the t0 oracle's own comment, because
#      process start and agent-ready are ~2s apart -- so a t0 taken from
#      the SESSION start, which is the substitution the oracle exists to
#      catch, would still clear a pre-launch floor. This header described
#      the rejected design as though it had shipped.
#   2. With those variables UNSET, the same recording writes no annotation.
#
# WHAT HALF 2 DOES AND DOES NOT PROVE: it does
# NOT prove the opt-in check is load-bearing. MEASURED -- forcing
# `uiJoinRequested()` to true leaves this script at exit 0, because with
# no capture id the annotation is independently refused as unstorable.
# What it proves is the observable a user cares about: an ordinary
# recording gets no annotation. The opt-in branch itself is pinned by the
# unit tests.
#
# WHY A SCRIPT AND NOT A UNIT TEST. The annotation is assembled from an
# observed port, a wall clock and three env vars, and written by the stop
# defer at the end of a real session. Every one of those is a seam a unit
# test has to fake, and the feature shipped with unit tests that all
# passed. This drives the binary a user runs.
#
# Exit codes: 0 both assertions hold; nonzero regression or infrastructure
# error (see stderr).

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$HERE/../../.." && pwd)"
APP_PORT=8099
CAPTURE_ID="cap-0000000000000001"
NONCE="nonce-0000000000000001"
ORIGIN="https://app.example.com"

WORK="$(mktemp -d)"
BIN="$WORK/keploy"
KPID=""

cleanup() {
  if [ -n "$KPID" ] && kill -0 "$KPID" 2>/dev/null; then
    kill -9 "$KPID" 2>/dev/null || true
  fi
  # Explicit PIDs only. `pkill -f` matches the pattern against THIS
  # script's own command line and has killed the invoking shell before.
  #
  # MATCHED ON $WORK, the run directory: the binary, the app and the
  # agent are all copied into or launched from it, so it is the one
  # string every process this script starts has in common. The script's
  # own command line does not contain it, so the invoking shell is safe.
  #
  # WHAT THIS KILL CANNOT DO, said rather than assumed: the agent runs
  # under `sudo` as root, and `kill -9` from the unprivileged runner is
  # EPERM. It is reaped instead by its own `--client-pid` watchdog when
  # the recorder exits. Verified by forcing a mid-run failure with the
  # agent live: no survivors, no held ports. If that watchdog ever goes,
  # this cleanup will look like it worked and will not have.
  #
  # The empty-$WORK guard is not paranoia: `grep -F ""` matches every
  # line, so an unset WORK would feed every PID on the box to kill -9.
  [ -n "$WORK" ] || return 0
  ps -eo pid,args \
    | grep -F "$WORK" \
    | grep -v grep \
    | awk '{print $1}' \
    | xargs -r kill -9 2>/dev/null || true
  rm -rf "$WORK"
}
trap cleanup EXIT

require_port_free() {
  # A previous run still holding 8099 is the failure mode that cost the
  # most time while this test was being written: curl succeeds against
  # the STALE app, the new one dies on bind, nothing is recorded, and the
  # annotation's (correct) refusal reads as a product bug.
  # `ss` MUST EXIST. With `2>/dev/null` swallowing "command not found",
  # a missing ss silently turned this guard off -- and a guard that is
  # quietly absent is worse than no guard, because the failure it
  # prevents (a stale listener answering curl) reads as a product bug.
  if ! command -v ss >/dev/null 2>&1; then
    echo "ss is not installed; this guard cannot run, and without it a" >&2
    echo "stale listener on $APP_PORT would be mistaken for this recording" >&2
    exit 1
  fi
  if ss -lnt 2>/dev/null | grep -q ":$APP_PORT "; then
    echo "port $APP_PORT is already in use; refusing to run so a stale" >&2
    echo "listener cannot be mistaken for this recording" >&2
    exit 1
  fi
}

# $1 = run directory, $2 = "join" | "nojoin"
record_once() {
  local dir="$1" mode="$2"
  mkdir -p "$dir"
  cp "$HERE/app.py" "$dir/app.py"
  require_port_free

  if [ "$mode" = "join" ]; then
    env KEPLOY_UI_CAPTURE_ID="$CAPTURE_ID" \
        KEPLOY_UI_SESSION_NONCE="$NONCE" \
        KEPLOY_APP_ORIGINS="$ORIGIN" \
        "$BIN" record --debug -c "python3 $dir/app.py" --path "$dir" \
        > "$dir/rec.log" 2>&1 &
  else
    env -u KEPLOY_UI_CAPTURE_ID -u KEPLOY_UI_SESSION_NONCE -u KEPLOY_APP_ORIGINS \
        "$BIN" record --debug -c "python3 $dir/app.py" --path "$dir" \
        > "$dir/rec.log" 2>&1 &
  fi
  KPID=$!

  # Wait for the app to answer rather than sleeping a guessed interval:
  # agent bring-up dominates and varies with the machine.
  local up=""
  for _ in $(seq 1 60); do
    if curl -s -m 2 "http://127.0.0.1:$APP_PORT/hello" >/dev/null 2>&1; then
      up=yes
      break
    fi
    sleep 1
  done
  [ -n "$up" ] || { echo "app never answered on $APP_PORT" >&2; sed -n '1,40p' "$dir/rec.log" >&2; exit 1; }

  for _ in 1 2 3; do
    curl -s -m 5 "http://127.0.0.1:$APP_PORT/hello" >/dev/null 2>&1 || true
    sleep 2
  done

  # WAIT FOR THE PERSIST, not for the request. A test case is written
  # some time after the exchange, and stopping in between yields a
  # recording with nothing in it -- which the annotation then correctly
  # declines to write, looking exactly like a bug.
  local persisted=0
  for _ in $(seq 1 45); do
    # A MISSING DIRECTORY IS A MEASUREMENT, NOT AN ERROR, and saying so
    # explicitly is the only form that survives `set -euo pipefail`.
    #
    # Both obvious spellings abort the script at this assignment --
    # silently, with no message and no diagnostic:
    #   ls   on a missing directory exits 2
    #   find on a missing path      exits 1
    # and under pipefail either status propagates out of the command
    # substitution, where `set -e` kills the run. MEASURED for both; the
    # `ls` -> `find` swap did NOT fix it, it only changed the number.
    #
    # And it fires on EVERY run, healthy or not: tests/ is created by the
    # FIRST InsertTestCase, which is the event this loop is waiting for,
    # so the directory is routinely absent on the first iteration of a
    # perfectly good recording. It also swallows the case the message
    # below was written for -- a recording that captured nothing has no
    # tests/ directory at all -- so the diagnostic was unreachable in
    # exactly its own scenario. A `|| true` would silence the abort too,
    # but it would silence a genuine find failure with it.
    local tests_dir="$dir/keploy/test-set-0/tests"
    if [ -d "$tests_dir" ]; then
      persisted=$(find "$tests_dir" -type f | wc -l)
    else
      persisted=0
    fi
    [ "$persisted" -ge 3 ] && break
    sleep 2
  done
  [ "$persisted" -ge 3 ] || { echo "only $persisted test cases persisted" >&2; exit 1; }

  # STAMPED BEFORE THE SIGNAL, and recorded for the span oracle below.
  # t1 is stamped in the recorder's stop defer, which by construction runs
  # AFTER this signal -- so this is a hard lower bound on t1 that the
  # script actually observed, rather than a duration guessed in advance.
  date +%s%3N > "$dir/sigint_ms"
  kill -INT "$KPID" 2>/dev/null || true
  for _ in $(seq 1 60); do
    kill -0 "$KPID" 2>/dev/null || break
    sleep 1
  done
  KPID=""
}

echo ">> building keploy"
( cd "$REPO" && go build -o "$BIN" . )

echo ">> recording WITH the UI-join variables"
record_once "$WORK/join" join

CFG="$WORK/join/keploy/test-set-0/config.yaml"
[ -f "$CFG" ] || { echo "no config.yaml written; the annotation never landed" >&2; exit 1; }

fail() { echo "ASSERTION FAILED: $1" >&2; echo "--- config.yaml ---" >&2; cat "$CFG" >&2; exit 1; }

grep -q "io.keploy.ui-join/v1" "$CFG" || fail "the namespaced key is absent"
grep -q "captureId: $CAPTURE_ID"   "$CFG" || fail "captureId did not round-trip"
grep -q "sessionNonce: $NONCE"     "$CFG" || fail "sessionNonce did not round-trip"
grep -q "$ORIGIN"                  "$CFG" || fail "appOrigins did not round-trip"
grep -q "canonicalKeySpec: canonical-key.v1" "$CFG" || fail "canonicalKeySpec is not the spec this build joins under"
grep -q "specVersion: 1"           "$CFG" || fail "specVersion is not 1"

# THE PORT IS PRESENT, and that is all this line can prove.
#
# It does NOT discriminate observed-from-configured, though it claimed
# to. The app is served on $APP_PORT and ingress arrives on $APP_PORT, so
# an intent-derived list and an observation-derived list produce the
# identical byte; there is no input in this test that separates them.
# Doing so needs a port that is configured but never receives traffic
# (assert it is ABSENT), or traffic on a port the config never mentions.
#
# The check is still worth having -- an annotation with no ports, or the
# wrong port, fails here -- it is just weaker than the sentence that used
# to sit above it. The rule the recorder actually relies on for
# observed-not-configured is unit-level: uiJoinPorts.observe() is fed
# models.TestCase.AppPort and nothing else.
grep -A2 "ingressPorts:" "$CFG" | grep -q -- "- $APP_PORT" \
  || fail "ingressPorts does not contain the port traffic actually arrived on ($APP_PORT)"

# A plausible epoch-ms span. Guards the seconds-vs-milliseconds mix-up
# that would place the capture decades away from its test-set, and the
# t1-before-t0 inversion.
# `|| true` ON BOTH, for the reason the persisted-count loop above spells
# out: a missing key makes `grep` exit 1, pipefail propagates it out of
# the substitution, and `set -e` kills the script with no output. The
# most obvious regression this file guards -- an annotation written
# without t0WallMs -- would therefore produce a SILENT exit rather than
# the diagnostic two lines below. Empty is a value the checks can report;
# a dead script is not.
T0=$(grep "t0WallMs:" "$CFG" | awk '{print $2}' || true)
T1=$(grep "t1WallMs:" "$CFG" | awk '{print $2}' || true)
[ -n "$T0" ] || fail "no t0WallMs key in the annotation: $(cat "$CFG")"
[ -n "$T1" ] || fail "no t1WallMs key in the annotation: $(cat "$CFG")"
[ "$T0" -gt 1000000000000 ] || fail "t0WallMs $T0 is not epoch MILLISECONDS"
[ "$T1" -gt "$T0" ]         || fail "t1WallMs $T1 does not follow t0WallMs $T0"

# t0 IS WHEN CAPTURE BEGAN, AND THE RECORDER SAYS WHEN THAT WAS.
#
# `> 1e12` and `t1 > t0` are satisfied by any plausible timestamp,
# including the SESSION start -- which is the substitution that defeats
# the whole reason t0WallMs exists, since a session start includes agent
# bring-up and the app's boot. MEASURED: replacing the capture clock with
# the session start left this script at exit 0.
#
# A stamp taken by this script before launch is too early to catch it:
# measured on a real run, process start and "agent is ready" are ~2s
# apart, so a session start still clears that floor. The recorder logs
# the moment it means, so the oracle is that line -- it gives the ~2s of
# discrimination a pre-launch stamp cannot.
# THE OFFSET MAY BE A LITERAL `Z`, and requiring [+-] made this job red
# on every CI run. Go formats with time.RFC3339Nano, whose Z07:00 layout
# emits `Z` at zero offset, and GitHub's ubuntu-latest runs in UTC.
# MEASURED with `TZ=UTC ./run.sh`: the grep matched nothing and the
# script failed with "the t0 oracle cannot run" -- while the annotation
# itself had landed correctly. `date -d` parses both spellings.
READY_TS=$(grep -m1 -o '[0-9-]\{10\}T[0-9:.]*\([+-][0-9:]*\|Z\)' \
  <(grep -m1 "Keploy agent is ready" "$WORK/join/rec.log") || true)
[ -n "$READY_TS" ] \
  || fail "no 'Keploy agent is ready' timestamp in rec.log; the t0 oracle cannot run"
READY_MS=$(date -d "$READY_TS" +%s%3N) \
  || fail "could not parse the agent-ready timestamp: $READY_TS"
# ONE SECOND OF SLACK, because the exact bound is the WRONG WAY ROUND.
#
# captureStart is stamped BEFORE the "agent is ready" line is logged --
# measured at 5-11us earlier, every time, over 8 instrumented runs. So
# `T0 >= READY_MS` asserts the opposite of what the code guarantees and
# passed only because both land in the same millisecond. A boundary
# falling inside that ~7us window fails a correct build and blames the
# product ("capture cannot have begun before the agent was ready" -- it
# can, and does).
#
# The substitution this oracle exists to catch moves t0 about 2s, so a
# 1s allowance still leaves ~1s of discrimination and removes the knife
# edge entirely.
READY_FLOOR_MS=$((READY_MS - 1000))
[ "$T0" -ge "$READY_FLOOR_MS" ] \
  || fail "t0WallMs $T0 is more than a second before the agent-ready moment $READY_MS ($READY_TS); capture cannot have begun that long before the agent was ready, so this t0 came from somewhere else"

# THE FLOOR ALONE LEAVES HALF THE MUTANTS ALIVE.
#
# `t0 >= READY-1s` and `t1 > t0` are both satisfied by a t0 stamped
# ANYWHERE LATER in the run -- moving `captureStart = time.Now()` into
# the stop defer, or having t0 read the clock at annotation time, keeps
# every assertion above green while the recorded span collapses to
# milliseconds. The span is the entire point of the field: a joiner uses
# it to decide which exchanges belong to this capture.
#
# So bound t0 from ABOVE as well, and bound t1 from BELOW -- each against
# a moment THIS SCRIPT OBSERVED, so no duration has to be guessed:
#   t0 <= agent-ready + 1s   (captureStart is stamped 5-11us BEFORE the
#                             agent-ready line; 1s mirrors the floor)
#   t1 >= the SIGINT we sent - 1s   (the stop defer runs after it)
# Together these force the span to cover [agent-ready, SIGINT], which is
# the recording, without this file asserting how long a recording takes.
SIGINT_MS=$(cat "$WORK/join/sigint_ms" 2>/dev/null || true)
[ -n "$SIGINT_MS" ] || fail "no SIGINT stamp was recorded; the span oracle cannot run"
READY_CEIL_MS=$((READY_MS + 1000))
[ "$T0" -le "$READY_CEIL_MS" ] \
  || fail "t0WallMs $T0 is more than a second AFTER the agent-ready moment $READY_MS ($READY_TS); capture is stamped as the agent becomes ready, so a later t0 means the clock was read somewhere else and the recorded span is short by $((T0 - READY_MS))ms"
STOP_FLOOR_MS=$((SIGINT_MS - 1000))
[ "$T1" -ge "$STOP_FLOOR_MS" ] \
  || fail "t1WallMs $T1 precedes the SIGINT this script sent at $SIGINT_MS; the stop time is stamped after the signal, so a t1 this early means it was read before the recording ended and the span is short by $((SIGINT_MS - T1))ms"

echo ">> recording WITHOUT them"
record_once "$WORK/nojoin" nojoin

NOCFG="$WORK/nojoin/keploy/test-set-0/config.yaml"
if [ -f "$NOCFG" ] && grep -q "io.keploy.ui-join/v1" "$NOCFG"; then
  echo "ASSERTION FAILED: an annotation was written with no UI-join variables set;" >&2
  echo "the presence check above would then pass on any build" >&2
  cat "$NOCFG" >&2
  exit 1
fi

echo "PASS: the annotation is written when asked for, and only then"
