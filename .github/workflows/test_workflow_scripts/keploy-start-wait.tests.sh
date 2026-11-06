#!/usr/bin/env bash
# Tests for keploy-start-wait.sh. Run by linux-ci-scripts.yml, or locally (it
# leaves nothing behind, and needs no root: sudo here runs its command as this
# user):
#
#   bash .github/workflows/test_workflow_scripts/keploy-start-wait.tests.sh
#
# ready-later: the ready line written 2 s after the wait starts ends it, ready,
#   with no warning.
# slow-warns: an agent ready at or after KEPLOY_AGENT_READY_WARN_AFTER seconds
#   is ready, and said in a ::warning:: that names how long it took; at 0, an
#   agent ready at once is said too (the bound is "or more").
# says-step: while it waits it says so, with the log's last line (the step a
#   slow start is in).
# cadence: it says so every KEPLOY_AGENT_WAIT_SAY_EVERY seconds, from then:
#   for 2, first at 2 s, then at least 2 s apart (a look can come a second
#   late), not at once and not every second after.
# no-pid: with no PID (a driver beside a keploy in the foreground) the line
#   still ends the wait.
# exited: keploy gone with no ready line fails at once, names the exit, and
#   saves the evidence.
# ready-with-exit: a ready line that lands as keploy exits is ready, not a
#   failure.
# bound: a live keploy with no ready line fails at the bound, not before nor
#   after (it says how long it waited), and saves the evidence.
# pidfile-waits: @FILE with no FILE yet waits as with no PID, and the ready
#   line ends it once FILE names a live keploy.
# pidfile-exited: FILE written later with a keploy that has exited fails then,
#   at once, naming it, not at the bound.
# pidfile-read-once: the PID read from FILE is kept: a FILE rewritten later
#   does not change whose exit fails the wait.
# exec-noting-pid: as a pipeline's element, FILE names the command's own
#   process (exec keeps the PID), written whole, with no temp file left.
# exec-noting-pid-refuses: in a shell's own process it refuses, fails, writes
#   nothing and does not replace that shell.
# exec-noting-pid-unwritable: a FILE it cannot write is said, and the command
#   still runs.
# lane-shape: as flask-secret and django-postgres run it, a driver in the
#   background waits on @FILE while keploy runs in the foreground through
#   exec_noting_pid, piped to tee: a keploy that exits with no ready line
#   fails the driver at once; one that prints it ends the driver's wait.
# xtrace: a caller's set -x is on again after the wait, ready or not.
# alive: a live process is alive; a zombie, a missing pid and a pid that is
#   not a number ("self" included) are not.
# evidence: the logs and each boot.json (named by its directory) of every
#   root are copied, readable, with the process list and the java a root
#   shell finds; other files are not.
# evidence-never-fails: a directory that cannot be made fails nothing, and
#   says so.
# evidence-root-owned: as on CI, keploy's files are root's (0600, in a 0700
#   directory) and this user has sudo: the copies are this user's and
#   readable. Run only where `sudo -n` works for a user that is not root (CI's
#   runner); elsewhere it says it is skipped.
set -uo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=keploy-start-wait.sh
source "$DIR/keploy-start-wait.sh"
FAILS=0
TMP=$(mktemp -d)
PIDS=()
cleanup() {
  local p
  for p in "${PIDS[@]}"; do kill "$p" 2>/dev/null; done
  rm -rf "$TMP"
}
trap cleanup EXIT
fail() {
  echo "FAIL: $*"
  FAILS=$((FAILS + 1))
}
pass() { echo "ok: $*"; }

# The helper asks sudo -n; here the files are this user's.
sudo() {
  [ "${1:-}" = -n ] && shift
  "$@"
}
export KEPLOY_START_EVIDENCE_ROOTS="$TMP/none"

# ready-later
cd "$TMP" || exit 1
log=$TMP/ready.txt
: >"$log"
sleep 60 &
PIDS+=($!)
live=$!
( sleep 2; echo "$KEPLOY_AGENT_READY_LINE" >>"$log" ) &
t0=$SECONDS
if out=$(KEPLOY_AGENT_READY_BOUND=20 await_agent_ready "$log" "$live" 2>&1); then
  if [ $((SECONDS - t0)) -ge 2 ] && grep -q "ready after" <<<"$out" && ! grep -q "::warning::" <<<"$out"; then
    pass ready-later
  else
    fail "ready-later: returned after $((SECONDS - t0))s: $out"
  fi
