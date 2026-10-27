#!/bin/bash
# golangci-new-from.tests.sh: golangci-new-from.sh against a keploy/keploy of
# its own, fetched over file:// as actions/checkout fetches the real one.
#
# Its main has a commit b0 and a later b1 that changes base.go; a pull request
# off b0 changes pr.go, and its merge commit (GitHub's refs/pull/N/merge) was
# made on b0, before main moved to b1. A checkout of the whole history picks
# --new-from-merge-base=origin/main for the pull request, whose merge base is
# b0: git's diff from it names pr.go and not base.go, so main's later commits
# are never taken for the pull request's. A push picks --new-from-rev=<before>,
# and a push that creates the branch (before all zeros) the merge base with
# the default branch. Each of these fails, names what is missing and writes
# no flag: a checkout one commit deep (actions/checkout's default), a base
# branch or before commit the checkout lacks, a base branch with no history in
# common with HEAD (an orphan branch), an empty GITHUB_BASE_REF, BEFORE
# or DEFAULT_BRANCH, an event the script does not handle, no GITHUB_OUTPUT,
# and a directory git status fails in.
set -euo pipefail

script="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/golangci-new-from.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# Nothing from the runner's or the user's git configuration (insteadOf rules,
# hooks, signing) reaches these repositories.
export GIT_CONFIG_GLOBAL="$tmp/gitconfig" GIT_CONFIG_NOSYSTEM=1
# git looks for no repository above $tmp, so "no repository" stays one.
export GIT_CEILING_DIRECTORIES="$tmp"
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t
: >"$GIT_CONFIG_GLOBAL"

up="$tmp/up"
git init -q -b main "$up"
(
  cd "$up"
  printf 'package p\n\nfunc A() {}\n' >base.go
  printf 'package p\n\nfunc B() {}\n' >pr.go
  git add -A && git commit -qm b0
  git tag b0
  git checkout -qb pr
  printf 'package p\n\nfunc B() { _ = 1 }\n' >pr.go
  git commit -qam p1
  git checkout -q --detach b0
  git merge -q --no-ff -m merge pr
  git update-ref refs/pull/1/merge HEAD
  git checkout -q main
  printf 'package p\n\nfunc A() { _ = 2 }\n' >base.go
  git commit -qam b1
  git checkout -q --orphan unrelated
  git commit -qm u0
  git checkout -q main
)
b0=$(git -C "$up" rev-parse b0)
merge=$(git -C "$up" rev-parse refs/pull/1/merge)

# checkout DIR [DEPTH]: as actions/checkout does for the merge commit, with
# every branch when DEPTH is 0.
checkout() {
  git init -q "$1"
  git -C "$1" remote add origin "file://$up"
  if [[ ${2:-0} == 0 ]]; then
    git -C "$1" fetch -q origin '+refs/heads/*:refs/remotes/origin/*' '+refs/tags/*:refs/tags/*'
    git -C "$1" fetch -q origin "+$merge:refs/remotes/pull/1/merge"
  else
    git -C "$1" fetch -q --depth="$2" origin "+$merge:refs/remotes/pull/1/merge"
  fi
  git -C "$1" checkout -q --detach "$merge"
}
checkout "$tmp/full"
checkout "$tmp/shallow" 1
mkdir "$tmp/norepo"

pass=0 failed=0
ok() { echo "ok   $1"; pass=$((pass + 1)); }
bad() { echo "FAIL $1"; failed=$((failed + 1)); }

# run DIR VAR=VALUE...: the script in DIR with only those inputs set.
run() {
  local dir=$1
  shift
  out="$tmp/output" && : >"$out"
  log=$( cd "$dir" && env -u GITHUB_EVENT_NAME -u GITHUB_BASE_REF -u BEFORE -u DEFAULT_BRANCH \
    GITHUB_OUTPUT="$out" "$@" bash "$script" 2>&1 ) && rc=0 || rc=$?
}

# expect NAME FLAG DIR VAR=VALUE...: exits 0 and writes exactly flag=FLAG.
expect() {
  local name=$1 flag=$2
  shift 2
  run "$@"
  if [[ $rc == 0 && $(cat "$out") == "flag=$flag" ]]; then ok "$name"; else
    bad "$name: rc=$rc, output '$(cat "$out")', want flag=$flag"; echo "$log"; fi
}

