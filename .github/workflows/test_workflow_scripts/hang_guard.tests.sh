#!/usr/bin/env bash
# Rows for hang_guard.sh: the exit code a run ends with is its own, whatever
# coreutils the host has, and the guard says when it ended one; plus a sweep
# that no script under .github takes an exit code it checks from timeout(1).
# Runs on any host with bash (no root, no network).
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FAILS=0
ok() { echo "ok: $1"; }
bad() { echo "FAIL: $1"; FAILS=$((FAILS + 1)); }
# Sourced, the guard only defines its functions; run as a command, it bounds
# what it is given (rows below).
got=$(bash -c '. "$1" && type -t bounded' _ "$here/hang_guard.sh" 2>&1 </dev/null)
[ "$got" = function ] && ok "sourced, the guard defines its functions and runs nothing" ||
  { bad "sourcing hang_guard.sh gave \"$got\", want only its functions defined"; exit 1; }
# shellcheck source=hang_guard.sh
. "$here/hang_guard.sh" || exit 1
root="$(cd "$here/../../.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# A command that exits 0 on SIGINT, SIGTERM or SIGHUP, once it is ready,
# and ends its sleep first (a background sleep ignores INT; left, it held
# the caller's stdout 30 s after the rows ended).
cat >"$tmp/trap0.sh" <<'E'
for s in INT TERM HUP; do trap 'kill "$p" 2>/dev/null; exit 0' $s; done
sleep 30 & p=$!
touch "$1"
wait
E
# One that ignores SIGTERM, and ends by itself (0) after 8 s.
cat >"$tmp/ignterm.sh" <<'E'
trap "" TERM
sleep 8 & p=$!
touch "$1"
while kill -0 "$p" 2>/dev/null; do wait "$p"; done
E
# cpu_children: the user + system time, in clock ticks, of the children this
# shell has waited for (from /proc), watchdogs and their sleeps among them.
cpu_children() { local f; read -r -a f <"/proc/$$/stat"; echo $((f[15] + f[16])); }
ready() { local i; for ((i = 0; i < 100; i++)); do [ -e "$1" ] && return 0; sleep 0.1; done; return 1; }

# row NAME WANT_RC WANT_LINES(count of HANG_LINE lines) RC LOG
row() {
  local name=$1 want=$2 lines=$3 rc=$4 log=$5 n
  n=$(grep -c "^$HANG_LINE" "$log")
  if [ "$rc" = "$want" ] && [ "$n" = "$lines" ]; then ok "$name"; else
    bad "$name: exit $rc, want $want; $n guard lines, want $lines"; cat "$log"; fi
}

bounded 5 sh -c 'exit 42' 2>"$tmp/l1"; row "an exit code passes as it is" 42 0 $? "$tmp/l1"

for sig in INT TERM HUP; do
  rm -f "$tmp/r"
  hang_start 30 bash "$tmp/trap0.sh" "$tmp/r" 2>"$tmp/l2"
  ready "$tmp/r" && kill -s "$sig" "$HANG_PID"
  hang_wait "$HANG_PID"
  row "SIG$sig sent to the run, which exits 0 on it: 0 (uutils timeout: $((128 + $(kill -l "$sig"))))" 0 0 $? "$tmp/l2"
done