else
  fail "ready-later: failed: $out"
fi

# says-step
log=$TMP/step.txt
echo "keploy agent start: step 'proxy CA' (busy: java keystore)" >"$log"
( sleep 3; echo "$KEPLOY_AGENT_READY_LINE" >>"$log" ) &
out=$(KEPLOY_AGENT_WAIT_SAY_EVERY=1 KEPLOY_AGENT_READY_BOUND=20 await_agent_ready "$log" "$live" 2>&1)
if grep -q "Still waiting for keploy's agent.*step 'proxy CA' (busy: java keystore)" <<<"$out"; then
  pass says-step
else
  fail "says-step: $out"
fi

# cadence
log=$TMP/cadence.txt
echo "keploy: starting" >"$log"
( sleep 5; echo "$KEPLOY_AGENT_READY_LINE" >>"$log" ) &
out=$(KEPLOY_AGENT_WAIT_SAY_EVERY=2 KEPLOY_AGENT_READY_BOUND=20 await_agent_ready "$log" "$live" 2>&1)
said=$(grep -o "Still waiting for keploy's agent ([0-9]*s)" <<<"$out" | grep -o '[0-9]*' | tr '\n' ' ')
if awk '{ if (NF < 2 || $1 != 2) exit 1; for (i = 2; i <= NF; i++) if ($i - $(i-1) < 2) exit 1 }' <<<"$said"; then
  pass "cadence (at $said)"
else
  fail "cadence: said at '$said', want 2 and then at least 2 s apart: $out"
fi

# no-pid
log=$TMP/nopid.txt
: >"$log"
( sleep 2; echo "$KEPLOY_AGENT_READY_LINE" >>"$log" ) &
t0=$SECONDS
if out=$(KEPLOY_AGENT_READY_BOUND=20 await_agent_ready "$log" 2>&1) && [ $((SECONDS - t0)) -ge 2 ]; then
  pass no-pid
else
  fail "no-pid: after $((SECONDS - t0))s: $out"
fi

# exited
log=$TMP/exited.txt
echo "keploy: could not start" >"$log"
sleep 0.1 &
gone=$!
wait "$gone"
rm -rf "$TMP/keploy-start-evidence"
t0=$SECONDS
if out=$(KEPLOY_AGENT_READY_BOUND=20 await_agent_ready "$log" "$gone" 2>&1); then
  fail "exited: ready: $out"
elif [ $((SECONDS - t0)) -ge 5 ]; then
  fail "exited: took $((SECONDS - t0))s"
elif ! grep -q "::error::keploy ($gone) exited before its agent was ready" <<<"$out"; then
  fail "exited: no error line: $out"
elif ! grep -q "could not start" <<<"$out"; then
  fail "exited: log not shown: $out"
elif [ ! -s "$TMP/keploy-start-evidence/ps.txt" ]; then
  fail "exited: no evidence saved"
else
  pass exited
fi

# ready-with-exit: the first look misses the line; it is there at the exit.
log=$TMP/withexit.txt
echo "$KEPLOY_AGENT_READY_LINE" >"$log"
looks=0
grep() {
  looks=$((looks + 1))
  [ "$looks" -eq 1 ] && return 1
  command grep "$@"
}
if out=$(KEPLOY_AGENT_READY_BOUND=20 await_agent_ready "$log" "$gone" 2>&1); then
  pass ready-with-exit
else
  fail "ready-with-exit: $out"
fi
unset -f grep

# bound
log=$TMP/bound.txt
echo "keploy: starting" >"$log"
rm -rf "$TMP/keploy-start-evidence"
t0=$SECONDS
if out=$(KEPLOY_AGENT_READY_BOUND=3 await_agent_ready "$log" "$live" 2>&1); then
  fail "bound: ready: $out"
elif [ $((SECONDS - t0)) -lt 3 ] || [ $((SECONDS - t0)) -gt 8 ]; then
  fail "bound: failed after $((SECONDS - t0))s, want 3"
elif ! grep -q "::error::keploy's agent was not ready after 3s (the bound is 3s)" <<<"$out"; then
  fail "bound: $out"
elif [ ! -s "$TMP/keploy-start-evidence/ps.txt" ]; then
  fail "bound: no evidence saved"
else
  pass bound
fi

