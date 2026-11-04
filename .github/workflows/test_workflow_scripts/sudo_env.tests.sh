#!/usr/bin/env bash
# Tests for sudo_env.sh's SUDO_ENV and verify_ran. Run by
# linux-ci-scripts.yml, or locally (it needs only bash and grep, and leaves
# nothing behind):
#
#   bash .github/workflows/test_workflow_scripts/sudo_env.tests.sh
#
# SUDO_ENV names KEPLOY_EXACT_VIEW_VERIFY with its value while it is set, an
# empty value included, and nothing while it is unset. verify_ran passes a run
# under the proven switch whose log holds the agent's verify line, and fails
# one whose log lacks it or does not exist, saying so; a run without the
# switch, or with KEPLOY_EXACT_VIEW_VERIFY other than 1, passes whatever its
# log holds; the line passes as a docker agent's reaches the log too, or in the
# agent's debug log, which verify_ran then removes. proven_of
# prints the KEPLOY_PROVEN value its arguments set, the last one, or nothing.
set -u
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fails=0 cases=0

# load [NAME=value] CODE [ARG...]: sources sudo_env.sh in a fresh bash with
# only that variable of the ones it reads, then runs CODE there with ARG... as
# its "$@".
load() {
  local set=$1 code=$2
  shift 2
  env -u KEPLOY_EXACT_VIEW_VERIFY ${set:+"$set"} bash -c 'source "$1/sudo_env.sh" || exit 9; code=$2; shift 2; eval "$code"' _ "$here" "$code" "$@"
}

# sudo_env_is NAME [NAME=value] WANT: SUDO_ENV, its entries joined by "|",
# is WANT.
sudo_env_is() {
  local name=$1 set=$2 want=$3 got
  cases=$((cases + 1))
  got=$(load "$set" 'IFS="|"; echo "${SUDO_ENV[*]}"')
  if [ "$got" != "$want" ]; then
    echo "FAIL $name: SUDO_ENV is '$got', want '$want'"
    fails=$((fails + 1))
  fi
}

sudo_env_is unset "" ""
sudo_env_is one KEPLOY_EXACT_VIEW_VERIFY=1 "KEPLOY_EXACT_VIEW_VERIFY=1"
sudo_env_is empty KEPLOY_EXACT_VIEW_VERIFY= "KEPLOY_EXACT_VIEW_VERIFY="
sudo_env_is spaced "KEPLOY_EXACT_VIEW_VERIFY=a b" "KEPLOY_EXACT_VIEW_VERIFY=a b"

LINE="2026-11-05T00:00:00Z INFO Keploy(agent): the exact view's verify mode is on in this agent: each read it keeps is recomputed"
printf 'start\n%s\nend\n' "$LINE" > "$tmp/with.log"
# An agent in docker: its lines reach the keploy CLI's own output, the log a
# lane keeps, through docker's attach stream (`docker run`) and through
# compose's, after the agent's service name.
printf 'start\n\360\237\220\260 Keploy(agent): 2026-11-05T00:00:00Z\t\033[34mINFO\033[0m\tthe exact view'"'"'s verify mode is on in this agent: each read it keeps is recomputed\nend\n' > "$tmp/docker-run.log"
printf 'start\nkeploy-v3-b12d63f2eb5731bb  | \360\237\220\260 Keploy(agent): 2026-11-05T00:00:00Z\t\033[34mINFO\033[0m\tthe exact view'"'"'s verify mode is on in this agent: each read it keeps is recomputed\nend\n' > "$tmp/compose.log"
# without.log: what a run whose agent did not run in verify mode says, near
# misses included (the agent's other switch line, and a verify-mode line that
# is not the agent's "on").
printf 'start\nthe proven model'"'"'s staging switch is on in this agent\nthe exact view'"'"'s verify mode is off\nend\n' > "$tmp/without.log"

# verify_is NAME [NAME=value] PROVEN LOG WANT [AGENT_LOG]: verify_ran PROVEN
# LOG [AGENT_LOG] returns WANT (0 or 1), and says FAIL with the log's name
# exactly when it returns 1.
verify_is() {
  local name=$1 set=$2 proven=$3 log=$4 want=$5 out rc
  cases=$((cases + 1))
  out=$(load "$set" 'verify_ran "$@"' "$proven" "$log" ${6:+"$6"})
  rc=$?
  if [ "$rc" != "$want" ]; then
    echo "FAIL $name: verify_ran returned $rc, want $want (it said: $out)"
    fails=$((fails + 1))
  elif [ "$want" = 1 ] && ! grep -qF "FAIL: $log" <<< "$out"; then
    echo "FAIL $name: verify_ran failed without saying which log: '$out'"
    fails=$((fails + 1))
  elif [ "$want" = 0 ] && [ -n "$out" ]; then
    echo "FAIL $name: verify_ran passed and printed '$out'"
    fails=$((fails + 1))
  fi
}

verify_is proven-line KEPLOY_EXACT_VIEW_VERIFY=1 1 "$tmp/with.log" 0
verify_is proven-no-line KEPLOY_EXACT_VIEW_VERIFY=1 1 "$tmp/without.log" 1
verify_is proven-no-log KEPLOY_EXACT_VIEW_VERIFY=1 1 "$tmp/missing.log" 1
verify_is switch-off KEPLOY_EXACT_VIEW_VERIFY=1 "" "$tmp/without.log" 0
verify_is switch-zero KEPLOY_EXACT_VIEW_VERIFY=1 0 "$tmp/without.log" 0
verify_is verify-unset "" 1 "$tmp/without.log" 0
verify_is verify-zero KEPLOY_EXACT_VIEW_VERIFY=0 1 "$tmp/without.log" 0
verify_is docker-run KEPLOY_EXACT_VIEW_VERIFY=1 1 "$tmp/docker-run.log" 0
verify_is compose KEPLOY_EXACT_VIEW_VERIFY=1 1 "$tmp/compose.log" 0