# refuse NAME TEXT DIR VAR=VALUE...: exits non-zero, says TEXT, writes nothing.
refuse() {
  local name=$1 text=$2
  shift 2
  run "$@"
  if [[ $rc != 0 && ! -s $out && $log == *"$text"* ]]; then ok "$name"; else
    bad "$name: rc=$rc, output '$(cat "$out")', want a failure saying '$text'"; echo "$log"; fi
}

expect "pull request: merge base with its base branch" "--new-from-merge-base=origin/main" \
  "$tmp/full" GITHUB_EVENT_NAME=pull_request GITHUB_BASE_REF=main
mb=$(git -C "$tmp/full" merge-base origin/main HEAD)
changed=$(git -C "$tmp/full" diff --name-only "$mb" --)
if [[ $mb == "$b0" && $changed == pr.go ]]; then ok "pull request: main's later commit is not in its diff"; else
  bad "pull request: merge base $mb (want b0 $b0), diff names '$changed' (want pr.go)"; fi

expect "push: from the before commit" "--new-from-rev=$b0" \
  "$tmp/full" GITHUB_EVENT_NAME=push BEFORE="$b0" DEFAULT_BRANCH=main
expect "push creating the branch: merge base with the default branch" "--new-from-merge-base=origin/main" \
  "$tmp/full" GITHUB_EVENT_NAME=push BEFORE=0000000000000000000000000000000000000000 DEFAULT_BRANCH=main

refuse "pull request, checkout one commit deep" "fetch-depth: 0" \
  "$tmp/shallow" GITHUB_EVENT_NAME=pull_request GITHUB_BASE_REF=main
refuse "push, checkout one commit deep" "fetch-depth: 0" \
  "$tmp/shallow" GITHUB_EVENT_NAME=push BEFORE="$b0" DEFAULT_BRANCH=main
refuse "pull request, base branch not fetched" "origin/release-9 is not in the checkout" \
  "$tmp/full" GITHUB_EVENT_NAME=pull_request GITHUB_BASE_REF=release-9
refuse "pull request, empty GITHUB_BASE_REF" "GITHUB_BASE_REF is empty" \
  "$tmp/full" GITHUB_EVENT_NAME=pull_request GITHUB_BASE_REF=
refuse "push, before commit not in the checkout" "1234567890123456789012345678901234567890 is not in the checkout" \
  "$tmp/full" GITHUB_EVENT_NAME=push BEFORE=1234567890123456789012345678901234567890 DEFAULT_BRANCH=main
refuse "push, empty BEFORE" "BEFORE (github.event.before) is empty" \
  "$tmp/full" GITHUB_EVENT_NAME=push BEFORE= DEFAULT_BRANCH=main
refuse "push creating the branch, empty DEFAULT_BRANCH" "DEFAULT_BRANCH is empty" \
  "$tmp/full" GITHUB_EVENT_NAME=push BEFORE=0000000000000000000000000000000000000000 DEFAULT_BRANCH=
refuse "push creating the branch, default branch not fetched" "origin/trunk is not in the checkout" \
  "$tmp/full" GITHUB_EVENT_NAME=push BEFORE=0000000000000000000000000000000000000000 DEFAULT_BRANCH=trunk
refuse "pull request, base branch with no history in common" "HEAD has no merge base with origin/unrelated" \
  "$tmp/full" GITHUB_EVENT_NAME=pull_request GITHUB_BASE_REF=unrelated
refuse "push creating the branch, default branch with no history in common" "HEAD has no merge base with origin/unrelated" \
  "$tmp/full" GITHUB_EVENT_NAME=push BEFORE=0000000000000000000000000000000000000000 DEFAULT_BRANCH=unrelated
refuse "unhandled event" "event 'workflow_dispatch' is not handled" \
  "$tmp/full" GITHUB_EVENT_NAME=workflow_dispatch
refuse "no repository" "git status fails" \
  "$tmp/norepo" GITHUB_EVENT_NAME=pull_request GITHUB_BASE_REF=main

log=$( cd "$tmp/full" && env -u GITHUB_OUTPUT GITHUB_EVENT_NAME=pull_request GITHUB_BASE_REF=main bash "$script" 2>&1 ) && rc=0 || rc=$?
if [[ $rc != 0 && $log == *"GITHUB_OUTPUT is not set"* ]]; then ok "no GITHUB_OUTPUT"; else
  bad "no GITHUB_OUTPUT: rc=$rc"; echo "$log"; fi

echo "$pass passed, $failed failed"
[[ $failed == 0 ]]
