#!/usr/bin/env bash
# Tests for reap-stale-containers.sh, the sweep of the self-hosted macOS
# runners' shared Docker daemon: what it removes and what it leaves. Plain bash
# 3.2 with no Docker and no `date`, so it runs unchanged on those runners
# (precheck-macos), on GitHub's macOS and Linux images (macos-ci-scripts.yml)
# and locally:
#
#   bash .github/scripts/macos/reap-stale-containers.tests.sh
#
# The checks of the workflows around the script (the threshold against every
# job's timeout-minutes, how clean_up_docker_macos runs the sweep) are in
# reap-stale-containers.workflows.tests.sh. They run only on GitHub's runners:
# a mistake in an unrelated workflow must not fail precheck-macos and skip the
# macOS test jobs.
#
# The script drives a fake docker CLI (REAP_DOCKER) backed by a state directory,
# so every case pins the daemon clock and each container's timestamps exactly,
# and a fake `sleep` that only notes how long it was asked to wait. Nothing
# here touches the real Docker daemon. These tests run on the shared macOS
# machine itself, so the script refuses to run here with the docker CLI left
# at its default (KEPLOY_MACOS_SCRIPT_TESTS).
set -u
export KEPLOY_MACOS_SCRIPT_TESTS=1

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
reaper="$here/reap-stale-containers.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
fake="$work/fake-docker"
echo "bash $BASH_VERSION on $(uname -s)"

# The fake docker. Understands exactly the calls the script makes; anything
# else is recorded in <state>/unexpected, which fails the case, so a new call
# cannot silently get an empty answer. A container is a line of
# <state>/containers: id|name|created|started|finished|flag.
cat >"$fake" <<'EOF'
#!/usr/bin/env bash
state="$FAKE_DOCKER_STATE"
fail() {
  echo "fake docker: $2" >&2
  exit "$1"
}
unexpected() {
  echo "$*" >>"$state/unexpected"
  fail 2 "unexpected command: $*"
}
drop() {
  grep -v "$1" "$state/containers" >"$state/containers.new"
  mv "$state/containers.new" "$state/containers"
}
# A daemon that is down has no clock file.
if [ ! -f "$state/now" ]; then fail 1 'the fake Docker daemon is down'; fi
case "${1:-}" in
  info)
    if [ "$#" -ne 3 ] || [ "$2" != --format ] || [ "$3" != '{{.SystemTime}}' ]; then unexpected "$@"; fi
    cat "$state/now"
    ;;
  ps)
    if [ "$#" -ne 2 ] || [ "$2" != -aq ]; then unexpected "$@"; fi
    if [ -f "$state/ps-fails" ]; then fail 1 "the fake Docker daemon answers 'info' but not 'ps'"; fi
    # Once the script has waited: the removal another sweep had under way is
    # over, and a daemon that dies mid-sweep no longer answers.
    if [ -f "$state/slept" ]; then
      if [ -f "$state/ps-fails-later" ]; then fail 1 'the fake Docker daemon went down during the sweep'; fi
      drop '|removing$'
    fi
    cut -d'|' -f1 "$state/containers"
    # A vanishing container's owner removes it right after this listing:
    # listed here, gone by the `inspect` that follows.
    drop '|vanishing$'
    ;;
  inspect)
    if [ "$#" -ne 4 ] || [ "$2" != --format ] ||
      [ "$3" != '{{.Name}}|{{.Created}}|{{.State.StartedAt}}|{{.State.FinishedAt}}' ]; then unexpected "$@"; fi
    line="$(grep "^$4|" "$state/containers")" || fail 1 "Error: No such object: $4"
    IFS='|' read -r _ name created started finished _ <<<"$line"
    echo "/$name|$created|$started|$finished"
    ;;
  rm)
    if [ "$#" -ne 3 ] || [ "$2" != -f ]; then unexpected "$@"; fi
    echo "$3" >>"$state/asked"
    # `docker rm -f` of a container that is not there exits 0.
    line="$(grep "^$3|" "$state/containers")" || exit 0
    case "$line" in
      *'|stuck') fail 1 "Error response from daemon: cannot remove container $3" ;;
      *'|removing') fail 1 "Error response from daemon: removal of container $3 is already in progress" ;;
    esac
    drop "^$3|"
    echo "$3" >>"$state/removed"
    echo "$3"
    ;;
  *) unexpected "$@" ;;
