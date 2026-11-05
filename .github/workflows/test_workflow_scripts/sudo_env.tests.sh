#!/usr/bin/env bash
# Tests for sudo_env.sh's SUDO_ENV and verify_ran. Run by
# sudo-env-scripts.yml, or locally (it needs only bash and grep, and leaves
# nothing behind):
#
#   bash .github/workflows/test_workflow_scripts/sudo_env.tests.sh
#
# SUDO_ENV names each of its switches (KEPLOY_EXACT_VIEW_VERIFY,
# KEPLOY_PG_V3_LOG_BIND_VALUES) with its value while it is set, an empty value
# included, and nothing while it is unset, and every KEPLOY_ variable a
# workflow's env: block sets is one of them or one keploy does not read. Every
# script that sources sudo_env.sh does so by its own path, never through the
# workspace, so it loads where a workflow checks keploy out at a path; a script
# that copies a script's functions into a file to run them there checks that
# the copy loads sudo_env.sh, unless the script it copies from loads none.
# verify_ran passes a run under the proven switch whose log holds the agent's
# verify line, and fails one whose log lacks it or does not exist, saying so; a
# run without the switch, or with KEPLOY_EXACT_VIEW_VERIFY other than 1, passes
# whatever its log holds; the line passes as a docker agent's reaches the log
# too, or in the agent's debug log, which verify_ran then removes. proven_of
# prints the KEPLOY_PROVEN value its arguments set, the last one, or nothing.
set -u
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fails=0 cases=0

# The switches sudo_env.sh reads (SUDO_SWITCHES), each unset for load.
unset_switches=()
for n in $(env -u KEPLOY_EXACT_VIEW_VERIFY bash -c 'source "$1/sudo_env.sh" >/dev/null && echo "${SUDO_SWITCHES[*]}"' _ "$here"); do
  unset_switches+=(-u "$n")
done

