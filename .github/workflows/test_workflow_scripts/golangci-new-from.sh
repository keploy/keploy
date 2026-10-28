#!/bin/bash
# golangci-new-from.sh: picks the golangci-lint flag that limits CI's lint to
# the issues on the lines this event adds or changes, from the checkout's own
# git history, and writes it to $GITHUB_OUTPUT as flag=...
#
#   pull_request  --new-from-merge-base=origin/<base branch>: the PR's merge
#                 commit against the base commit GitHub merged it onto.
#   push          --new-from-rev=<github.event.before>: what the push added.
#                 A push that creates the branch (before is all zeros) is
#                 linted from its merge base with the default branch.
#
# Why not golangci-lint-action's only-new-issues: it asks GitHub's API for
# the diff, and GitHub refuses a PR diff over 20,000 lines (406); on that, or
# on any API error for a push, the action lints the whole repository, which
# holds over a thousand issues no PR introduced. Git needs no API: the
# checkout must have the whole history (actions/checkout fetch-depth: 0).
#
# Anything it cannot resolve (a shallow checkout, a missing base branch or
# before commit, an event it does not know) fails the step, naming what is
# missing: it never falls back to linting everything or nothing.
#
# Inputs (environment): GITHUB_EVENT_NAME, GITHUB_BASE_REF (pull_request),
# BEFORE and DEFAULT_BRANCH (push), GITHUB_OUTPUT.
set -euo pipefail

fail() {
  echo "::error::golangci-new-from: $*" >&2
  exit 1
}

commit_of() {
  git rev-parse --verify --quiet "$1^{commit}" >/dev/null ||
    fail "$2"
}

[[ -n ${GITHUB_OUTPUT:-} ]] || fail "GITHUB_OUTPUT is not set"
# golangci-lint runs these same git commands; where one fails it only warns
# and reports every issue in the repository.
git status --porcelain >/dev/null ||
  fail "git status fails in $PWD"
[[ $(git rev-parse --is-shallow-repository) == false ]] ||
  fail "the checkout is shallow; actions/checkout needs fetch-depth: 0 for git to find what changed"

case "${GITHUB_EVENT_NAME:-}" in
  pull_request)
    [[ -n ${GITHUB_BASE_REF:-} ]] || fail "GITHUB_BASE_REF is empty on a pull_request event"
    base="origin/$GITHUB_BASE_REF"
    commit_of "$base" "$base is not in the checkout"
    git merge-base "$base" HEAD >/dev/null ||
      fail "HEAD has no merge base with $base"
    flag="--new-from-merge-base=$base"
    ;;
  push)
    [[ -n ${BEFORE:-} ]] || fail "BEFORE (github.event.before) is empty on a push event"
    if [[ $BEFORE =~ ^0+$ ]]; then
      [[ -n ${DEFAULT_BRANCH:-} ]] || fail "DEFAULT_BRANCH is empty"
      base="origin/$DEFAULT_BRANCH"
      commit_of "$base" "the push creates the branch and $base is not in the checkout"
      git merge-base "$base" HEAD >/dev/null ||
        fail "the push creates the branch and HEAD has no merge base with $base"
      flag="--new-from-merge-base=$base"
    else
      commit_of "$BEFORE" "the push's before commit $BEFORE is not in the checkout (was the branch force-pushed?)"
      flag="--new-from-rev=$BEFORE"
    fi
    ;;
  *)
    fail "event '${GITHUB_EVENT_NAME:-}' is not handled; add it here before adding it to the workflow's triggers"
    ;;
esac

echo "golangci-lint reports only issues on changed lines: $flag"
echo "flag=$flag" >>"$GITHUB_OUTPUT"