bounded -s KILL 1 sleep 30 2>"$tmp/l3"; row "the guard's KILL: 137, and it says so (uutils timeout: 124)" 137 1 $? "$tmp/l3"
bounded 1 sleep 30 2>"$tmp/l4"; row "the guard's TERM: 143, and it says so" 143 1 $? "$tmp/l4"
bounded -s INT 1 bash "$tmp/trap0.sh" "$tmp/r5" 2>"$tmp/l5"; row "the guard's INT, the run exits 0 on it: 0, and it says so" 0 1 $? "$tmp/l5"
bounded -k 1 1 bash "$tmp/ignterm.sh" "$tmp/r6" 2>"$tmp/l6"; row "-k: KILL after the TERM a run ignores: 137, both lines" 137 2 $? "$tmp/l6"
bounded -q -k 1 1 bash "$tmp/ignterm.sh" "$tmp/r13" 2>"$tmp/l13"; row "-q -k: KILL after the TERM a run ignores: 137, no line" 137 0 $? "$tmp/l13"
bash "$here/hang_guard.sh" 5 sh -c 'exit 7' 2>"$tmp/l7"; row "run as a command: the code passes as it is" 7 0 $? "$tmp/l7"
bash "$here/hang_guard.sh" -s KILL 1 sleep 30 2>"$tmp/l8"; row "run as a command: the guard's KILL is 137" 137 1 $? "$tmp/l8"

hang_start -q 1 sleep 30 2>"$tmp/l9"; hang_wait "$HANG_PID"; rc=$?
[ "$HANG_FIRED" = 1 ] && [ ! -s "$tmp/l9" ] && [ "$rc" = 143 ] && ok "-q: says nothing, and HANG_FIRED says it fired" ||
  bad "-q: HANG_FIRED=$HANG_FIRED, exit $rc, said \"$(cat "$tmp/l9")\""
# -k: the run ends on the TERM while the watchdog waits to KILL it; the
# watchdog, which then ends by itself, counts as fired.
hang_start -q -k 30 1 sleep 30 2>/dev/null; hang_wait "$HANG_PID"; rc=$?
[ "$HANG_FIRED" = 1 ] && [ "$rc" = 143 ] && ok "-k: a run the TERM ended sets HANG_FIRED" ||
  bad "-k: HANG_FIRED=$HANG_FIRED, exit $rc, for a run the guard's TERM ended"
# -k with a run the TERM ended, waited on only later: the KILL is not sent,
# nor said (one guard line), and it still counts as fired.
hang_start -k 1 1 sleep 30 2>"$tmp/l11"; sleep 3; hang_wait "$HANG_PID"
row "-k: a run the TERM ended gets no KILL, one line" 143 1 $? "$tmp/l11"
[ "$HANG_FIRED" = 1 ] || bad "-k: HANG_FIRED=$HANG_FIRED for a run the TERM ended, its watchdog done"
bounded 5 true 2>/dev/null
[ "$HANG_FIRED" = 0 ] && ok "HANG_FIRED is 0 for a run that ends in time" || bad "HANG_FIRED=$HANG_FIRED for a run that ended in time"
got=$( (bounded -s KILL 1 sleep 30 2>/dev/null; echo "rc=$?") | cat)
[ "$got" = rc=137 ] && ok "in a pipeline's subshell, the guard still ends the run" || bad "in a pipeline: \"$got\", want rc=137"

# Neither the run nor its watchdog runs the caller's EXIT trap: once, for
# the caller itself. A builtin run is not exec'd, so its subshell exits.
# Runs that end at once are the ones whose watchdog a signal would reach as
# it starts, while it still has the caller's handlers (that ran the trap in
# a few of every hundred such runs on bash 5.2 when the guard TERMed it).
: >"$tmp/exit-trap"
(
  trap 'echo x >>"$tmp/exit-trap"' EXIT
  for ((i = 0; i < 20; i++)); do bounded 5 true; done
  bounded -q 1 sleep 5
) 2>/dev/null
n=$(wc -l <"$tmp/exit-trap")
[ "$n" = 1 ] && ok "the caller's EXIT trap runs once, in the caller only" || bad "the caller's EXIT trap ran $n times, want 1"

