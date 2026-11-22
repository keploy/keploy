#!/bin/bash
# check-setup-java.sh: no workflow or action in this repo runs
# actions/setup-java itself. Every JDK comes from
# .github/actions/setup-java-maven/jdk (setup-java-maven uses it for Maven
# builds), which installs the build .github/actions/setup-java-maven/jdk/jdks.txt
# pins for the major version asked: the runner image's own copy when it is
# exactly that build, else the build's archive, checked against the sha256
# Adoptium publishes and kept in the Actions cache under that digest. Its own
# steps that run actions/setup-java are the only ones allowed, and only in two
# forms, neither of which downloads anything: distribution jdkfile with a
# jdkFile (the checked archive), or distribution temurin with java-version
# ${{ steps.pin.outputs.version }} (the exact pinned build) under
# if: steps.pin.outputs.source == 'image' (jdk.sh found that build in the
# runner image's tool cache, where setup-java then finds it). Such a step may
# have no other key or input: the step keys name, id, if, uses and with only,
# and the inputs distribution, java-version, jdkFile, mvn-toolchain-id and
# mvn-toolchain-vendor only. Any other (check-latest, architecture,
# java-package, env: and any input setup-java adds later) fails: several make
# setup-java resolve and download a JDK itself.
#
# Asked for a major version, setup-java resolves it with Adoptium's API to
# the latest build, on every run, and when the runner image has no such
# build (ubuntu-26.04 has no JDK 8) it downloads it from GitHub's release
# servers and extracts whatever comes back, unchecked: one bad answer fails
# the job. On 2026-11-20 three async-config-poll jobs failed so ("gzip:
# stdin: not in gzip format", the 103 MB archive "downloaded" in 25 to
# 60 ms), and the build a job ran on changed whenever Adoptium released one.
#
# A step is a YAML list item. It runs setup-java when its uses: key (in
# block or flow style) names actions/setup-java@ (any ref, quoted or not, in
# any letter case). A script line that mentions it is not a use, unless the
# line itself starts with `uses: actions/setup-java@` (no file has one: it
# is then reported as a use). Every
# YAML file under .github is read, at any depth: workflows, and the actions
# they use, composite or nested in another action's directory. A YAML comment
# (from a # at the start of a line or after a space) is not a use. An action
# that runs setup-java inside it from another repository is not seen: none
# of this repository's workflows uses one.
#
#   bash .github/workflows/test_workflow_scripts/check-setup-java.sh
#
# check-setup-java.tests.sh runs it on repositories made for each case.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
own=.github/actions/setup-java-maven/jdk/action.yml