esac
EOF
chmod +x "$fake"
# The fake sleep, first on the script's PATH: the script waits before it looks
# again at a removal docker refused, and the cases must not.
mkdir "$work/bin"
cat >"$work/bin/sleep" <<'EOF'
#!/bin/sh
echo "$*" >>"$FAKE_DOCKER_STATE/slept"
EOF
chmod +x "$work/bin/sleep"

# The threshold and the wait the script ships with, so the cases follow them.
min_age="$(sed -n 's/^MIN_AGE_MINUTES=\([0-9][0-9]*\)$/\1/p' "$reaper")"
settle="$(sed -n 's/^SETTLE_SECONDS=\([0-9][0-9]*\)$/\1/p' "$reaper")"
if [ -z "$min_age" ] || [ -z "$settle" ]; then
  echo "FAIL - cannot read MIN_AGE_MINUTES and SETTLE_SECONDS from $reaper"
  exit 1
fi

NEVER='0001-01-01T00:00:00Z'
# The moment of the failing run: run 37682705606's cleanup removed
# echoApp_37682999736-echo-sql-1 at this time.
NOW='2026-10-07T20:56:23.991663000Z'
# That many seconds before $NOW, with nine fractional digits as docker prints
# them. Same day only: 20:56:23 is 75383 s after midnight.
ago() {
  local t=$((75383 - $1))
  if [ "$t" -lt 0 ]; then
    echo "ago: $1 s before NOW is the day before" >&2
    return 1
  fi
  printf '2026-10-07T%02d:%02d:%02d.000000000Z\n' $((t / 3600)) $((t % 3600 / 60)) $((t % 60))
}
minutes_ago() { ago $(($1 * 60)); }

# new_state [daemon clock]: an empty fake daemon. "down" has no clock at all.
states=0
new_state() {
  states=$((states + 1))
  state="$work/state-$states"
  mkdir "$state"
  : >"$state/containers"
  : >"$state/removed"
  : >"$state/asked"
  if [ "${1-$NOW}" != down ]; then printf '%s\n' "${1-$NOW}" >"$state/now"; fi
  export FAKE_DOCKER_STATE="$state"
}
# container <id> <name> <created> [started] [finished] [flag]. Left out, started
# and finished are year 1, as docker gives them for "never". flag: stuck (it
# survives `rm -f`), removing (another sweep's removal of it is under way) or
# vanishing (gone between the listing and its inspect).
container() {
  printf '%s|%s|%s|%s|%s|%s\n' "$1" "$2" "$3" "${4-$NEVER}" "${5-$NEVER}" "${6-}" >>"$state/containers"
}
# Runs the script against the fake, under the bash running these tests; leaves
# its output in $out and its exit code in $code.
run_reaper() {
  out="$(PATH="$work/bin:$PATH" REAP_DOCKER="$fake" "$BASH" "$reaper" 2>&1)"
  code=$?
  if [ -s "$state/unexpected" ]; then
    problem "made a docker call the fake does not know: $(tr '\n' ';' <"$state/unexpected")"
  fi
}

failures=0
problems=''
problem() { problems="$problems       $1"$'\n'; }
report() {
  if [ -z "$problems" ]; then
    echo "ok   - $1"
  else
    failures=$((failures + 1))
    echo "FAIL - $1"
    printf '%s' "$problems"
    printf '%s\n' "$out" | sed 's/^/       | /'
  fi
  problems=''
}
# removed <ids in any order>: exactly these were removed.
removed() {
  local got want
  got="$(sort "$state/removed" | tr '\n' ' ')"
  want="$(printf '%s\n' "$@" | sort | tr '\n' ' ')"
  if [ "$#" -eq 0 ]; then want=''; fi
  if [ "$got" != "$want" ]; then problem "removed [${got% }], want [${want% }]"; fi
}
code() { if [ "$code" -ne "$1" ]; then problem "exit $code, want $1"; fi; }
says() { if ! grep -Eq -- "$1" <<<"$out"; then problem "missing $2"; fi; }
silent_about() { if grep -q -- "$1" <<<"$out"; then problem "said something of $1: $(grep -- "$1" <<<"$out" | tr '\n' ';')"; fi; }

# ---- the age sweep ----------------------------------------------------------

# compose created the application 5.4 s before the sweep and was still waiting
# for the keploy agent to turn healthy, so it had never been started.
new_state
container 9a4d39ac14e5 echoApp_37682999736-echo-sql-1 '2026-10-07T20:56:18.635034000Z'
run_reaper
removed
code 0
says 'leaving echoApp_37682999736-echo-sql-1 - last active 0 minute\(s\) ago' "the line saying why it was left"
report "a sibling's application container, created and not started yet (the run 37682999736 case), is left alone"

