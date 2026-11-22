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
# Nor does any step cache ~/.m2 (the whole of it, or its repository) with
# actions/cache, or its restore or save half, outside setup-java-maven's own
# action. Maven builds online on such a cache, so it still asks Maven Central
# about what the cache holds: the metadata of a version range, once the copy
# restored is a day old (the default update policy), on every run, since an
# entry is never replaced. When Central answers that request with a 404, Maven
# deletes the cached copy and the build fails; the performance workflow's
# PetClinic build failed so on 2026-11-19 ("No versions available for
# org.bouncycastle:bcutil-jdk18on:jar:[1.81,1.82) within specified range").
# A step is a YAML list item; it is such a cache when it uses actions/cache@,
# actions/cache/restore@ or actions/cache/save@ and names .m2/repository, or
# .m2 itself, anywhere in it (a path in block or flow style). A path given
# only through an expression (path: ${{ env.M2 }}) is not seen, nor is a
# Windows-style path (~\.m2\repository): no workflow runs Maven on Windows.
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

# Steps that cache ~/.m2 themselves (above), file:line of each step's start.
# setup-java-maven's own action is the one place that may.
rc=0
# shellcheck disable=SC2016 # an awk program
raw=$(git ls-files -z -- ':(glob).github/**/*.yml' ':(glob).github/**/*.yaml' \
  ':(exclude,glob).github/actions/setup-java-maven/**' | xargs -0 -r awk '
  function flush() {
    if (item != "" && item ~ /uses:[[:space:]]*["\047]?actions\/cache(\/restore|\/save)?@/ &&
        (item ~ /\.m2\/repository/ || item ~ /\.m2\/?(["\047[:space:],}]|$)/))
      print file ":" start
    item = ""
  }
  FNR == 1 { flush(); file = FILENAME }
  {
    text = $0
    sub(/\r$/, "", text)
    sub(/(^|[[:space:]])#.*/, "", text)
    if (text ~ /^[[:space:]]*$/) next
    match(text, /^[[:space:]]*/)
    ind = RLENGTH
    if (item != "" && ind <= iind) flush()
    if (item == "" && text ~ /^[[:space:]]*-([[:space:]]|$)/) { item = text; iind = ind; start = FNR; next }
    if (item != "") item = item "\n" text
  }
  END { flush() }') || rc=$?
if [ "$rc" -ne 0 ]; then
  echo "::error::reading the workflows for steps that cache ~/.m2 failed (exit $rc)"
  exit "$rc"
fi
if [ -n "$raw" ]; then
  echo "::error::these steps cache ~/.m2 with actions/cache themselves. Maven builds online on such a cache and asks Maven Central again about what it holds (a version range's metadata, once the copy is a day old, on every run), so a bad answer from Central fails the build (see .github/workflows/test_workflow_scripts/check-maven-caches.sh):"
  printf '%s\n' "$raw"
  echo "Use ./.github/actions/setup-java-maven (with java-version and cache-dependency-path) and its save step: mvn then runs offline on the restored repository."
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
echo "No workflow or action turns on actions/setup-java's own Maven cache or caches ~/.m2 itself, and every use of setup-java-maven has its save step."