# The guard signals the run's group and nothing else: not its watchdog,
# whether the run ends in time or the guard ends it. (A function named kill
# runs in place of the builtin, in the watchdog too.)
: >"$tmp/kills"
got=$(
  kill() { echo "$*" >>"$tmp/kills"; builtin kill "$@"; }
  bounded 5 true
  bounded -q 1 sleep 30
  echo "$HANG_PID"
) 2>/dev/null
[ "$(cat "$tmp/kills")" = "-s TERM -- -$got" ] && ok "the guard signals only the run's process group" ||
  bad "the guard sent: \"$(tr '\n' ';' <"$tmp/kills")\", want only \"-s TERM -- -$got\""

# The watchdog sleeps between its looks at the run: a 2 s run costs it a
# few ticks of CPU, not the 2 s a busy loop would.
c0=$(cpu_children)
bounded 5 sleep 2 2>/dev/null
c1=$(cpu_children)
[ $((c1 - c0)) -lt 50 ] && ok "the watchdog sleeps between its looks at the run" ||
  bad "a 2 s bounded run cost its children $((c1 - c0)) ticks of CPU, want under 50"

# The guard leaves job control as the caller had it.
bounded 5 true 2>/dev/null
case $- in *m*) bad "job control is on after a bounded run, and was off" ;; *) ok "job control stays off" ;; esac
set -m; bounded 5 true 2>/dev/null
case $- in *m*) ok "job control stays on" ;; *) bad "job control is off after a bounded run, and was on" ;; esac
set +m

# hang_running takes a pid: nothing else counts as running, not even a word
# /proc has an entry for.
if hang_running "" || hang_running self; then bad "hang_running: an empty or non-numeric pid counts as running"; else
  ok "hang_running: only a number is a pid"; fi

# A run that ended but is not reaped yet (its caller is stopped, so the run
# is a zombie) is not running: the watchdog does not fire on it.
(
  hang_start -q 1 sleep 0.3
  touch "$tmp/z-ready"
  sleep 2.5
  hang_wait "$HANG_PID"
  echo "rc=$? fired=$HANG_FIRED" >"$tmp/z-out"
) 2>/dev/null &
zp=$!
ready "$tmp/z-ready" && kill -STOP "$zp"
sleep 2
kill -CONT "$zp"
wait "$zp"
got=$(cat "$tmp/z-out" 2>/dev/null)
[ "$got" = "rc=0 fired=0" ] && ok "a zombie run is not running: the watchdog does not fire" || bad "a zombie run: \"$got\", want rc=0 fired=0"

# A run of this user's own: the guard needs no sudo, and does not call it
# (a sudo on PATH that says when it is called), for a run that outlives its
# TERM too.
mkdir -p "$tmp/fakesudo" && printf '#!/bin/sh\necho "$*" >>"$FAKE_SUDO_LOG"\nexit "${FAKE_SUDO_RC:-0}"\n' >"$tmp/fakesudo/sudo" &&
  chmod +x "$tmp/fakesudo/sudo"
: >"$tmp/sudo-calls"
FAKE_SUDO_LOG="$tmp/sudo-calls" PATH="$tmp/fakesudo:$PATH" bounded -k 1 1 bash -c 'trap "" TERM; sleep 30 & wait' 2>"$tmp/l16"
row "a run of this user's own that ignores TERM: -k's KILL: 137, two lines" 137 2 $? "$tmp/l16"
[ ! -s "$tmp/sudo-calls" ] && ok "a run of this user's own: the guard does not call sudo" ||
  bad "the guard called sudo for a run of this user's own: $(cat "$tmp/sudo-calls")"

