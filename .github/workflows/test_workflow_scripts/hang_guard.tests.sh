#!/usr/bin/env bash
# Rows for hang_guard.sh: the exit code a run ends with is its own, whatever
# coreutils the host has, and the guard says when it ended one; plus a sweep
# that no script under .github takes an exit code it checks from timeout(1).
# Runs on any host with bash (no root, no network).
set -uo pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=hang_guard.sh
. "$here/hang_guard.sh" || exit 1
root="$(cd "$here/../../.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
FAILS=0
ok() { echo "ok: $1"; }
bad() { echo "FAIL: $1"; FAILS=$((FAILS + 1)); }

# A command that exits 0 on SIGINT, SIGTERM or SIGHUP, once it is ready.
cat >"$tmp/trap0.sh" <<'E'
for s in INT TERM HUP; do trap "exit 0" $s; done
touch "$1"
sleep 30 & wait
E
cat >"$tmp/ignterm.sh" <<'E'
trap "" TERM
touch "$1"
sleep 30 & while :; do wait; done
E
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
bash "$here/hang_guard.sh" 5 sh -c 'exit 7' 2>"$tmp/l7"; row "run as a command: the code passes as it is" 7 0 $? "$tmp/l7"
bash "$here/hang_guard.sh" -s KILL 1 sleep 30 2>"$tmp/l8"; row "run as a command: the guard's KILL is 137" 137 1 $? "$tmp/l8"

hang_start -q 1 sleep 30 2>"$tmp/l9"; hang_wait "$HANG_PID"; rc=$?
[ "$HANG_FIRED" = 1 ] && [ ! -s "$tmp/l9" ] && [ "$rc" = 143 ] && ok "-q: says nothing, and HANG_FIRED says it fired" ||
  bad "-q: HANG_FIRED=$HANG_FIRED, exit $rc, said \"$(cat "$tmp/l9")\""
bounded 5 true 2>/dev/null
[ "$HANG_FIRED" = 0 ] && ok "HANG_FIRED is 0 for a run that ends in time" || bad "HANG_FIRED=$HANG_FIRED for a run that ended in time"
got=$( (bounded -s KILL 1 sleep 30 2>/dev/null; echo "rc=$?") | cat)
[ "$got" = rc=137 ] && ok "in a pipeline's subshell, the guard still ends the run" || bad "in a pipeline: \"$got\", want rc=137"

got=$(echo piped | bounded 5 cat 2>/dev/null)
[ "$got" = piped ] && ok "the run reads the caller's stdin" || bad "the run read \"$got\" from stdin, want \"piped\""

# A run that ends in time leaves no watchdog, nor its sleep, behind.
bounded 600 true 2>/dev/null
left=$(ps -o pid=,args= --ppid $$ | grep -c 'sleep 600')
[ "$left" = 0 ] && [ "${#HANG_WATCHDOG[@]}" = 0 ] && ok "a run that ends in time leaves no watchdog" ||
  bad "a run that ended in time left $left sleep(s), ${#HANG_WATCHDOG[@]} watchdog(s)"

# The sweep: a line that runs timeout(1) and whose exit status is kept (into
# a variable, or by the next line that reads $?), or whose run is started in
# the background (its pid waited on), or that sits in an array a run is
# started from. timeout counts after any word boundary (a `docker run IMAGE
# timeout` line has it after the image) when its next word is one of its
# own: a flag, a number or a variable's value; so `local timeout kind` is not
# a run. A comment line is not code. A spelling no script uses (timeout's
# limit from a command substitution, `command timeout`) is not looked for.
# timeout_takers FILE...: prints FILE:LINE for each such line.
timeout_takers() {
  awk '
    function cmd(l) { return l ~ /(^|[[:space:];&|(`!{])timeout[[:space:]]+(-|[0-9]|"?\$)/ }
    FNR == 1 { held = "" }
    /^[[:space:]]*#/ { next }
    {
      if (held != "" && $0 ~ /\$\?/) print held
      held = ""
      if (!cmd($0)) next
      if ($0 ~ /\$\?/ || $0 ~ /&[[:space:]]*(\\)?$/ || $0 ~ /=\((.* )?timeout /) { print FILENAME ":" FNR; next }
      # a run continued over lines: its last line decides
      line = FILENAME ":" FNR
      while ($0 ~ /\\$/ && (getline nxt) > 0) { $0 = nxt; if ($0 ~ /&[[:space:]]*$/ || $0 ~ /\$\?/) { print line; held = ""; next } }
      held = line
    }
  ' "$@"
}
# Fixtures: each must be found, and none of the clean ones.
cat >"$tmp/takes.sh" <<'E'
timeout 5 keploy record; rc=$?
out=$(timeout 60 keploy test 2>&1 </dev/null)
rc=$?
sudo env A=1 timeout -k 10 180 keploy record \
  --record-timer 60s > log 2>&1 &
docker run --rm img timeout -s KILL 30 keploy report >log 2>&1
echo $? >exit
runner=(timeout 120 script -qec "$cmd" /dev/null)
x=1 timeout 45 apt-get update || rc=$?
E
cat >"$tmp/clean.sh" <<'E'
# timeout 5 keploy record; rc=$?
sudo timeout 600 apt-get -y update || true
if ! timeout 5s bash -c "until x; do sleep 1; done"; then echo no; fi
local timeout kind
bounded -k 10 180 keploy record; rc=$?
echo "--timeout 5"; rc=$?
E
want="$tmp/takes.sh:1 $tmp/takes.sh:2 $tmp/takes.sh:4 $tmp/takes.sh:6 $tmp/takes.sh:8 $tmp/takes.sh:9"
got=$(timeout_takers "$tmp/takes.sh" "$tmp/clean.sh" | tr '\n' ' ')
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