# An agent whose lines compose did not print (`docker compose up app`): its
# debug file holds them. Each check removes the file it read, so the next
# run's check cannot pass on this run's line.
cp "$tmp/with.log" "$tmp/agent-debug.log"
verify_is agent-log KEPLOY_EXACT_VIEW_VERIFY=1 1 "$tmp/without.log" 0 "$tmp/agent-debug.log"
cases=$((cases + 1))
if [ -e "$tmp/agent-debug.log" ]; then
  echo "FAIL agent-log-removed: verify_ran left the agent's debug log it read"
  fails=$((fails + 1))
fi
verify_is agent-log-read-once KEPLOY_EXACT_VIEW_VERIFY=1 1 "$tmp/without.log" 1 "$tmp/agent-debug.log"
cp "$tmp/without.log" "$tmp/agent-debug.log"
verify_is agent-log-no-line KEPLOY_EXACT_VIEW_VERIFY=1 1 "$tmp/without.log" 1 "$tmp/agent-debug.log"
cp "$tmp/without.log" "$tmp/agent-debug.log"
verify_is agent-log-in-log KEPLOY_EXACT_VIEW_VERIFY=1 1 "$tmp/with.log" 0 "$tmp/agent-debug.log"
cp "$tmp/with.log" "$tmp/agent-debug.log"
verify_is agent-log-switch-off KEPLOY_EXACT_VIEW_VERIFY=1 "" "$tmp/without.log" 0 "$tmp/agent-debug.log"
cases=$((cases + 1))
if [ ! -e "$tmp/agent-debug.log" ]; then
  echo "FAIL agent-log-kept-off: verify_ran removed an agent's debug log off the switch, which it does not read"
  fails=$((fails + 1))
fi
# A debug log it cannot remove (a folder it cannot write, and a sudo that
# refuses) fails the run, though the line is there: a later check could read
# it. Root removes it whatever the folder's mode, so the case needs a user.
if [ "$(id -u)" != 0 ]; then
  cases=$((cases + 1))
  mkdir "$tmp/ro" "$tmp/nosudo"
  cp "$tmp/with.log" "$tmp/ro/agent-debug.log"
  printf '#!/bin/sh\nexit 1\n' > "$tmp/nosudo/sudo"
  chmod +x "$tmp/nosudo/sudo"
  chmod a-w "$tmp/ro"
  out=$(env KEPLOY_EXACT_VIEW_VERIFY=1 PATH="$tmp/nosudo:$PATH" bash -c 'source "$1/sudo_env.sh" || exit 9; verify_ran 1 "$2" "$3"' _ "$here" "$tmp/with.log" "$tmp/ro/agent-debug.log")
  rc=$?
  chmod u+w "$tmp/ro"
  if [ "$rc" != 1 ] || ! grep -qF "FAIL: $tmp/ro/agent-debug.log: could not remove" <<< "$out"; then
    echo "FAIL agent-log-unremovable: verify_ran returned $rc, want 1 saying it could not remove the log (it said: $out)"
    fails=$((fails + 1))
  fi
  # The same folder, and a sudo that removes it, as root would: it passes,
  # and the log is gone.
  cases=$((cases + 1))
  printf '#!/bin/sh\n[ "$1" = -n ] && shift\nfor a; do last=$a; done\nchmod u+w "$(dirname "$last")" && exec "$@"\n' > "$tmp/nosudo/sudo"
  chmod a-w "$tmp/ro"
  out=$(env KEPLOY_EXACT_VIEW_VERIFY=1 PATH="$tmp/nosudo:$PATH" bash -c 'source "$1/sudo_env.sh" || exit 9; verify_ran 1 "$2" "$3"' _ "$here" "$tmp/with.log" "$tmp/ro/agent-debug.log")
  rc=$?
  chmod u+w "$tmp/ro"
  if [ "$rc" != 0 ] || [ -e "$tmp/ro/agent-debug.log" ]; then
    echo "FAIL agent-log-sudo-removes: verify_ran returned $rc with the log $( [ -e "$tmp/ro/agent-debug.log" ] && echo left || echo removed), want 0 and removed by sudo (it said: $out)"
    fails=$((fails + 1))
  fi
fi

# proven_is NAME WANT [ARG...]: proven_of ARG... prints WANT.
proven_is() {
  local name=$1 want=$2 got
  shift 2
  cases=$((cases + 1))
  got=$(load "" 'proven_of "$@"' "$@")
  if [ "$got" != "$want" ]; then
    echo "FAIL $name: proven_of printed '$got', want '$want'"
    fails=$((fails + 1))
  fi
}

proven_is none ""
proven_is others "" OVERFLOW=1 KEPLOY_PROVENX=1 XKEPLOY_PROVEN=1
proven_is one 1 KEPLOY_PROVEN=1
proven_is among 1 OVERFLOW=1 KEPLOY_PROVEN=1 MODE=x
proven_is last-wins "" KEPLOY_PROVEN=1 KEPLOY_PROVEN=
proven_is spaced "a b" "KEPLOY_PROVEN=a b"

if [ "$fails" -ne 0 ]; then
  echo "sudo_env.sh: $fails case(s) failed"
  exit 1
fi
echo "sudo_env.sh: all $cases cases passed"