rc=0
# shellcheck disable=SC2016 # an awk program
bad=$(git ls-files -z -- ':(glob).github/**/*.yml' ':(glob).github/**/*.yaml' |
  xargs -0 -r awk -v own="$own" '
  BEGIN {
    split("name id if uses with", a, " "); for (i in a) STEPOK[a[i]] = 1
    split("distribution java-version jdkfile mvn-toolchain-id mvn-toolchain-vendor", a, " "); for (i in a) WITHOK[a[i]] = 1
  }
  function trim(s) { sub(/^[[:space:]]+/, "", s); sub(/[[:space:]]+$/, "", s); return s }
  # flow STR: the keys at the top level of the flow mapping STR ("{...}") in
  # FK, space-separated; the text of its with: value, when that is a flow
  # mapping, in FW. Quoted text and nested ${{ }} or {} are not keys.
  function flow(str,   i, c, d, q, buf, key, expect, ws) {
    FK = ""; FW = ""; d = 0; q = ""; buf = ""; key = ""; expect = 0; ws = 0
    for (i = 1; i <= length(str); i++) {
      c = substr(str, i, 1)
      if (d == 1 && expect) {
        if (c == ":") { key = trim(buf); gsub(/["\047]/, "", key); FK = FK " " key; expect = 0; buf = "" }
        else if (c == "}") d--
        else buf = buf c
        continue
      }
      if (q != "") { if (c == q) q = ""; continue }
      if (c == "\"" || c == "\047") { q = c; continue }
      if (c == "{" || c == "[") {
        d++
        if (d == 1) { expect = 1; buf = "" } else if (d == 2 && key == "with" && c == "{") ws = i
        continue
      }
      if (c == "}" || c == "]") { if (d == 2 && ws) { FW = substr(str, ws, i - ws + 1); ws = 0 } d--; continue }
      if (d == 1 && c == ",") { expect = 1; buf = "" }
    }
  }
  # keys IT: the keys of the step IT in SK, and its with: inputs in WK, space-separated,
  # in block or flow style.
  function keys(it,   n, L, i, k, ind, t, key, inwith, wk, rest, from) {
    SK = ""; WK = ""
    n = split(it, L, "\n")
    match(L[1], /^[[:space:]]*-[[:space:]]*/); k = RLENGTH
    t = substr(L[1], k + 1); from = 1
    if (t ~ /^\{/) { flow(t); SK = FK; if (FW != "") { flow(FW); WK = FK } return }
    if (t == "") { if (n < 2) return; match(L[2], /^[[:space:]]*/); k = RLENGTH; from = 2 }
    else L[1] = sprintf("%" k "s", "") t
    inwith = 0; wk = -1
    for (i = from; i <= n; i++) {
      match(L[i], /^[[:space:]]*/); ind = RLENGTH; t = substr(L[i], ind + 1)
      if (ind == k) {
        key = t; sub(/[[:space:]]*:.*/, "", key); gsub(/["\047]/, "", key)
        SK = SK " " key
        inwith = (key == "with"); wk = -1
        if (inwith) { rest = t; sub(/^[^:]*:[[:space:]]*/, "", rest); if (rest ~ /^\{/) { flow(rest); WK = WK FK; inwith = 0 } }
      } else if (ind > k && inwith) {
        if (wk < 0) wk = ind
        if (ind == wk) { key = t; sub(/[[:space:]]*:.*/, "", key); gsub(/["\047]/, "", key); WK = WK " " key }
      } else if (ind < k) inwith = 0
    }
  }
  function flush(  low, extra, n, a, x) {
    low = tolower(item)
    if (item != "" && (low ~ /(^|\n)[[:space:]]*(-[[:space:]]+)?uses:[[:space:]]*["\047]?actions\/setup-java@/ ||
                       low ~ /[{,][[:space:]]*uses:[[:space:]]*["\047]?actions\/setup-java@/)) {
      if (file != own) print file ":" start
      else {
        keys(low)
        extra = ""
        n = split(SK, a, " "); for (x = 1; x <= n; x++) if (!(a[x] in STEPOK)) extra = extra " step key " a[x] ":"
        n = split(WK, a, " "); for (x = 1; x <= n; x++) if (!(a[x] in WITHOK)) extra = extra " input " a[x] ":"
        archive = low ~ /distribution:[[:space:]]*["\047]?jdkfile["\047]?([[:space:],}]|$)/ &&
                  low ~ /(^|[[:space:]{,])jdkfile:[[:space:]]*[^[:space:],}]/
        image = low ~ /distribution:[[:space:]]*["\047]?temurin["\047]?([[:space:],}]|$)/ &&
                low ~ /java-version:[[:space:]]*["\047]?\$\{\{[[:space:]]*steps\.pin\.outputs\.version[[:space:]]*\}\}["\047]?([[:space:],}]|$)/ &&
                low ~ /(^|\n|[{,])[[:space:]]*(-[[:space:]]+)?if:[[:space:]]*(\$\{\{[[:space:]]*)?steps\.pin\.outputs\.source[[:space:]]*==[[:space:]]*\047image\047[[:space:]]*(\}\}[[:space:]]*)?(\n|[,}]|$)/
        if (extra != "")
          print file ":" start " (setup-java-maven/jdk: not allowed on a setup-java step:" extra " it allows the step keys name, id, if, uses and with, and the inputs distribution, java-version, jdkFile, mvn-toolchain-id and mvn-toolchain-vendor)"
        else if (!archive && !image)
          print file ":" start " (setup-java-maven/jdk: neither distribution jdkfile with a jdkFile, nor distribution temurin of the pinned version under if: steps.pin.outputs.source == \047image\047)"
      }
    }
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
  echo "::error::reading the workflows for steps that run actions/setup-java failed (exit $rc)"
  exit "$rc"
fi
if [ -n "$bad" ]; then
  echo "::error::these steps run actions/setup-java themselves. It resolves the JDK to whatever build is latest on every run and, when the runner image has none, downloads it unchecked, so one bad answer from GitHub's release servers fails the job (see .github/workflows/test_workflow_scripts/check-setup-java.sh):"
  printf '%s\n' "$bad"
  echo "Use ./.github/actions/setup-java-maven (a Maven build) or ./.github/actions/setup-java-maven/jdk (a JDK alone) with java-version: the build its jdks.txt pins, checked against its sha256 and cached."
  exit 1
fi
echo "No workflow or action runs actions/setup-java but setup-java-maven/jdk, on the archive it has checked or the image's copy of the pinned build."
