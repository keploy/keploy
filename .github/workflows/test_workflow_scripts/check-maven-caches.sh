#!/bin/bash
# check-maven-caches.sh: no workflow or action in this repo turns on
# actions/setup-java's own Maven cache (`cache: maven`). Every Maven cache
# goes through .github/actions/setup-java-maven, and every file that uses it
# also uses its save half (.github/actions/setup-java-maven/save) at least as
# many times: without it nothing is saved, and every run starts cold.
#
# setup-java keys its Maven cache on the OS, the arch and the pom's hash
# only, and never replaces an entry. When GitHub's runner image changed its
# Maven (3.10.0 on ubuntu-24.04, 3.9.15 on ubuntu-26.04), the Java jobs kept
# restoring caches the new Maven could not use: Maven 3.10.0 records where
# each file came from in a form Maven 3.9 does not read, so every file was
# "present, but unavailable" and fetched from Maven Central again, on every
# run, until Central answered 429 Too Many Requests.
# setup-java-maven's key names the Maven version and its configuration as
# well; mvn runs offline on a restored cache, and a cache that falls short
# heals in one run (its scripts say how).
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

# Uses of setup-java-maven and of its save half, per file.
use_re="^[[:space:]]*(-[[:space:]]+)?uses:[[:space:]]*['\"]?\./\.github/actions/setup-java-maven"
rc=0
pairs=$(git grep -n -I -E "$use_re" -- ':(glob).github/**/*.yml' ':(glob).github/**/*.yaml') || rc=$?
case $rc in
  0 | 1) ;;
  *) echo "::error::git grep failed (exit $rc)"; exit "$rc" ;;
esac
unsaved=$(printf '%s\n' "$pairs" | awk -F: '
  NF < 3 { next }
  {
    text = $0
    sub(/^[^:]*:[0-9]+:/, "", text)
    sub(/(^|[[:space:]])#.*/, "", text)
    if (text ~ /setup-java-maven\/save(["\047]|[[:space:]]|$)/) saves[$1]++
    else if (text ~ /setup-java-maven(["\047]|[[:space:]]|$)/) uses[$1]++
  }
  END { for (f in uses) if (saves[f] + 0 < uses[f]) printf "%s: %d use(s) of setup-java-maven, %d of setup-java-maven/save\n", f, uses[f], saves[f] + 0 }')
if [ -n "$unsaved" ]; then
  echo "::error::these files use ./.github/actions/setup-java-maven without as many steps of ./.github/actions/setup-java-maven/save after their builds, so their Maven repository is never saved and every run fetches it again (see .github/workflows/test_workflow_scripts/check-maven-caches.sh):"
  printf '%s\n' "$unsaved" | LC_ALL=C sort
  exit 1
fi
echo "No workflow or action turns on actions/setup-java's own Maven cache, and every use of setup-java-maven has its save step."
