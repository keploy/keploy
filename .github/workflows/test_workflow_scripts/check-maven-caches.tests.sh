#!/bin/bash
# check-maven-caches.tests.sh: check-maven-caches.sh on this repository, and
# on a repository made for each case below. Run by the lint workflow, or
# locally (it leaves nothing behind):
#
#   bash .github/workflows/test_workflow_scripts/check-maven-caches.tests.sh
#
# It passes on this repository; on setup-java with no cache or another one
# (gradle); on setup-java-maven with its save step; on a comment, at the start
# of a line or after one, that names `cache: maven`; on a shell script (not
# YAML) that does; and where no file names it. It fails, naming the line, on
# `cache: maven` in a workflow (.yml and .yaml, quoted or not, with a comment
# after it, with CRLF line ends), in a composite action, in an action nested
# in another action's directory, and in flow style (`with: { cache: maven }`).
# It fails, naming the file, on setup-java-maven with no save step, or with
# fewer save steps than uses (a save only named in a comment is none).
set -euo pipefail

dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="$dir/check-maven-caches.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Nothing from the runner's or the user's git configuration (insteadOf rules,
# hooks, signing) reaches these repositories.
export GIT_CONFIG_GLOBAL="$tmp/gitconfig" GIT_CONFIG_NOSYSTEM=1
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t
: >"$GIT_CONFIG_GLOBAL"

fails=0
n=0

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

# wf WITH...: a workflow whose one step is setup-java with these lines.
c=cache # kept apart from its value, so this file never names the use itself
wf() {
  printf 'jobs:\n  t:\n    steps:\n      - uses: actions/setup-java@v4\n        with:\n          java-version: "17"\n'
  local l
  for l in "$@"; do printf '          %s\n' "$l"; done
}
action() {
  printf 'runs:\n  using: composite\n  steps:\n    - uses: actions/setup-java@v4\n      with:\n        java-version: "17"\n        %s\n' "$1"
}

n=$((n + 1))
rc=0
out=$(cd "$dir" && bash "$script" 2>&1) || rc=$?
if [ "$rc" -eq 0 ]; then echo "ok   this repository (pass)"; else
  echo "FAIL this repository: check ended fail (exit $rc); it said:"; printf '%s\n' "$out" | sed 's/^/    /'; fails=$((fails + 1))
fi

repo none-at-all ".github/workflows/w.yml=$(wf)"
expect none-at-all pass "No workflow or action"
repo gradle ".github/workflows/w.yml=$(wf "$c: gradle")"
expect gradle pass
own() { # own N SAVES: a workflow with N uses of setup-java-maven, SAVES save steps
  printf 'jobs:\n  t:\n    steps:\n'
  local i
  for i in $(seq 1 "$1"); do
    printf '      - uses: ./.github/actions/setup-java-maven\n        with:\n          java-version: "17"\n          cache-dependency-path: pom.xml\n      - run: mvn clean package\n'
  done
  for i in $(seq 1 "$2"); do printf '      - uses: ./.github/actions/setup-java-maven/save\n'; done
}
repo own-action ".github/workflows/w.yml=$(own 1 1)"
expect own-action pass "every use of setup-java-maven has its save step"
repo own-two ".github/workflows/w.yml=$(own 2 2)"
expect own-two pass
repo own-no-save ".github/workflows/w.yml=$(own 1 0)"
expect own-no-save fail ".github/workflows/w.yml: 1 use(s) of setup-java-maven, 0 of setup-java-maven/save"
repo own-one-save ".github/workflows/w.yml=$(own 2 1)"
expect own-one-save fail ".github/workflows/w.yml: 2 use(s) of setup-java-maven, 1 of setup-java-maven/save"
repo own-save-comment ".github/workflows/w.yml=$(own 1 0)
      # - uses: ./.github/actions/setup-java-maven/save
"
expect own-save-comment fail ".github/workflows/w.yml: 1 use(s) of setup-java-maven, 0 of setup-java-maven/save"
repo comments ".github/workflows/w.yml=$(wf "# $c: maven was setup-java's" "distribution: temurin  # not $c: maven")"
expect comments pass
repo shell-script ".github/workflows/s.sh=echo '$c: maven'
"
expect shell-script pass

repo plain ".github/workflows/w.yml=$(wf "$c: maven")"
expect plain fail ".github/workflows/w.yml:7:" "setup-java-maven"
repo quoted ".github/workflows/w.yaml=$(wf "$c: 'maven'" "cache-dependency-path: pom.xml")"
expect quoted fail ".github/workflows/w.yaml:7:"
repo double-quoted-comment ".github/workflows/w.yml=$(wf "$c: \"maven\"  # setup-java's")"
expect double-quoted-comment fail ".github/workflows/w.yml:7:"
repo crlf ".github/workflows/w.yml=$(wf "$c: maven" | sed 's/$/\r/')"
expect crlf fail ".github/workflows/w.yml:7:"
repo composite ".github/actions/x/action.yml=$(action "$c: maven")"
expect composite fail ".github/actions/x/action.yml:7:"
repo nested ".github/actions/x/inner/action.yaml=$(action "$c: maven")"
expect nested fail ".github/actions/x/inner/action.yaml:7:"
repo flow ".github/workflows/w.yml=jobs:
  t:
    steps:
      - { uses: actions/setup-java@v4, with: { java-version: \"17\", $c: maven } }
"
expect flow fail ".github/workflows/w.yml:4:"

echo "$((n - fails))/$n passed"
[ "$fails" -eq 0 ]