new_state
container never 'keploy-v3-4831' "$(minutes_ago $((min_age + 5)))"
container fresh 'echoApp_1-echo-sql-1' "$(ago 1)"
run_reaper
removed never
code 0
report "a container created long ago and never started is removed, the fresh one beside it kept"

new_state
container live 'keploy-v3-live' "$(minutes_ago 3)" "$(minutes_ago 3)"
container old 'keploy-v3-4319' "$(minutes_ago 78)" "$(minutes_ago 78)"
run_reaper
removed old
code 0
says 'removed keploy-v3-4319 \(last active 78 minute\(s\) ago\)' "the line naming what was removed"
silent_about '::warning::'
if [ -e "$state/slept" ]; then problem "waited $(tr '\n' ' ' <"$state/slept")s though docker refused no removal"; fi
report "a leftover is removed while a live agent beside it is kept"

new_state
past="$(minutes_ago $((min_age + 5)))"
container restarted 'mongoDb_1' "$(minutes_ago $((min_age * 4)))" "$(minutes_ago 1)"
container justexited 'ginApp_1' "$past" "$past" "$(minutes_ago 2)"
container longexited 'ginApp_2' "$(minutes_ago $((min_age * 2)))" "$(minutes_ago $((min_age * 2)))" "$past"
container longrunning 'ginApp_3' "$past" "$past"
container justcreated 'flaskApp_1' "$(ago 1)"
run_reaper
removed longexited longrunning
report "age is the most recent of created, started and stopped"

new_state
container inside 'keploy-v3-inside' "$(ago $((min_age * 60 - 1)))" "$(ago $((min_age * 60 - 1)))"
container exact 'keploy-v3-exact' "$(ago $((min_age * 60)))" "$(ago $((min_age * 60)))"
container past 'keploy-v3-past' "$(ago $((min_age * 60 + 1)))" "$(ago $((min_age * 60 + 1)))"
run_reaper
removed exact past
report "a second inside the threshold is left; at it and a second past it are removed"

# A builder like the one these runners keep. It is days old by design.
new_state
container builder 'buildx_buildkit_multiarch0' '2026-09-30T08:00:00.123456789Z' '2026-09-30T08:00:01.123456789Z'
container old 'postgresDb_1' '2026-09-30T08:00:00.123456789Z' '2026-09-30T08:00:01.123456789Z'
run_reaper
removed old
says 'leaving buildx_buildkit_multiarch0 - a buildx builder' "the line saying why the builder was left"
report "a buildx builder is kept whatever its age"

new_state
run_reaper
removed
code 0
says 'Found 0 container\(s\)' "the count of an empty daemon"
says 'Nothing removed\.' "the line saying nothing was removed"
report "a daemon with no containers is swept without a word of complaint"

# Each of these is years old if it is read as a time at all, so one that is
# read is removed: a month 13, a day 0 or 32, an hour 24, a minute or second
# past the end, an offset of a day, a space for the T and no zone.
new_state
container garbled 'keploy-v3-garbled' 'not-a-time' 'also-not' ''
n=0
for t in 2020-13-01T00:00:00Z 2020-00-10T00:00:00Z 2020-01-32T00:00:00Z 2020-01-00T00:00:00Z \
  2020-01-01T24:00:00Z 2020-01-01T00:60:00Z 2020-01-01T00:00:61Z 2020-01-01T00:00:00+24:00 \
  2020-01-01T00:00:00+05:60 '2020-01-01 00:00:00Z' 2020-01-01T00:00:00; do
  n=$((n + 1))
  container "bad$n" "keploy-v3-bad$n" "$t"
done
run_reaper
removed
code 0
says 'leaving keploy-v3-garbled - its age cannot be determined' "the line saying its age is unknown"
if [ "$(grep -c 'its age cannot be determined' <<<"$out")" -ne $((n + 1)) ]; then
  problem "want $((n + 1)) containers left because their age cannot be determined"
fi
says "^::warning::$((n + 1)) container\(s\) left because a timestamp of theirs is in a form this script cannot read" "one warning counting them"
if [ "$(grep -c '^::warning::' <<<"$out")" -ne 1 ]; then problem "want exactly one warning for them all"; fi
report "a container whose age cannot be read is left alone, and the sweep warns once, with their count"