# A run under sudo: the guard ends all of it, root's command included. sudo
# keeps this user's real uid while its command runs, so the guard's signal
# reaches it, and sudo relays it; but not KILL, which ends sudo alone, so the
# guard sends its signal through sudo -n kill too. Needs password-free sudo
# for a user that is not root (a GitHub runner's): on GitHub Actions the
# rows fail without it; elsewhere they say they did not run.
# Each run gets 3 s to start before the guard fires, so a slow sudo still
# writes its pids first.
# gone NAME FILE: each pid in FILE has ended (2 s for root's to be reaped);
# one left is a FAIL, and is ended here so it holds nothing after the rows.
gone() {
  local p left="" i
  for ((i = 0; i < 10; i++)); do
    left=""
    for p in $(cat "$2" 2>/dev/null); do hang_running "$p" && left+=" $p"; done
    [ -n "$left" ] || break
    sleep 0.2
  done
  if [ -s "$2" ] && [ -z "$left" ]; then ok "$1"; else
    bad "$1: still running:${left:- (no pid written)}"
    # shellcheck disable=SC2086
    [ -z "$left" ] || sudo -n kill -s KILL $left 2>/dev/null
  fi
}
if [ "$(id -u)" != 0 ] && sudo -n true 2>/dev/null; then
  bounded 1 sudo -n sleep 30 2>"$tmp/l10"; row "a run under sudo: the guard's TERM reaches it and sudo relays it: 143" 143 1 $? "$tmp/l10"
  bounded -s KILL 3 sudo -n sh -c 'echo $$ >"$1"; exec sleep 30' _ "$tmp/s1" 2>"$tmp/l14"
  row "a run under sudo: the guard's KILL: 137, one line" 137 1 $? "$tmp/l14"
  gone "a run under sudo: the guard's KILL ends root's command, not only sudo" "$tmp/s1"
  bounded -k 1 3 sudo -n bash -c 'trap "" TERM; echo $$ >"$1"; sleep 30 & echo $! >>"$1"; wait' _ "$tmp/s2" 2>"$tmp/l15"
  row "a run under sudo that ignores TERM: -k's KILL: 137, two lines" 137 2 $? "$tmp/l15"
  gone "a run under sudo that ignores TERM: -k's KILL ends root's command and its child" "$tmp/s2"
  # A command substitution ends when the last writer of its pipe has: root's
  # command, which only the guard's KILL through sudo ends.
  t0=$SECONDS
  got=$(bounded -q -k 1 3 sudo -n bash -c 'trap "" TERM; echo $$ >"$1"; sleep 12; echo late' _ "$tmp/s3" 2>/dev/null)
  took=$((SECONDS - t0))
  [ -z "$got" ] && [ "$took" -le 8 ] && ok "a run under sudo in a command substitution is bounded too" ||
    bad "a run under sudo in a command substitution took ${took}s and printed \"$got\", want at most 8 s and nothing"
  gone "a run under sudo in a command substitution: root's command ended" "$tmp/s3"
  # When sudo -n kill fails, the guard says root's command may still run.
  realsudo=$(command -v sudo)
  FAKE_SUDO_LOG="$tmp/sudo-calls" FAKE_SUDO_RC=1 PATH="$tmp/fakesudo:$PATH" \
    bounded -s KILL 3 "$realsudo" -n sh -c 'echo $$ >"$1"; exec sleep 30' _ "$tmp/s4" 2>"$tmp/l17"
  row "a run under sudo whose sudo -n kill fails: 137, and the guard says so (two lines)" 137 2 $? "$tmp/l17"
  grep -q "could not send SIGKILL" "$tmp/l17" || bad "the guard's line for a failed sudo -n kill is missing: $(cat "$tmp/l17")"
  # shellcheck disable=SC2046
  sudo -n kill -s KILL $(cat "$tmp/s4" 2>/dev/null) 2>/dev/null
  # As root the guard needs no sudo, for another user's member too, one
  # that outlives the TERM.
  : >"$tmp/sudo-calls"
  sudo -n env PATH="$tmp/fakesudo:/usr/sbin:/usr/bin:/sbin:/bin" FAKE_SUDO_LOG="$tmp/sudo-calls" bash -c \
    '. "$1"; bounded -k 1 1 setpriv --reuid=65534 --regid=65534 --clear-groups sh -c "trap \"\" TERM; exec sleep 30"' \
    _ "$here/hang_guard.sh" 2>"$tmp/l18"
  row "as root, a run of another user's that ignores TERM: -k's KILL: 137, two lines" 137 2 $? "$tmp/l18"
  [ ! -s "$tmp/sudo-calls" ] && ok "as root, the guard does not call sudo" ||
    bad "the guard, as root, called sudo: $(cat "$tmp/sudo-calls")"
