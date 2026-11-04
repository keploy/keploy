#!/usr/bin/env bash
# Tests for mock/sudo_env.sh's SUDO_ENV and verify_ran. Run by
# linux-ci-scripts.yml, or locally (it needs only bash and grep, and leaves
# nothing behind):
#
#   bash .github/workflows/test_workflow_scripts/mock/sudo_env.tests.sh
#
# SUDO_ENV names KEPLOY_EXACT_VIEW_VERIFY with its value while it is set, an
# empty value included, and nothing while it is unset. verify_ran passes a run
# under the proven switch whose log holds the agent's verify line, and fails
# one whose log lacks it or does not exist, saying so; a run without the
# switch, or with KEPLOY_EXACT_VIEW_VERIFY other than 1, passes whatever its
# log holds.
set -u
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fails=0

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
printf 'start\nthe proven model'"'"'s staging switch is on in this agent\nend\n' > "$tmp/without.log"

# verify_is NAME [NAME=value] PROVEN LOG WANT: verify_ran PROVEN LOG returns
# WANT (0 or 1), and says FAIL with the log's name exactly when it returns 1.
verify_is() {
  local name=$1 set=$2 proven=$3 log=$4 want=$5 out rc
  out=$(load "$set" 'verify_ran "$1" "$2"' "$proven" "$log")
  rc=$?
  if [ "$rc" != "$want" ]; then
    echo "FAIL $name: verify_ran returned $rc, want $want (it said: $out)"
    fails=$((fails + 1))
  elif [ "$want" = 1 ] && ! grep -qF "FAIL: $log: " <<< "$out"; then
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

if [ "$fails" -ne 0 ]; then
  echo "sudo_env.sh: $fails case(s) failed"
  exit 1
fi
echo "sudo_env.sh: all 11 cases passed"