# One timestamp it cannot read might be the latest, so the two it can read do
# not make the container old.
new_state
container started 'keploy-v3-started' "$(minutes_ago 300)" 'garbage' "$NEVER"
container finished 'keploy-v3-finished' "$(minutes_ago 300)" "$(minutes_ago 300)" '2026-10-07T20:00:00'
container created 'keploy-v3-created' 'not-a-time' "$(minutes_ago 300)" "$(minutes_ago 290)"
run_reaper
removed
code 0
if [ "$(grep -c 'its age cannot be determined' <<<"$out")" -ne 3 ]; then
  problem "want all 3 left because one of their timestamps cannot be read"
fi
says '^::warning::3 container\(s\) left because a timestamp of theirs' "the warning counting them"
report "a container with one timestamp it cannot read is left alone, however old the others are"

# Its owner can remove a container between the sweep's listing and its
# inspect. That one is skipped without a word: it is neither removed nor taken
# for a container whose age cannot be read.
new_state
container gone4a1f 'echoApp_gone4a1f' "$(minutes_ago 300)" "$(minutes_ago 300)" "$NEVER" vanishing
container old 'echoApp_0badf00d' "$(minutes_ago 300)" "$(minutes_ago 300)"
run_reaper
removed old
code 0
silent_about gone4a1f
report "a container gone between the sweep's listing and its inspect is skipped"

# ---- the clock ---------------------------------------------------------------

# A daemon on a Linux host gives its clock in local time and the containers'
# timestamps in UTC. The clock here is the failing run's instant again.
new_state '2026-10-08T02:26:23.991663000+05:30'
container fresh 'echoApp_1-echo-sql-1' '2026-10-07T20:56:18.635034000Z'
container offset 'echoApp_2-echo-sql-1' '2026-10-07T13:56:18-07:00'
container old 'keploy-v3-4319' '2026-10-07T19:38:23.5Z' '2026-10-08T01:08:24+05:30'
run_reaper
removed old
says 'removed keploy-v3-4319 \(last active 77 minute\(s\) ago\)' "the age worked out across the two offsets"
report "timestamps are read with Z or an offset, with or without a fraction"

# Month lengths, the leap day (which 2100 does not have) and the turn of the
# year: 30 minutes ago is kept and 50 minutes ago removed, on either side of
# midnight.
if [ "$min_age" -le 30 ] || [ "$min_age" -gt 50 ]; then
  echo "FAIL - the cases across midnight are written for a threshold above 30 and up to 50 minutes, not $min_age"
  failures=$((failures + 1))
fi
for at in '2028-03-01T00:10:00Z 2028-02-29T23:40:00Z 2028-02-29T23:20:00Z' \
  '2027-03-01T00:10:00Z 2027-02-28T23:40:00Z 2027-02-28T23:20:00Z' \
  '2100-03-01T00:10:00Z 2100-02-28T23:40:00Z 2100-02-28T23:20:00Z' \
  '2027-01-01T00:10:00Z 2026-12-31T23:40:00Z 2026-12-31T23:20:00Z' \
  '2026-10-01T00:10:00Z 2026-09-30T23:40:00Z 2026-09-30T23:20:00Z'; do
  # shellcheck disable=SC2086 # three timestamps
  set -- $at
  new_state "$1"
  container kept 'keploy-v3-kept' "$2" "$2"
  container old 'keploy-v3-old' "$3" "$3"
  run_reaper
  removed old
  says 'leaving keploy-v3-kept - last active 30 minute' "an age of 30 minutes for the one kept"
  says 'removed keploy-v3-old \(last active 50 minute' "an age of 50 minutes for the one removed"
  report "ages are right across midnight into $1"
done

new_state garbage
container old 'keploy-v3-old' "$(minutes_ago 360)" "$(minutes_ago 360)"
run_reaper
removed
code 0
says "::warning::could not read the Docker daemon's clock \(got 'garbage'\); sweeping nothing by age" "the unreadable-clock warning"
report "an unreadable daemon clock reaps nothing by age"

# ---- a daemon that does not do as asked ---------------------------------------

new_state down
container old 'keploy-v3-old' "$(minutes_ago 360)" "$(minutes_ago 360)"
run_reaper
removed
code 0
says 'Docker is not reachable; nothing to reap' "the line saying Docker is not reachable"
report "an unreachable daemon reaps nothing"

new_state
container old 'keploy-v3-old' "$(minutes_ago 360)" "$(minutes_ago 360)"
: >"$state/ps-fails"
run_reaper
removed
code 0
says '::warning::could not list the containers' "the warning that the listing failed"
report "a daemon that cannot list its containers reaps nothing"

