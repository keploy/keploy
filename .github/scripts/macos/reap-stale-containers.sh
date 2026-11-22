#!/usr/bin/env bash
# Remove containers left behind on the self-hosted macOS runners' Docker VM.
#
# ONLY CONTAINERS NO LIVE JOB CAN OWN
# The macOS runners are separate runner processes on ONE machine talking to ONE
# Docker daemon, and this runs (from clean_up_docker_macos, at the end of every
# run) while the other runners are mid-job. So any container on the daemon is
# as likely to be a sibling job's live container as a leftover.
#
# The sweep used to remove any container whose StartedAt was more than 30
# minutes before the host's clock. A created-but-not-started container reports
# StartedAt 0001-01-01T00:00:00Z, which that loop read as a start time long
# ago, so it removed every sibling container waiting in the Created state - and
# an application container sits there on every `compose up` while compose
# waits for its keploy agent to turn healthy (5.6 s in job 113004345693: created
# at 20:56:18.6, started at 20:56:24.3). Run 37682999736 lost its echo-sql
# record that way: run 37682705606's cleanup, on another runner of the machine,
# removed echoApp_37682999736-echo-sql-1 5.4 s after compose created it, and
# compose then failed to start it with "No such container".
#
# The rule is the one .github/scripts/windows/reap-keploy-containers.ps1 uses
# for the Windows runners. A job cannot outlive its timeout-minutes and creates
# its containers after it starts, so while it is alive none of its containers
# is older than that. A container whose most recent lifecycle event (created,
# started or stopped) is at least MIN_AGE_MINUTES old belongs to no live job; a
# younger one might, and is left alone. A never-started container reports
# StartedAt and FinishedAt as year 1, so it is aged from its creation - it is
# NOT ancient. A container with a timestamp this cannot read is left alone,
# whatever the other two say. Age is measured on the Docker daemon's own clock (`docker info`
# SystemTime) against the timestamps that same daemon recorded, because on
# macOS the daemon runs in a VM whose clock can drift from the host's.
#
# buildx builders (buildx_buildkit_*) are kept whatever their age, as the old
# loop kept them: one is meant to run for days, and removing it kills whatever
# build is running in it.
#
# Always exits 0, so that the sweep never fails clean_up_docker_macos: gate_macos
# needs that job, and a run must not turn red for a leftover the sweep could not
# remove; the next run's cleanup retries it. The one exception is exit 2, when
# the tests run it without a fake docker (see KEPLOY_MACOS_SCRIPT_TESTS below).
#
# Written for bash 3.2, which is the /bin/bash these runners run steps with.
set -u

# The sweep leaves any container whose last lifecycle event is younger than
# this, because it may belong to a job still running on another runner of this
# machine. Must exceed, by a few minutes, the timeout-minutes of every job that
# uses Docker on these runners (at most 30 today): a job's if: always() steps
# still run after it is cancelled at its timeout. reap-stale-containers.tests.sh
# enforces it for this repository's workflows. The runners belong to the org,
# so other repositories' jobs land on them too; none of those uses Docker
# today, and one that starts to has to stay under this as well.
MIN_AGE_MINUTES=45

# How long a removal that docker refused is given to finish before its
# container counts as having survived; the end of the sweep says why.
SETTLE_SECONDS=20

# The docker CLI to drive. Only the tests set REAP_DOCKER.
docker="${REAP_DOCKER:-docker}"

# reap-stale-containers.tests.sh runs on the shared machine too
# (precheck-macos), and sets KEPLOY_MACOS_SCRIPT_TESTS. A case there that left
# the docker CLI at its default would sweep the daemon every runner uses, so
# under the tests this stops before doing anything unless one was given.
if [ -n "${KEPLOY_MACOS_SCRIPT_TESTS:-}" ] && [ -z "${REAP_DOCKER:-}" ]; then
  echo "reap-stale-containers.sh run by the tests (KEPLOY_MACOS_SCRIPT_TESTS) without REAP_DOCKER: the default acts on the Docker daemon every runner of the shared machine uses." >&2
  exit 2