# load [NAME=value[|NAME=value...]] CODE [ARG...]: sources sudo_env.sh in a
# fresh bash with only those variables of the ones it reads, then runs CODE
# there with ARG... as its "$@".
load() {
  local set=$1 code=$2 sets=()
  shift 2
  [ -z "$set" ] || IFS='|' read -r -a sets <<<"$set"
  env "${unset_switches[@]}" "${sets[@]}" bash -c 'source "$1/sudo_env.sh" || exit 9; code=$2; shift 2; eval "$code"' _ "$here" "$code" "$@"
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
sudo_env_is pg KEPLOY_PG_V3_LOG_BIND_VALUES=1 "KEPLOY_PG_V3_LOG_BIND_VALUES=1"
sudo_env_is pg-empty KEPLOY_PG_V3_LOG_BIND_VALUES= "KEPLOY_PG_V3_LOG_BIND_VALUES="
sudo_env_is both "KEPLOY_PG_V3_LOG_BIND_VALUES=1|KEPLOY_EXACT_VIEW_VERIFY=1" "KEPLOY_EXACT_VIEW_VERIFY=1|KEPLOY_PG_V3_LOG_BIND_VALUES=1"
sudo_env_is other-keploy-name "KEPLOY_PROVEN=1|KEPLOY_EXACT_VIEW_VERIFY=1" "KEPLOY_EXACT_VIEW_VERIFY=1"

# Every KEPLOY_ variable a workflow sets in an env: block is a switch SUDO_ENV
# names, or one keploy does not read: a script's own (the binary or flags it
# runs, a go test filter, an earlier step's outcome) or go test's own
# KEPLOY_TEST_ switches, which go-test.yaml names on its sudo lines. A new
# switch fails here until it is in one of them, so no sudo'd keploy run drops
# it unseen.
NOT_READ=" KEPLOY_BIN KEPLOY_FLAGS KEPLOY_E2E_MYSQL_RUN KEPLOY_START_STEP_OUTCOME "
switches=" $(load "" 'echo "${SUDO_SWITCHES[*]}"') "
# unnamed_switches DIR: each KEPLOY_ name a workflow in DIR sets in an env:
# block that is neither a switch nor in NOT_READ, as "FILE sets NAME".
unnamed_switches() {
  local wf n
  for wf in "$1"/*.yml "$1"/*.yaml; do
    [ -e "$wf" ] || continue
    for n in $(grep -oE '^[[:space:]]+KEPLOY_[A-Z0-9_]+:' "$wf" | tr -d ' :' | sort -u); do
      case $n in KEPLOY_TEST_*) continue ;; esac
      case "$switches$NOT_READ" in *" $n "*) continue ;; esac
      echo "$(basename "$wf") sets $n"
    done
  done
}
# unnamed_is NAME DIR WANT: unnamed_switches DIR, its lines joined by "|", is
# WANT.
unnamed_is() {
  local got
  cases=$((cases + 1))
  got=$(unnamed_switches "$2" | paste -sd'|')
  if [ "$got" != "$3" ]; then
    echo "FAIL $1: workflow KEPLOY_ names neither in sudo_env.sh's SUDO_SWITCHES nor in NOT_READ here: '$got', want '$3'"
    fails=$((fails + 1))
  fi
}
unnamed_is workflows "$here/.." ""
mkdir -p "$tmp/wf"
printf 'env:\n  KEPLOY_EXACT_VIEW_VERIFY: 1\njobs:\n  a:\n    env:\n      KEPLOY_TEST_X: 1\n    steps:\n      - env:\n          KEPLOY_BIN: k\n          KEPLOY_PG_V3_LOG_BIND_VALUES: "1"\n' > "$tmp/wf/known.yml"
unnamed_is known-names "$tmp/wf" ""
printf 'jobs:\n  a:\n    env:\n      KEPLOY_NEW_YML: 1\n' > "$tmp/wf/new-yml.yml"
unnamed_is new-switch-yml "$tmp/wf" "new-yml.yml sets KEPLOY_NEW_YML"
printf 'jobs:\n  a:\n    steps:\n      - env:\n          KEPLOY_NEW_SWITCH: 1\n        run: echo "KEPLOY_IN_RUN: 1"\n' > "$tmp/wf/new.yaml"
unnamed_is new-switch-yaml "$tmp/wf" "new-yml.yml sets KEPLOY_NEW_YML|new.yaml sets KEPLOY_NEW_SWITCH"

# A script sources sudo_env.sh by its own path, never through the workspace:
# sampling-test.yml and keploy-performance-test.yml check keploy out at
# `path: keploy`, where $GITHUB_WORKSPACE/.github does not exist.
# sourced_from_workspace DIR: each line of a script under DIR that sources
# sudo_env.sh through $GITHUB_WORKSPACE or a path from the workspace (one that
# names .github/), as "FILE:LINE".
sourced_from_workspace() {
  local f n line
  grep -rnE --include='*.sh' '^[^#]*(source|\.)[[:space:]].*sudo_env\.sh' "$1" |
    while IFS=: read -r f n line; do
      case $line in *GITHUB_WORKSPACE* | *.github/*) echo "${f#"$1"/}:$n" ;; esac
    done
}
# from_workspace_is NAME DIR WANT: sourced_from_workspace DIR, its lines joined
# by "|", is WANT.
from_workspace_is() {
  local got
  cases=$((cases + 1))
  got=$(sourced_from_workspace "$2" | sort | paste -sd'|')
  if [ "$got" != "$3" ]; then
    echo "FAIL $1: scripts that source sudo_env.sh through the workspace, not by their own path: '$got', want '$3'"
    fails=$((fails + 1))
  fi
}
from_workspace_is scripts "$here/../.." ""
mkdir -p "$tmp/src/a"
name=sudo_env
printf '# source "$GITHUB_WORKSPACE/%s.sh" in a comment\nsource "$(dirname "${BASH_SOURCE[0]}")/../%s.sh" || exit 1\n' "$name" "$name" > "$tmp/src/a/own.sh"
printf 'set -e\nsource "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/%s.sh" || exit 1\n' "$name" > "$tmp/src/a/ws.sh"
printf '. ./.github/workflows/test_workflow_scripts/%s.sh\n' "$name" > "$tmp/src/rel.sh"
printf 'source "$GITHUB_WORKSPACE/scripts/%s.sh"\n' "$name" > "$tmp/src/ws-only.sh"
from_workspace_is workspace-forms "$tmp/src" "a/ws.sh:2|rel.sh:1|ws-only.sh:1"

# The same scripts in GitHub's layout for `path: keploy`: the repository under
# $GITHUB_WORKSPACE/keploy and nothing at the workspace root.
# path_checkout DIR: lays out, in that layout, each script under DIR that
# sources sudo_env.sh by its own directory, holding only that source line;
# runs it by its path and sources it, from the workspace root; and prints each
# that did not load sudo_env.sh, as "FILE(bash)" or "FILE(source)", or
# "no script" when DIR has none.
path_checkout() {
  local ws lay f line rel how got laid=0
  ws=$(mktemp -d "$tmp/ws.XXXXXX")
  lay="$ws/keploy/.github/workflows/test_workflow_scripts"
  mkdir -p "$lay"
  cp "$here/sudo_env.sh" "$lay/"
  while IFS=: read -r f _ line; do
    rel=${f#"$1"/}
    mkdir -p "$lay/$(dirname "$rel")"
    printf '%s\ndeclare -F verify_ran >/dev/null && echo loaded\n' "$line" > "$lay/$rel"
    laid=$((laid + 1))
    for how in bash source; do
      got=$(cd "$ws" && env "${unset_switches[@]}" GITHUB_WORKSPACE="$ws" bash -c '"$1" "$2"' _ "$how" "keploy/.github/workflows/test_workflow_scripts/$rel" 2>&1)
      grep -qx loaded <<<"$got" || echo "$rel($how)"
    done
  done < <(grep -rnE --include='*.sh' '^[[:space:]]*source "\$\(dirname "\$\{BASH_SOURCE\[0\]\}"\)/[./]*sudo_env\.sh"' "$1")
  [ "$laid" -gt 0 ] || echo "no script"
}
# path_checkout_is NAME DIR WANT: path_checkout DIR, its lines joined by "|",
# is WANT.
path_checkout_is() {
  local got
  cases=$((cases + 1))
  got=$(path_checkout "$2" | sort | paste -sd'|')
  if [ "$got" != "$3" ]; then
    echo "FAIL $1: scripts that did not load sudo_env.sh with keploy checked out at a path: '$got', want '$3'"
    fails=$((fails + 1))
  fi
}
path_checkout_is path-checkout "$here" ""
mkdir -p "$tmp/pc/golang/x" "$tmp/none"
printf 'source "$(dirname "${BASH_SOURCE[0]}")/../../%s.sh" || exit 1\n' "$name" > "$tmp/pc/golang/x/own.sh"
printf 'source "$(dirname "${BASH_SOURCE[0]}")/../%s.sh" || exit 1\n' "$name" > "$tmp/pc/golang/x/short.sh"
path_checkout_is path-checkout-depth "$tmp/pc" "golang/x/short.sh(bash)|golang/x/short.sh(source)"
path_checkout_is path-checkout-none "$tmp/none" "no script"

# A script that copies another's functions into a file and runs them from
# there runs them away from that script's directory: what it loads at its top
# by its own path (sudo_env.sh's SUDO_ENV and verify_ran) is in the copy only
# if the copy loads it too, and a check of the copy is what fails the job when
# it does not (compose-proven-setend-linux.tests.sh's).
# copies_unchecked DIR: each line of a script under DIR that copies functions
# (awk or sed over a function's head, or declare, typeset or export -f), as
# "FILE:LINE", unless the script checks its copy has verify_ran (declare -F
# verify_ran) or every script the line names is under DIR and loads no
# sudo_env.sh. A line that names no script it can find is counted (the copy
# may be of a script that loads it).
copy_re='^[^#]*(\b(awk|sed)\b.*[]A-Za-z0-9_*+]\\*\(\\*\) *\\*\{|\b(declare|typeset|export) -f\b)'
copies_unchecked() {
  local f n line tok src uses
  grep -rnE --include='*.sh' "$copy_re" "$1" |
    while IFS=: read -r f n line; do
      grep -qE '^[^#]*declare -F verify_ran' "$f" && continue
      uses=unknown
      while IFS= read -r tok; do
        src=$(find "$1" -type f -name "$(basename "$tok")" | head -n1)
        [ -n "$src" ] || continue
        [ "$uses" = unknown ] && uses=no
        grep -qE '^[^#]*(SUDO_ENV|verify_ran|sudo_env\.sh)' "$src" && uses=yes
      done < <(grep -oE '[A-Za-z0-9_./-]+\.sh' <<<"$line")
      [ "$uses" = no ] || echo "${f#"$1"/}:$n"
    done
}
# copies_unchecked_is NAME DIR WANT: copies_unchecked DIR, its lines joined by
# "|", is WANT.
copies_unchecked_is() {
  local got
  cases=$((cases + 1))
  got=$(copies_unchecked "$2" | sort | paste -sd'|')
  if [ "$got" != "$3" ]; then
    echo "FAIL $1: scripts that copy functions into a file with no check that the copy loads sudo_env.sh: '$got', want '$3'"
    fails=$((fails + 1))
  fi
}
copies_unchecked_is copies "$here/../../.." ""
mkdir -p "$tmp/cp/sub"
# (Each fixture's command is a printf argument, so no line here is a copy.)
fn='() {'
printf "sed -n '/^record%s/,/^}/p' lane.sh >\"\$t/f.sh\"\nbash -c '. \"\$1\" && declare -F verify_ran' _ \"\$t/f.sh\" || exit 1\n" "$fn" > "$tmp/cp/checked.sh"
printf "%s -v names='^[a-z_]+\\\\\\\\(\\\\\\\\) \\\\\\\\{' '\$0 ~ names' \"\$lane\" >\"\$t/f.sh\"\n# declare -F verify_ran, in a comment only\n" awk > "$tmp/cp/sub/awk.sh"
printf '%s -f record\nbash -c record\n' export > "$tmp/cp/export.sh"
printf '# sed -n "/^record%s/,/^}/p", in a comment\necho "record%s"\n' "$fn" "$fn" > "$tmp/cp/not-a-copy.sh"
printf "x=\$(printf %%s \"\$1\" | sed 's/[][\\\\.*^\$+?%s}|]/\\\\\\\\&/g')\n" "(){" > "$tmp/cp/escape.sh"
printf 'helper() { echo hi; }\n' > "$tmp/cp/plain-lane.sh"
printf 'source "$(dirname "${BASH_SOURCE[0]}")/%s.sh" || exit 1\nhelper() { sudo env "${SUDO_ENV[@]}" true; }\n' "$name" > "$tmp/cp/env-lane.sh"
printf 'eval "$(sed -n "/^helper%s/,/^}/p" "$d/plain-lane.sh")"\n' "$fn" > "$tmp/cp/plain-copy.sh"
printf 'eval "$(sed -n "/^helper%s/,/^}/p" "$d/env-lane.sh")"\n' "$fn" > "$tmp/cp/env-copy.sh"
copies_unchecked_is copies-fixtures "$tmp/cp" "env-copy.sh:1|export.sh:1|sub/awk.sh:1"

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