elif [ "${GITHUB_ACTIONS:-}" = true ]; then
  bad "a run under sudo: these rows need password-free sudo, as a user that is not root, which a GitHub runner has"
else
  echo "skip: a run under sudo (needs password-free sudo, as a user that is not root)"
fi

# The bound is real time even when the caller's PATH has a sleep that
# returns at once (golang-docker-fakes.sh puts one there), and the watchdog
# still sleeps between its looks: it does not spin on the fake.
mkdir -p "$tmp/fakebin" && printf '#!/bin/sh\nexit 0\n' >"$tmp/fakebin/sleep" && chmod +x "$tmp/fakebin/sleep"
c0=$(cpu_children)
PATH="$tmp/fakebin:$PATH" bounded 3 sh -c '/bin/sleep 2; exit 5' 2>"$tmp/l12"
row "a fake sleep on PATH does not end the bound early" 5 0 $? "$tmp/l12"
c1=$(cpu_children)
[ $((c1 - c0)) -lt 50 ] && ok "a fake sleep on PATH: the watchdog still sleeps between its looks" ||
  bad "a 2 s bounded run with a fake sleep on PATH cost its children $((c1 - c0)) ticks of CPU, want under 50"

# The guard's signal reaches the run's whole process group: a child the run
# started ends with it.
bounded -s KILL 1 bash -c 'sleep 30 & echo $! >"$1"; wait' _ "$tmp/child" 2>/dev/null
cp=$(cat "$tmp/child" 2>/dev/null)
if [ -n "$cp" ] && ! hang_running "$cp"; then ok "the guard's signal ends the run's children too"; else
  bad "the run's child ${cp:-?} still runs after the guard's KILL"; [ -z "$cp" ] || kill "$cp" 2>/dev/null; fi

got=$(echo piped | bounded 5 cat 2>/dev/null)
[ "$got" = piped ] && ok "the run reads the caller's stdin" || bad "the run read \"$got\" from stdin, want \"piped\""

# A run that ends in time returns at once, and leaves no watchdog, nor its
# sleep, behind.
t0=$SECONDS
bounded 20 true 2>/dev/null
took=$((SECONDS - t0))
left=$(ps -o pid=,args= --ppid $$ | grep -c 'sleep 20')
[ "$took" -le 3 ] && [ "$left" = 0 ] && [ "${#HANG_WATCHDOG[@]}" = 0 ] && ok "a run that ends in time returns at once and leaves no watchdog" ||
  bad "a run that ended in time took ${took}s, left $left sleep(s), ${#HANG_WATCHDOG[@]} watchdog(s)"

