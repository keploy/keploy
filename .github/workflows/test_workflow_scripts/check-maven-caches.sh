#!/bin/bash
# check-maven-caches.sh: no workflow or action in this repo turns on
# actions/setup-java's own Maven cache (`cache: maven`). Every Maven cache
# goes through .github/actions/setup-java-maven.
#
# setup-java keys its Maven cache on the OS, the arch and the pom's hash
# only, and never replaces an entry. When GitHub's runner image changed its
# Maven (3.10.0 on ubuntu-24.04, 3.9.15 on ubuntu-26.04), the Java jobs kept
# restoring caches the new Maven could not use: Maven 3.10.0 records where
# each file came from in a form Maven 3.9 does not read, so every file was
# "present, but unavailable" and fetched from Maven Central again, on every
# run, until Central answered 429 Too Many Requests.
# setup-java-maven's key names the Maven version and configuration as well,
# and on an exact hit it runs Maven offline (its scripts say how).
#
# Every YAML file under .github is read, at any depth: workflows, and the
# actions they use, composite or nested in another action's directory. A
# `cache:` of maven, quoted or not, in block or flow style (`with: { cache:
# maven }`), is a use. A YAML comment (from a # at the start of a line or
# after a space) is not.
#
#   bash .github/workflows/test_workflow_scripts/check-maven-caches.sh
#
# check-maven-caches.tests.sh runs it on repositories made for each case.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
rc=0
lines=$(git grep -n -I -E "cache[[:space:]]*:[[:space:]]*['\"]?maven" -- \
  ':(glob).github/**/*.yml' ':(glob).github/**/*.yaml') || rc=$?
case $rc in
  0) ;;
  1) lines='' ;;
  *) echo "::error::git grep failed (exit $rc)"; exit "$rc" ;;
esac

# git grep prefixes each line with path:line:. What is left of the line once
# its comment is cut off must set cache to maven.
uses=$(printf '%s\n' "$lines" | awk '
  NF == 0 { next }
  {
    text = $0
    sub(/^[^:]*:[0-9]+:/, "", text)
    sub(/(^|[[:space:]])#.*/, "", text)
    if (text ~ /(^|[[:space:]{,])cache[[:space:]]*:[[:space:]]*["\047]?maven["\047]?[[:space:]]*([,}]|$)/) print $0
  }')
if [ -n "$uses" ]; then
  echo "::error::these lines turn on actions/setup-java's own Maven cache, whose key stays the same when the runner image changes its Maven, so a restored cache can be one that Maven cannot use and fetches from Maven Central again on every run (see .github/workflows/test_workflow_scripts/check-maven-caches.sh):"
  printf '%s\n' "$uses"
  echo "Use ./.github/actions/setup-java-maven (with java-version and cache-dependency-path) in place of actions/setup-java's cache: maven."
  exit 1
fi
echo "No workflow or action turns on actions/setup-java's own Maven cache."