# slow-warns
log=$TMP/slow.txt
: >"$log"
( sleep 2; echo "$KEPLOY_AGENT_READY_LINE" >>"$log" ) &
if ! out=$(KEPLOY_AGENT_READY_WARN_AFTER=1 KEPLOY_AGENT_READY_BOUND=20 await_agent_ready "$log" "$live" 2>&1); then
  fail "slow-warns: failed: $out"
elif ! grep -Eq "^::warning::keploy's agent was ready only after [2-9]s \(1s or more; the bound is 20s\)" <<<"$out"; then
  fail "slow-warns: no warning naming the wait: $out"
else
  pass slow-warns
fi
echo "$KEPLOY_AGENT_READY_LINE" >"$log"
if ! out=$(KEPLOY_AGENT_READY_WARN_AFTER=0 KEPLOY_AGENT_READY_BOUND=20 await_agent_ready "$log" "$live" 2>&1); then
  fail "slow-warns at 0: failed: $out"
elif ! grep -q "^::warning::keploy's agent was ready only after" <<<"$out"; then
  fail "slow-warns at 0: an agent ready at once, at a warning bound of 0, is not said: $out"
else
  pass "slow-warns at 0"
fi

# pidfile-waits
log=$TMP/pidwait.txt
: >"$log"
rm -f "$TMP/wait.pid"
( sleep 1; echo "$live" >"$TMP/wait.pid"; sleep 2; echo "$KEPLOY_AGENT_READY_LINE" >>"$log" ) &
t0=$SECONDS
if out=$(KEPLOY_AGENT_READY_BOUND=20 await_agent_ready "$log" "@$TMP/wait.pid" 2>&1) && [ $((SECONDS - t0)) -ge 3 ]; then
  pass pidfile-waits
else
  fail "pidfile-waits: after $((SECONDS - t0))s: $out"
fi

# pidfile-exited
log=$TMP/pidgone.txt
echo "keploy: could not start" >"$log"
rm -f "$TMP/gone.pid"
( sleep 2; echo "$gone" >"$TMP/gone.pid" ) &
t0=$SECONDS
if out=$(KEPLOY_AGENT_READY_BOUND=20 await_agent_ready "$log" "@$TMP/gone.pid" 2>&1); then
  fail "pidfile-exited: ready: $out"
elif [ $((SECONDS - t0)) -lt 2 ] || [ $((SECONDS - t0)) -ge 6 ]; then
  fail "pidfile-exited: failed after $((SECONDS - t0))s, want about 2"
elif ! grep -q "::error::keploy ($gone) exited before its agent was ready" <<<"$out"; then
  fail "pidfile-exited: no error line naming it: $out"
else
  pass pidfile-exited
fi

# pidfile-read-once
log=$TMP/pidonce.txt
: >"$log"
echo "$live" >"$TMP/once.pid"
( sleep 1; echo "$gone" >"$TMP/once.pid"; sleep 2; echo "$KEPLOY_AGENT_READY_LINE" >>"$log" ) &
if out=$(KEPLOY_AGENT_READY_BOUND=20 await_agent_ready "$log" "@$TMP/once.pid" 2>&1); then
  pass pidfile-read-once
else
  fail "pidfile-read-once: failed on a FILE rewritten after its PID was read: $out"
fi

# exec-noting-pid
rm -f "$TMP/exec.pid"
# shellcheck disable=SC2016 # the $$ is sh's
out=$(exec_noting_pid "$TMP/exec.pid" sh -c 'echo "$$"' | cat)
if [ -z "$out" ] || [ "$out" != "$(cat "$TMP/exec.pid" 2>&1)" ]; then
  fail "exec-noting-pid: the command ran as $out; FILE says $(cat "$TMP/exec.pid" 2>&1)"
elif ls "$TMP"/exec.pid.* >/dev/null 2>&1; then
  fail "exec-noting-pid: a temp file is left: $(ls "$TMP"/exec.pid.*)"
else
  pass exec-noting-pid
fi

# exec-noting-pid-refuses
rm -f "$TMP/own.pid"
# shellcheck disable=SC2016 # $1, $2 and $? are that shell's
out=$(bash -c 'source "$1"; exec_noting_pid "$2" echo replaced; echo "returned $?"' sh "$DIR/keploy-start-wait.sh" "$TMP/own.pid" 2>&1)
if ! grep -q "^returned 1$" <<<"$out" || grep -q replaced <<<"$out" || [ -e "$TMP/own.pid" ]; then
  fail "exec-noting-pid-refuses: $out (FILE there: $([ -e "$TMP/own.pid" ] && echo yes || echo no))"