fi

# Prints the seconds since 1970-01-01T00:00:00Z for an RFC 3339 timestamp as
# docker prints it: 2026-10-07T20:56:18.635041Z, with or without the fraction,
# with Z or an offset (a daemon on a Linux host gives its SystemTime in the
# host's zone, ...+05:30). Prints nothing for anything else, rather than guess
# an age.
#
# Shell arithmetic, not `date`: BSD date (these runners) and GNU date (Linux,
# where the tests also run) take different flags to parse a timestamp, so the
# old loop's `date -j -f` could be run nowhere but on the runners. Year 1 comes
# out as a large negative number, which the caller reads as "never".
rfc3339='^([0-9]{4})-([0-9]{2})-([0-9]{2})T([0-9]{2}):([0-9]{2}):([0-9]{2})(\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})$'
to_epoch() {
  [[ "$1" =~ $rfc3339 ]] || return 0
  # 10#: with a leading zero, 08 and 09 would be read as invalid octal.
  local y=$((10#${BASH_REMATCH[1]})) mo=$((10#${BASH_REMATCH[2]})) d=$((10#${BASH_REMATCH[3]}))
  local h=$((10#${BASH_REMATCH[4]})) mi=$((10#${BASH_REMATCH[5]})) s=$((10#${BASH_REMATCH[6]}))
  local zone="${BASH_REMATCH[8]}" offset=0
  if [ "$mo" -lt 1 ] || [ "$mo" -gt 12 ] || [ "$d" -lt 1 ] || [ "$d" -gt 31 ] ||
    [ "$h" -gt 23 ] || [ "$mi" -gt 59 ] || [ "$s" -gt 60 ]; then
    return 0
  fi
  if [ "$zone" != Z ]; then
    local zh=$((10#${zone:1:2})) zm=$((10#${zone:4:2}))
    if [ "$zh" -gt 23 ] || [ "$zm" -gt 59 ]; then return 0; fi
    offset=$(((zh * 60 + zm) * 60))
    if [ "${zone:0:1}" = - ]; then offset=$((-offset)); fi
  fi
  # Days since 1970-01-01 in the Gregorian calendar. Years are counted from
  # 1 March, which puts the leap day last, and in 400-year eras of 146097 days.
  if [ "$mo" -le 2 ]; then y=$((y - 1)); fi
  local era=$(((y >= 0 ? y : y - 399) / 400))
  local yoe=$((y - era * 400))
  local doy=$(((153 * (mo > 2 ? mo - 3 : mo + 9) + 2) / 5 + d - 1))
  local days=$((era * 146097 + yoe * 365 + yoe / 4 - yoe / 100 + doy - 719468))
  echo $((days * 86400 + h * 3600 + mi * 60 + s - offset))
}

if ! now_raw="$("$docker" info --format '{{.SystemTime}}' 2>/dev/null)"; then
  echo "Docker is not reachable; nothing to reap."
  exit 0
fi
now="$(to_epoch "$now_raw")"
if [ -z "$now" ] || [ "$now" -le 0 ]; then
  # Without the daemon's clock no container's age is known, and removing one
  # whose age is unknown is exactly how a sibling's live container gets killed.
  echo "::warning::could not read the Docker daemon's clock (got '$now_raw'); sweeping nothing by age."
  exit 0
fi

if ! ids="$("$docker" ps -aq 2>/dev/null)"; then
  echo "::warning::could not list the containers; sweeping nothing."
  exit 0
fi
# shellcheck disable=SC2086 # one id per word
set -- $ids
echo "Found $# container(s); removing those idle for at least $MIN_AGE_MINUTES minutes."

removed=0
refused=0
unknown=0
nl=$'\n'
for id in "$@"; do
  meta="$("$docker" inspect --format '{{.Name}}|{{.Created}}|{{.State.StartedAt}}|{{.State.FinishedAt}}' "$id" 2>/dev/null)"
  # Gone between `ps` and `inspect`: its owner removed it.
  if [ -z "$meta" ]; then continue; fi
  IFS='|' read -r name created started finished <<<"$meta"
  name="${name#/}"
  if [ -z "$name" ]; then name="$id"; fi
  # The most recent of created / started / stopped. Year-1 StartedAt and
  # FinishedAt (never started, never stopped) are not positive, so they lose
  # to Created. One that cannot be read at all might be the latest, so then
  # the age is not known.
  last=0
  for raw in "$created" "$started" "$finished"; do
    t="$(to_epoch "$raw")"
    if [ -z "$t" ]; then
      last=0
      break
    fi
    if [ "$t" -gt "$last" ]; then last="$t"; fi
  done
  if [ "$last" -le 0 ]; then
    echo "  leaving $name - its age cannot be determined (inspect said '$meta')."
    unknown=$((unknown + 1))
    continue
  fi
  idle=$((now - last))
  if [ "$idle" -lt $((MIN_AGE_MINUTES * 60)) ]; then
    echo "  leaving $name - last active $((idle / 60)) minute(s) ago, so it may be a live job's on another runner of this machine."
    continue
  fi
  case "$name" in
    buildx*)
      echo "  leaving $name - a buildx builder, kept whatever its age."
      continue
      ;;
  esac
  # `docker rm -f` exits 0 for a container that is already gone, so one that a
  # sweep on another runner removed a moment ago counts as removed here too.
  # While that other removal is still under way docker refuses this one
  # ("removal of container ... is already in progress"), just as it refuses
  # for a container that will not die. Which of the two it was is settled
  # after the sweep.
  if err="$("$docker" rm -f "$id" 2>&1 >/dev/null)"; then
    echo "  removed $name (last active $((idle / 60)) minute(s) ago)"
    removed=$((removed + 1))
  else
    refused_ids[refused]="$id"
    refused_names[refused]="$name"
    refused_why[refused]="$(printf '%s' "$err" | tr '\n' ' ')"
    echo "  docker would not remove $name (last active $((idle / 60)) minute(s) ago): ${refused_why[refused]}"
    refused=$((refused + 1))
  fi
done

# Which of the two a refused removal was is found by looking again, not from
# docker's exit code (reap-keploy-containers.ps1 does the same): a removal
# another sweep had under way is over within seconds and the container is
# gone, while one that will not die is still listed. One removal that takes
# longer than SETTLE_SECONDS is reported as a container that stayed: a
# ::warning:: on a run that otherwise passes, and gone by the next sweep.
if [ "$refused" -gt 0 ]; then
  sleep "$SETTLE_SECONDS"
  if ! listed="$("$docker" ps -aq 2>/dev/null)"; then
    echo "::warning::could not list the containers again; cannot tell whether the $refused removal(s) docker refused went through."
    refused=0
  fi
  i=0
  while [ "$i" -lt "$refused" ]; do
    case "$nl$listed$nl" in
      *"$nl${refused_ids[i]}$nl"*)
        echo "::warning::could not remove ${refused_names[i]}: it is still there $SETTLE_SECONDS s after docker said '${refused_why[i]}'. The next run's cleanup retries it."
        ;;
      *) echo "  ${refused_names[i]} is gone: another removal of it was under way." ;;
    esac
    i=$((i + 1))
  done
fi

# A timestamp this cannot read most likely means docker now prints it in
# another form, and then every leftover stays, run after run, on runs that
# pass. So it is said where it is seen.
if [ "$unknown" -gt 0 ]; then
  echo "::warning::$unknown container(s) left because a timestamp of theirs is in a form this script cannot read (the 'leaving' lines above show it). If docker's format changed, nothing is being swept: teach to_epoch the new form."
fi

if [ "$removed" -eq 0 ]; then
  echo "Nothing removed."
else
  echo "Removed $removed container(s)."
fi
exit 0