# The sweep: a line that runs timeout(1) and whose exit status is kept (into
# a variable, or by the next line that reads $? or ${PIPESTATUS[...]}: the
# code of a timeout run in a pipeline), or whose run is started in
# the background (its pid waited on), or that sits in an array a run is
# started from. timeout counts after any word boundary (a `docker run IMAGE
# timeout` line has it after the image) when its next word is one of its
# own: a flag, a number or a variable's value; so `local timeout kind` is not
# a run. A comment line is not code. A spelling no script uses (timeout's
# limit from a command substitution, `command timeout`, a background run's
# `& pid=$!` on its own line, a multi-line `( ... ) &`) is not looked for.
# timeout_takers FILE...: prints FILE:LINE for each such line.
timeout_takers() {
  awk '
    function cmd(l) { return l ~ /(^|[[:space:];&|(`!{])timeout[[:space:]]+(-|[0-9]|"?\$)/ }
    FNR == 1 { held = "" }
    /^[[:space:]]*#/ { next }
    {
      if (held != "" && $0 ~ /\$\?|PIPESTATUS/) print held
      held = ""
      if (!cmd($0)) next
      if ($0 ~ /\$\?|PIPESTATUS/ || $0 ~ /&[[:space:]]*(\\)?$/ || $0 ~ /=\((.* )?timeout /) { print FILENAME ":" FNR; next }
      # a run continued over lines: its last line decides
      line = FILENAME ":" FNR
      while ($0 ~ /\\$/ && (getline nxt) > 0) { $0 = nxt; if ($0 ~ /&[[:space:]]*$/ || $0 ~ /\$\?|PIPESTATUS/) { print line; held = ""; next } }
      held = line
    }
  ' "$@"
}
# Fixtures: each must be found, and none of the clean ones; a line is found
# by its own rule, not by a $? on the line after it, and a file's first line
# reads no $? of the file before it.
cat >"$tmp/takes.sh" <<'E'
timeout 5 keploy record; rc=$?
out=$(timeout 60 keploy test 2>&1 </dev/null)
rc=$?
sudo env A=1 timeout -k 10 180 keploy record \
  --record-timer 60s > log 2>&1 &
timeout 30 keploy record >log 2>&1 &
docker run --rm img timeout -s KILL 30 keploy report >log 2>&1
echo $? >exit
x=1 timeout 45 apt-get update || rc=$?
runner=(timeout 120 script -qec "$cmd" /dev/null)
timeout --kill-after=30s 8m env A=1 \
  keploy test 2>&1 | tee log
rc=${PIPESTATUS[0]}
timeout 5 keploy test |& tee log; rc=${PIPESTATUS[0]}
timeout 8m env A=1 \
  keploy test | tee log; rc=${PIPESTATUS[0]}
E
cat >"$tmp/clean.sh" <<'E'
# timeout 5 keploy record; rc=$?
bash hang_guard.sh 60 keploy test 2>&1 | cat >out; rc=${PIPESTATUS[0]}
sudo timeout 600 apt-get -y update || true
if ! timeout 5s bash -c "until x; do sleep 1; done"; then echo no; fi
local timeout kind
bounded -k 10 180 keploy record; rc=$?
echo "--timeout 5"; rc=$?
timeout 5 sync
E
printf 'rc=$?\n' >"$tmp/next.sh"
want="$tmp/takes.sh:1 $tmp/takes.sh:2 $tmp/takes.sh:4 $tmp/takes.sh:6 $tmp/takes.sh:7 $tmp/takes.sh:9 $tmp/takes.sh:10 $tmp/takes.sh:11 $tmp/takes.sh:14 $tmp/takes.sh:15"
got=$(timeout_takers "$tmp/takes.sh" "$tmp/clean.sh" "$tmp/next.sh" | tr '\n' ' ')
[ "${got% }" = "$want" ] && ok "the sweep finds each spelling that keeps timeout's code, and no other line" ||
  bad "the sweep found \"${got% }\", want \"$want\""
# Every file but this one, whose fixtures above are such lines on purpose.
mapfile -t scripts < <(cd "$root" && { git ls-files -- '.github/*.sh' '.github/*.yml' '.github/*.yaml' 2>/dev/null ||
  find .github -name '*.sh' -o -name '*.yml' -o -name '*.yaml'; } | grep -vx '.github/workflows/test_workflow_scripts/hang_guard.tests.sh')
found=$(cd "$root" && timeout_takers "${scripts[@]}")
[ -z "$found" ] && ok "no script under .github takes an exit code from timeout(1) (${#scripts[@]} files)" ||
  bad "these lines take an exit code from timeout(1); use bounded or hang_start/hang_wait (hang_guard.sh):
$found"

if [ "$FAILS" -ne 0 ]; then echo "hang_guard.tests.sh: $FAILS FAILED"; exit 1; fi
echo "hang_guard.tests.sh: all passed"