# gate_macos needs clean_up_docker_macos, so the sweep must not fail the job.
new_state
container wedged 'keploy-v3-wedged' "$(minutes_ago 120)" "$(minutes_ago 120)" "$NEVER" stuck
container old 'keploy-v3-old' "$(minutes_ago 120)" "$(minutes_ago 120)"
run_reaper
removed old
code 0
says "::warning::could not remove keploy-v3-wedged: it is still there $settle s after docker said 'fake docker: Error response from daemon: cannot remove container wedged'" "the warning naming the container that stayed, with docker's reason"
if ! grep -qx wedged "$state/asked"; then problem "never asked docker to remove the one that stayed"; fi
if [ "$(cat "$state/slept" 2>/dev/null)" != "$settle" ]; then problem "did not wait SETTLE_SECONDS ($settle) once before looking again"; fi
says '^Removed 1 container\(s\)\.$' "a count of 1 removed, the one that stayed not among them"
report "a container that survives removal is a warning with docker's reason, and the rest are still removed"

# However many removals docker refuses, the sweep waits once, after all of them.
new_state
container wedged1 'keploy-v3-wedged1' "$(minutes_ago 120)" "$(minutes_ago 120)" "$NEVER" stuck
container taken 'keploy-v3-taken' "$(minutes_ago 120)" "$(minutes_ago 120)" "$NEVER" removing
container wedged2 'keploy-v3-wedged2' "$(minutes_ago 120)" "$(minutes_ago 120)" "$NEVER" stuck
run_reaper
removed
code 0
if [ "$(tr '\n' ' ' <"$state/slept" 2>/dev/null)" != "$settle " ]; then
  problem "waited [$(tr '\n' ' ' <"$state/slept" 2>/dev/null)], want one wait of SETTLE_SECONDS ($settle)"
fi
if [ "$(grep -c '^::warning::could not remove keploy-v3-wedged[12]: it is still there' <<<"$out")" -ne 2 ]; then
  problem "want a warning for each of the two that stayed"
fi
says 'keploy-v3-taken is gone: another removal of it was under way' "the line for the one another sweep removed"
report "three refused removals cost one wait, then each is told apart"

# Two cleanups can sweep at once. The one that comes second to a container is
# refused while the first one's removal is under way, and the container is gone
# moments later: that is not a container that will not die.
new_state
container taken 'keploy-v3-taken' "$(minutes_ago 120)" "$(minutes_ago 120)" "$NEVER" removing
container old 'keploy-v3-old' "$(minutes_ago 120)" "$(minutes_ago 120)"
run_reaper
removed old
code 0
says 'docker would not remove keploy-v3-taken .*is already in progress' "the line with docker's refusal"
says 'keploy-v3-taken is gone: another removal of it was under way' "the line saying the other removal finished"
silent_about '::warning::'
if [ "$(cat "$state/slept" 2>/dev/null)" != "$settle" ]; then problem "did not wait SETTLE_SECONDS ($settle) once before looking again"; fi
report "a container another sweep is removing at that moment is not reported as one that survived"

new_state
container wedged 'keploy-v3-wedged' "$(minutes_ago 120)" "$(minutes_ago 120)" "$NEVER" stuck
: >"$state/ps-fails-later"
run_reaper
removed
code 0
says '::warning::could not list the containers again; cannot tell whether the 1 removal\(s\) docker refused went through' "the warning that it could not look again"
silent_about 'is gone'
report "a daemon that stops answering after a refused removal is a warning, not a container taken for gone"

# ---- on the shared machine -----------------------------------------------------

# With REAP_DOCKER unset or blank the script would drive the machine's own
# docker. A decoy stands in for it on PATH and must never be called.
decoy="$work/decoy"
mkdir "$decoy"
printf '#!/bin/sh\necho "$*" >>"%s/calls"\n' "$decoy" >"$decoy/docker"
chmod +x "$decoy/docker"
new_state
out="$(unset REAP_DOCKER && PATH="$decoy:$PATH" "$BASH" "$reaper" 2>&1)"
code=$?
code 2
says 'run by the tests \(KEPLOY_MACOS_SCRIPT_TESTS\) without REAP_DOCKER' "the refusal"
out="$out"$'\n'"$(REAP_DOCKER='' PATH="$decoy:$PATH" "$BASH" "$reaper" 2>&1)"
if [ "$?" -ne 2 ]; then problem "ran with REAP_DOCKER=''"; fi
if [ -e "$decoy/calls" ]; then problem "called the docker on PATH: $(tr '\n' ';' <"$decoy/calls")"; fi
report "under the tests, the script refuses to run with the docker CLI left at its default"

if [ "$failures" -gt 0 ]; then
  echo "$failures case(s) failed"
  exit 1
fi
echo "all cases passed"
