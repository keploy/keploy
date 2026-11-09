#!/bin/bash
# check-artifact-downloads.tests.sh: check-artifact-downloads.sh on this
# repository, and on a repository made for each case below. Run by the lint
# workflow, or locally (it leaves nothing behind):
#
#   bash .github/workflows/test_workflow_scripts/check-artifact-downloads.tests.sh
#
# It passes on this repository; on a wrapper whose tries are all v8 or later
# (v8, v8.0.2, v10, quoted, with a comment after); on a comment or a line of
# prose that names an old version, which is not a use; and where nothing
# downloads an artifact. It fails, naming the line, on a direct use in a
# workflow (quoted or not) or in another action, a use in another file of the wrapper's
# directory, a direct use with no wrapper at all, a wrapper that tries once,
# and a try that is older than v8 (v7, v4) or not at a version tag (main, a
# SHA, v8x).
set -euo pipefail

dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$dir/check-artifact-downloads.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Nothing from the runner's or the user's git configuration (insteadOf rules,
# hooks, signing) reaches these repositories.
export GIT_CONFIG_GLOBAL="$tmp/gitconfig" GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t
: >"$GIT_CONFIG_GLOBAL"

fails=0
n=0
wrapper=.github/actions/download-artifact-retry/action.yml

# try REF: one try of the wrapper at REF, as the wrapper writes it.
try() {
  printf '    - id: try\n      uses: actions/download-artifact@%s\n      with:\n        name: x\n' "$1"
}

# repo NAME PATH=CONTENT...: a git repository of its own, holding each file.
repo() {
  local r="$tmp/$1" spec path
  shift
  mkdir -p "$r"
  git init -q "$r"
  for spec in "$@"; do
    path=${spec%%=*}
    mkdir -p "$r/$(dirname "$path")"
    printf '%s' "${spec#*=}" >"$r/$path"
  done
  git -C "$r" add -A
  git -C "$r" commit -q --allow-empty -m t
}

# expect CASE pass|fail [TEXT...]: the check on repository CASE ends so, and
# its output holds each TEXT.
expect() {
  local name=$1 want=$2 out rc=0 got text
  shift 2
  n=$((n + 1))
  out=$(cd "$tmp/$name" && bash "$script" 2>&1) || rc=$?
  got=pass
  [ "$rc" -eq 0 ] || got=fail
  if [ "$got" != "$want" ]; then
    echo "FAIL $name: check ended $got (exit $rc), want $want; it said:"
    printf '%s\n' "$out" | sed 's/^/    /'
    fails=$((fails + 1))
    return
  fi
  for text in "$@"; do
    if ! printf '%s\n' "$out" | grep -q -F -- "$text"; then
      echo "FAIL $name: output lacks '$text'; it said:"
      printf '%s\n' "$out" | sed 's/^/    /'
      fails=$((fails + 1))
      return
    fi
  done
  echo "ok   $name ($want)"
}

wf_use() {
  printf 'jobs:\n  t:\n    steps:\n      - uses: actions/checkout@v4\n      - uses: actions/download-artifact@%s\n        with:\n          name: x\n' "$1"
}
wf_wrapped='jobs:
  t:
    steps:
      - uses: actions/checkout@v4
      - uses: ./.github/actions/download-artifact-retry
        with:
          name: x
'
three="runs:
  steps:
$(try v8)
$(try v8)
$(try v8)
"

# This repository, as CI checks it.
n=$((n + 1))
if out=$(bash "$script" 2>&1); then
  echo "ok   this repository (pass)"
else
  echo "FAIL this repository: $out"
  fails=$((fails + 1))
fi

repo wrapped "$wrapper=$three" ".github/workflows/w.yml=$wf_wrapped"
expect wrapped pass "goes through $wrapper" "up to 3 times"

# A quoted use, written so that this file holds no line the check takes
# for a use of its own.
quoted=$(printf '    - id: quoted\n      uses: "actions/%s@v8"' download-artifact)
repo refs-ok "$wrapper=runs:
  steps:
$(try v8.0.2)
$quoted
$(try 'v10 # a newer major')
"
expect refs-ok pass "up to 3 times"

repo prose "$wrapper=$three" ".github/workflows/w.yml=# uses: actions/download-artifact@v4 is what this was
$wf_wrapped" ".github/CI.md=Call actions/download-artifact@v4 through the wrapper.
"
expect prose pass "up to 3 times"

repo none ".github/workflows/w.yml=jobs: {}
"
expect none pass "No workflow or action uses actions/download-artifact."

repo direct-workflow "$wrapper=$three" ".github/workflows/w.yml=$(wf_use v8)"
expect direct-workflow fail "use actions/download-artifact directly" ".github/workflows/w.yml:5:"

repo direct-quoted "$wrapper=$three" ".github/workflows/w.yml=jobs:
  t:
    steps:
$(printf "      - uses: 'actions/%s@v8'" download-artifact)
"
expect direct-quoted fail "use actions/download-artifact directly" ".github/workflows/w.yml:4:"

repo direct-action "$wrapper=$three" ".github/actions/get/action.yml=runs:
  steps:
$(try v8)
"
expect direct-action fail "use actions/download-artifact directly" ".github/actions/get/action.yml:4:"

repo beside-wrapper "$wrapper=$three" ".github/actions/download-artifact-retry/other.yml=runs:
  steps:
$(try v8)
"
expect beside-wrapper fail "use actions/download-artifact directly" "download-artifact-retry/other.yml:4:"

repo no-wrapper ".github/workflows/w.yml=$(wf_use v8)"
expect no-wrapper fail "use actions/download-artifact directly" "0 time(s)"

repo once "$wrapper=runs:
  steps:
$(try v8)
"
expect once fail "uses actions/download-artifact 1 time(s)"

repo v7 "$wrapper=runs:
  steps:
$(try v8)
$(try v7)
"
expect v7 fail "older than v8" "$wrapper:8:"

repo v4 "$wrapper=runs:
  steps:
$(try v4.3.0)
$(try v8)
"
expect v4 fail "older than v8" "$wrapper:4:"

repo branch "$wrapper=runs:
  steps:
$(try main)
$(try v8)
"
expect branch fail "not a version tag" "$wrapper:4:"

repo sha "$wrapper=runs:
  steps:
$(try 9000827ccba6bdab643e8b6fd33ac0654aef8333)
$(try v8)
"
expect sha fail "not a version tag" "$wrapper:4:"

repo v8x "$wrapper=runs:
  steps:
$(try v8)
$(try v8x)
"
expect v8x fail "not a version tag" "$wrapper:8:"

echo "$((n - fails)) of $n cases passed"
[ "$fails" -eq 0 ]