elif ! grep -q "::error::exec_noting_pid runs only in a subshell" <<<"$out"; then
  fail "exec-noting-pid-refuses: not said: $out"
else
  pass exec-noting-pid-refuses
fi

# exec-noting-pid-unwritable
out=$(exec_noting_pid "$TMP/no-such-dir/x.pid" echo ran | cat)
if grep -q "could not write keploy's PID to $TMP/no-such-dir/x.pid" <<<"$out" && grep -q "^ran$" <<<"$out"; then
  pass exec-noting-pid-unwritable
else
  fail "exec-noting-pid-unwritable: $out"
fi

# lane-shape
lane() { # lane NAME KEPLOY_SCRIPT: the driver's status, then its seconds
  local app=$TMP/$1 d t0=$SECONDS
  : >"$app.txt"
  rm -f "$app.pid"
  ( KEPLOY_AGENT_READY_BOUND=20 await_agent_ready "$app.txt" "@$app.pid" >"$app.out" 2>&1 ) &
  d=$!
  exec_noting_pid "$app.pid" bash -c "$2" 2>&1 | tee "$app.txt" >/dev/null
  wait "$d"
  echo "$? $((SECONDS - t0))"
}
read -r rc took < <(lane lane-exit 'sleep 1; echo "keploy: could not start"; exit 3')
if [ "$rc" = 0 ] || [ "$took" -ge 5 ] || ! grep -q "exited before its agent was ready" "$TMP/lane-exit.out"; then
  fail "lane-shape exit: driver ended $rc after ${took}s: $(cat "$TMP/lane-exit.out")"
else
  pass "lane-shape exit"
fi
read -r rc took < <(lane lane-ready "sleep 1; echo '$KEPLOY_AGENT_READY_LINE'; sleep 1")
if [ "$rc" != 0 ] || ! grep -q "ready after" "$TMP/lane-ready.out"; then
  fail "lane-shape ready: driver ended $rc after ${took}s: $(cat "$TMP/lane-ready.out")"
else
  pass "lane-shape ready"
fi

# xtrace
xt=$(
  exec 2>/dev/null
  set -x
  echo "$KEPLOY_AGENT_READY_LINE" >"$TMP/xt.txt"
  await_agent_ready "$TMP/xt.txt" "$live" >/dev/null 2>&1
  a=$-
  echo "keploy: starting" >"$TMP/xt.txt"
  KEPLOY_AGENT_READY_BOUND=1 await_agent_ready "$TMP/xt.txt" "$live" >/dev/null 2>&1
  b=$-
  KEPLOY_AGENT_READY_BOUND=1 await_agent_ready "$TMP/xt.txt" "$gone" >/dev/null 2>&1
  set +x
  echo "$a|$b|$-"
)
case "$xt" in
*x*"|"*x*"|"*) pass xtrace ;;
*) fail "xtrace: the options after each wait: $xt" ;;
esac
xt=$(
  exec 2>/dev/null
  set -x
  KEPLOY_AGENT_READY_BOUND=1 await_agent_ready "$TMP/xt.txt" "$gone" >/dev/null 2>&1
  c=$-
  set +x
  echo "$c"
)
case "$xt" in
*x*) pass xtrace-exited ;;
*) fail "xtrace-exited: the options after the wait: $xt" ;;
esac

# alive
python3 -c '
import os, sys, time
pid = os.fork()
if pid == 0:
    os._exit(0)
with open(sys.argv[1], "w") as f:
    f.write(str(pid))
time.sleep(30)
' "$TMP/zpid" &
PIDS+=($!)
sleep 1
zpid=$(cat "$TMP/zpid" 2>/dev/null)
if ! _kse_alive "$live"; then
  fail "alive: a live process is not alive"
elif [ -z "$zpid" ] || _kse_alive "$zpid"; then
  fail "alive: the zombie '$zpid' is alive ($(cat "/proc/$zpid/stat" 2>/dev/null))"
elif _kse_alive "$gone" || _kse_alive "" || _kse_alive "12x" || _kse_alive self; then
  fail "alive: a gone pid, an empty one or a non-number is alive"
else
  pass alive
fi

# evidence
r1=$TMP/root1 r2=$TMP/root2
mkdir -p "$r1/keploy-agent-stop-aaa" "$r2/keploy-agent-stop-bbb" "$r2/keploy-agent-stop-ccc"
echo log1 >"$r1/keploy-logs-1.txt"
echo log2 >"$r2/keploy-logs-2.txt"
echo '{"step":"a"}' >"$r1/keploy-agent-stop-aaa/boot.json"
echo '{"step":"b"}' >"$r2/keploy-agent-stop-bbb/boot.json"
echo '{"docker":1}' >"$r2/keploy-agent-boot.json"
echo other >"$r1/other.txt"
echo teardown >"$r2/keploy-agent-stop-ccc/teardown.json"
chmod 600 "$r1/keploy-logs-1.txt"
KEPLOY_START_EVIDENCE_ROOTS="$r1 $r2" save_keploy_start_evidence "$TMP/ev" >"$TMP/ev.out" 2>&1
got=$(cd "$TMP/ev" 2>/dev/null && ls | sort | tr '\n' ' ')
want="boot-aaa.json boot-bbb.json keploy-agent-boot.json keploy-logs-1.txt keploy-logs-2.txt ps.txt root-java.txt "
if [ "$got" != "$want" ]; then
  fail "evidence: got '$got', want '$want'"
elif [ "$(cat "$TMP/ev/boot-bbb.json")" != '{"step":"b"}' ] || [ "$(cat "$TMP/ev/keploy-logs-1.txt")" != log1 ]; then
  fail "evidence: contents differ"
elif [ ! -r "$TMP/ev/keploy-logs-1.txt" ] || [ "$(stat -c %a "$TMP/ev/keploy-logs-1.txt")" != 644 ]; then
  fail "evidence: the log is not readable by all: $(stat -c %a "$TMP/ev/keploy-logs-1.txt")"
elif ! grep -q "sleep 60" "$TMP/ev/ps.txt"; then
  fail "evidence: ps.txt lacks the running processes"
elif [ "$(cat "$TMP/ev/root-java.txt")" != "$(command -v java || echo "no java on root PATH ($PATH)")" ]; then
  fail "evidence: root-java.txt says '$(cat "$TMP/ev/root-java.txt")'"
else
  pass evidence
fi

# evidence-never-fails
echo x >"$TMP/afile"
if out=$(save_keploy_start_evidence "$TMP/afile/ev" 2>&1) && grep -q "could not make $TMP/afile/ev" <<<"$out" && ! grep -q "are in" <<<"$out"; then
  pass evidence-never-fails
else
  fail "evidence-never-fails: returned non-zero"
fi

# evidence-root-owned
if [ "$(id -u)" = 0 ] || ! command sudo -n true 2>/dev/null; then
  echo "skip: evidence-root-owned (needs a user that is not root, with sudo -n)"
else
  rr=$TMP/rootroot
  mkdir -p "$rr"
  command sudo -n sh -c 'mkdir -m 700 "$1/keploy-agent-stop-rrr" && echo "{\"step\":\"r\"}" >"$1/keploy-agent-stop-rrr/boot.json" && echo rootlog >"$1/keploy-logs-9.txt" && chmod 600 "$1/keploy-agent-stop-rrr/boot.json" "$1/keploy-logs-9.txt" && chown -R 0:0 "$1"' sh "$rr"
  (
    unset -f sudo
    KEPLOY_START_EVIDENCE_ROOTS=$rr save_keploy_start_evidence "$TMP/evr" >/dev/null 2>&1
  )
  if [ "$(cat "$TMP/evr/boot-rrr.json" 2>&1)" != '{"step":"r"}' ] || [ "$(cat "$TMP/evr/keploy-logs-9.txt" 2>&1)" != rootlog ]; then
    fail "evidence-root-owned: the copies are not readable: $(ls -la "$TMP/evr" 2>&1)"
  elif [ -n "$(find "$TMP/evr" ! -user "$(id -u)" 2>/dev/null)" ]; then
    fail "evidence-root-owned: copies not this user's: $(ls -la "$TMP/evr" 2>&1)"
  else
    pass evidence-root-owned
  fi
  command sudo -n rm -rf "$rr" "$TMP/evr"
fi

if [ "$FAILS" -ne 0 ]; then
  echo "$FAILS case(s) failed"
  exit 1
fi
echo "all cases passed"
