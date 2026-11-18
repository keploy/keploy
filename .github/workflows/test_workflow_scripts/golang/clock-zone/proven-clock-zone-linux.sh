#!/usr/bin/env bash
# E2E: a learned read of the application's clock, a wall clock of no zone
# cut to a day and to an hour, checked at its cut in the application's own
# zone under the proven switch, natively (keploy reads the zone of the
# process that made the call from its environment, /proc/<pid>/environ),
# and in keploy's docker and compose modes (keploy's CLI reads the zone of
# the app's container through Docker's API; see the docker and compose legs
# below).
#
# The app charges a payments service on GET /order with
# {"amount":100,"day":"2006-01-02","hour":"2006-01-02T15"}: its clock read
# as it calls, written in the zone it started with (TZ), as a date column
# and an hour bucket are. It answers {"order":"ok"} only when the charge
# answered as recorded ({"order":"unpaid"} otherwise), so a test passes only
# where its charge was served from its recording; its response never shows
# what it sent, so only the call can show a change.
#
# Recorded under KEPLOY_PROVEN=1 with the app in Asia/Kolkata (+05:30). An
# OSS keploy has no learn round (only a record with auto-replay learns), so
# the `clocks` block of each charge mock is learned by the learn step's code
# and written onto the mocks by the code that writes what record's full
# round learned (learnclocks/main.go, built from the keploy module as one
# file; its header lists what of the HTTP learn step it does not run). It
# plays record's full round for each charge: the body's day and hour, in
# the app's layouts as typed below from the app's main.go (and checked
# against each recorded call's own reading in its zone), read at 23:59:30
# IST on the mock's day and a FullRoundLead later, learned in the zone
# keploy reads from the app's TZ. So a change to what learn learns of a
# read of the clock, how it merges it or how the block is written changes
# what this lane replays and breaks it, where a block typed here would not.
# It is the block learn writes for a recording made at 23:59:30 IST, not for
# this one (the lead moves this recording's day and hour only within 61.545 s
# of their end). Today: plain reads (min and max 0s), learned with the
# application in +05:30 (app_zone), of a wall clock of no zone; body.day cut
# to a day (iso-date) and read in either UTC or the application's zone
# (zone: either: the call's day is one in both there), body.hour cut to an
# hour (iso-hour) and read in the application's zone alone (zone: app: its
# hour is another in UTC). A recording before 05:30 IST would name the day's
# zone app, and one in the hour after midnight cuts the hour to a day: this
# lane exercises neither. Learning itself, end to end (record, auto-replay
# learns the clocks, replay with the app's TZ varied), is proven in
# enterprise's learning lane, which lands with the keploy release that
# carries this lane; this lane is the replay side's contract. Replayed with the
# payments service down:
#   1. KEPLOY_PROVEN=1, the app in Asia/Kolkata: the set passes. The clock
#      call is the set's first test's, so it also shows the call has a
#      window on the application's clock (it starts at the set's begin)
#      with this build's own hooks alone: without one keploy refuses the
#      value.
#   2. KEPLOY_PROVEN=1, the app in Pacific/Honolulu (-10:00): the set
#      passes. The app's zone, read from its process, wins over the one the
#      learn step named: its hour read in +05:30 is 15.5 hours off.
#   2b. KEPLOY_PROVEN=1, the app's TZ naming a slim zone file of
#      Europe/Berlin, a zone of summer time (SLIM_DIR: zic -b slim's form,
#      zic's default, Go's own zoneinfo.zip's and so the zone data built
#      into keploy: no transition after the year its rule took over; the
#      distributions' tzdata is fat, every change to 2037): the set passes,
#      the app's zone read as Berlin. keploy compares such a file with its
#      own zone data of the name at each change of offset, and its walk of
#      them stopped on a leap year's last day, where Go ends a slim file's
#      zone without advancing (golang/go#81583): it never returned, and the
#      call, its replay with it, never ended.
#   2c. Switch off, keploy's own TZ naming that slim file, the app in
#      Asia/Kolkata: the set passes. keploy reads its own zone as its agent
#      starts, with the switch or without it, and hung there: the replay
#      never started the app.
#   2d. KEPLOY_PROVEN=1, keploy's own zone data Go's zoneinfo.zip
#      (ZONEINFO), the app's TZ naming a zone file of Africa/Casablanca of an
#      older release of IANA's data (OLD_DIR: tzdata 2024a, from keploy's
#      learn testdata), whose rules Go's release changed (UTC+01:00 there
#      from 2026-09-20 on, UTC+00:00 in Go's): the set passes, the app's zone
#      read as Africa/Casablanca in its file's rules. keploy read a file of
#      a zone whose rules are not its own release's as an unknown zone, and
#      refused the app's clock reads.
#   2e. KEPLOY_PROVEN=1, the app's TZ naming Europe/Berlin of a release of
#      IANA's data later than every one keploy knows (LATER_DIR: zic's build
#      of keploy's newest tzdata.zi made release 2099a, Berlin at UTC+03:00
#      from 2027, with that tzdata.zi beside it): the set passes, the app's
#      zone read as Europe/Berlin, as the release's own source says.
#   3. KEPLOY_PROVEN=1, SKEW_HOURS=3: the set fails, its report naming
#      body.hour with the reason (more than one cut off), never body.day (a
#      day is checked at its cut, and three hours are within one).
#   4. KEPLOY_PROVEN=1, SKEW_HOURS=-72: the set fails, naming body.day (three
#      days off in IST are two or more in UTC: more than one cut in both).
#   5. Switch off, SKEW_HOURS=-72: the set passes, as on main: the lenient
#      matcher serves each charge its recording, and lists no call it could
#      not answer.
#   6. No data race in any log.
# A replay that cannot read the application's zone checks the cut in the
# stored one, and a wall clock of zone app cut to an hour or a day in UTC
# too, refusing an hour where the stored zone stands more than an hour from
# UTC (learn's CheckClockRead rows). This lane's native run reads the zone
# of the application's long-lived process; keploy reads it once for each
# program a process runs.
#
# The docker and compose legs (CLOCK_ZONE_LEGS): there keploy's agent runs
# in a container and a PID namespace of its own and reads no app process's
# zone. The keploy CLI reads each app container's zone through Docker's API
# (its TZ, else its /etc/localtime) and hands it to the agent with its list
# of the daemon's containers; the agent checks a call's clock reads in the
# zone of the container whose socket made the call. The app runs in an image
# of its own, from scratch: the app, the zone files of America/Sao_Paulo and
# Pacific/Honolulu, and /etc/localtime a link to one of them (two images),
# its payments service on the Docker host (host.docker.internal, which
# keploy gives its agent). Each leg records under KEPLOY_PROVEN=1 with the
# app in America/Sao_Paulo (-03:00), and learnclocks writes the block
# learned there: at 23:59:30 in Sao Paulo it is the next day in UTC, so the
# day is learned zone: app, as the hour is. Each replays with the app in
# Pacific/Honolulu (-10:00), seven hours behind: its hour is never the
# recorded one, so it is always checked as a read of the clock, and a replay
# that read no zone of the app's container would check it in the zone the
# learn step named (Sao Paulo's), where it is seven hours stale, and fail.
# A date a day stale holds (a day's cut holds yesterday's date,
# models.ClockOffset.Holds); one more than a day stale never does: with
# SKEW_HOURS=-49 the app writes a date two days before the first day of the
# call's window in Honolulu, or three, wherever in the day it runs (for a
# window shorter than an hour), never the recorded date (Honolulu's date is
# never ahead of Sao Paulo's), and the report says keploy checked it in
# Pacific/Honolulu.
#   docker  1. `docker run -e TZ=Pacific/Honolulu` of the image whose
#              /etc/localtime links Sao Paulo: passes (TZ wins over the link).
#           2. The image whose /etc/localtime links Honolulu, no TZ: passes.
#           3. As 2, SKEW_HOURS=-49: fails naming body.day.
#           4. The image whose /etc/localtime links a slim zone file of
#              Europe/Berlin (IMG_DST, from Go's own zoneinfo.zip), no TZ:
#              passes, read in Berlin, a zone of summer time (step 2b's
#              hang, in the CLI's read of the container's zone and the
#              agent's of the call's).
#           5. As 4, SKEW_HOURS=-49: fails naming body.day, checked in
#              Europe/Berlin.
#           6. The image whose /etc/localtime links a zone file of
#              Africa/Casablanca of tzdata 2024a (IMG_OLD, OLD_DIR's), the
#              keploy CLI's own zone data Go's zoneinfo.zip (ZONEINFO), whose
#              release changed the zone's rules (UTC+00:00 from 2026-09-20,
#              2024a's UTC+01:00): passes, the app's container read as
#              Africa/Casablanca in its file's rules. keploy read a file of
#              a zone whose rules are not its own release's as an unknown
#              zone, and checked the app's clock in the learn step's zone.
#           7. The image whose /etc/localtime links Europe/Berlin, a
#              Dockerfile's COPY of Asia/Kolkata's file (Go's own) onto
#              /etc/localtime after it, which writes the file the link names
#              (IMG_COPY): fails naming body.hour, keploy having read the
#              zone as unknown, never as Europe/Berlin in Kolkata's rules
#              (Node and a JVM read Berlin there, the C library, Go and
#              Python Kolkata's offsets).
#           8. The image of release 2099a (IMG_LATER: LATER_DIR's files and
#              tzdata.zi), /etc/localtime linking Europe/Berlin: passes, read
#              as Europe/Berlin, as the image's own tzdata.zi says.
#           9. The image of release 2099b (IMG_SPLIT: SPLIT_DIR's, which
#              parts Europe/Oslo from Berlin at UTC+03:00 from 2027),
#              /etc/localtime linking Europe/Berlin, a Dockerfile's COPY of
#              the image's own Europe/Oslo onto /etc/localtime after it: fails,
#              the zone unknown. Its bytes are 2099a's Berlin's (step 8):
#              only the image's own tzdata.zi tells Oslo's file at Berlin's
#              path from Berlin's own.
#   compose 1. The app's service, of the image linking Honolulu, beside a
#              second service of the image linking Sao Paulo, which makes no
#              call: passes, the app's container read in its own zone.
#           2. As 1, SKEW_HOURS=-49: fails naming body.day.
#   keploy's compose rewrite puts only the --container-name service in the
#   agent's PID namespace, and its CLI reads the zone only of a container in
#   another container's PID namespace (a user's pid: service:<x> too, whose
#   calls never reach the agent), so this lane's second service, in a PID
#   namespace of its own, has no zone read, and this lane cannot show one
#   container's call taking another's zone: two app containers held in two
#   zones are proven at unit level only (containerkey's
#   TestEachAppContainersConnectionsTakeItsOwnZone).
#   No data race in any log.
#
# Readiness is the app's listening port while recording natively, the app's
# answer while recording in docker and compose (its port is the agent's,
# which accepts before the app runs), and keploy's own probe of the app
# (--health-path) while replaying. Each replay ends within REPLAY_LIMIT
# seconds (default 300): a keploy that never ends fails its step.
# The script stops only the processes and removes only the containers and
# images it started. Ports: 8092 (app), 9098
# (payments), as the app has them. Env: RECORD_BIN, REPLAY_BIN,
# CLOCK_ZONE_LEGS (the legs to run, of native, docker and compose; default
# native). The docker and compose legs need Docker, docker compose, and the
# agent image the binary names loaded (.github/actions/download-image). Go's
# own zoneinfo.zip (go env GOROOT) gives the slim zone file of
# Europe/Berlin, which python3 reads.
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
# shellcheck source=../../sudo_env.sh
source "$(dirname "${BASH_SOURCE[0]}")/../../sudo_env.sh" || exit 1

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-$RECORD_BIN}"
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP=8092
DEP=9098
REC_ZONE=Asia/Kolkata
# The keploy module's root, from which learn's own code runs.
ROOT="$(cd "$SRC/../../../../.." && pwd)"
FAIL=0
fail() {
    echo "::error::$*"
    FAIL=$((FAIL + 1))
}
LEGS=" ${CLOCK_ZONE_LEGS:-native} "
for leg in $LEGS; do
    case $leg in native | docker | compose) ;; *) echo "::error::CLOCK_ZONE_LEGS names no leg $leg (native, docker, compose)"; exit 1 ;; esac
done
want() { case $LEGS in *" $1 "*) return 0 ;; esac; return 1; }
# The docker and compose legs' zones, and names of this run's own: two runs
# on one host never share a container or an image.
# The docker and compose legs record with the app in DREC_ZONE and replay
# with it in DLIVE_ZONE.
DREC_ZONE=America/Sao_Paulo
DLIVE_ZONE=Pacific/Honolulu
# A zone of summer time, read from a slim zone file (steps 2b, docker 4
# and 5).
DST_ZONE=Europe/Berlin
REPLAY_LIMIT="${REPLAY_LIMIT:-300}"
ID=$$
IMG_REC="clockzone-app-rec:$ID"
IMG_LIVE="clockzone-app-live:$ID"
IMG_DST="clockzone-app-dst:$ID"
IMG_OLD="clockzone-app-old:$ID"
IMG_COPY="clockzone-app-copy:$ID"
IMG_LATER="clockzone-app-later:$ID"
IMG_SPLIT="clockzone-app-split:$ID"
# The zone a COPY of another zone's file overwrites in IMG_COPY, and that
# zone (Go's own file of it).
COPY_LINK=Europe/Berlin
COPY_ZONE=Asia/Kolkata
RUN_NAME="cz-run-$ID"
CAPP="cz-app-$ID"
COTHER="cz-other-$ID"
DOCKER_DEP_URL="http://host.docker.internal:$DEP"

WORK="$(mktemp -d)"
echo "work dir: $WORK"
COMPOSE_DIR="$WORK/cz-compose-$ID"
(cd "$SRC" && go build -o "$WORK/order-app" ./app && go build -o "$WORK/payments-svc" ./payments) || { echo "::error::build failed"; exit 1; }
(cd "$ROOT" && go build -o "$WORK/learnclocks" ./.github/workflows/test_workflow_scripts/golang/clock-zone/learnclocks/main.go) ||
    { echo "::error::cannot build learn's clocks writer (learnclocks) from the keploy module at $ROOT"; exit 1; }
cd "$WORK" || exit 1

# slim <file>: whether the zone file is in zic's slim form: version 2 or
# later, its 32-bit block listing no transition.
slim() {
    local version timecnt
    version="$(od -An -tu1 -j4 -N1 "$1" | tr -d ' ')"
    timecnt="$(od -An -tu1 -j32 -N4 "$1" | tr -s ' ' | sed 's/^ //')"
    [ "$version" -ge 50 ] 2>/dev/null && [ "$timecnt" = "0 0 0 0" ]
}
# SLIM_DIR holds the slim zone file of DST_ZONE, from Go's own
# zoneinfo.zip (zic -b slim's form).
SLIM_DIR="$WORK/slim/zoneinfo"
# OLD_DIR holds OLD_ZONE's zone file of tzdata 2024a (learn's testdata,
# copied from Go 1.23.12's zoneinfo.zip), and GO_ZONEINFO is Go's own
# zoneinfo.zip, keploy's own zone data in step 2d.
OLD_ZONE=Africa/Casablanca
OLD_DIR="$WORK/old/zoneinfo"
GO_ZONEINFO="$(go env GOROOT)/lib/time/zoneinfo.zip"
if ! mkdir -p "$SLIM_DIR/${DST_ZONE%/*}" ||
    ! python3 -I -c 'import sys, zipfile; sys.stdout.buffer.write(zipfile.ZipFile(sys.argv[1]).read(sys.argv[2]))' \
        "$(go env GOROOT)/lib/time/zoneinfo.zip" "$DST_ZONE" >"$SLIM_DIR/$DST_ZONE"; then
    echo "::error::cannot read $DST_ZONE from Go's zoneinfo.zip"
    exit 1
fi
slim "$SLIM_DIR/$DST_ZONE" || { echo "::error::Go's zone file of $DST_ZONE is not slim: steps 2b, docker 4 and 5 would read no slim file"; exit 1; }
mkdir -p "$OLD_DIR/${OLD_ZONE%/*}" && cp "$ROOT/pkg/matcher/learn/testdata/zoneinfo-2024a/$OLD_ZONE" "$OLD_DIR/$OLD_ZONE" ||
    { echo "::error::cannot copy tzdata 2024a's $OLD_ZONE from keploy's learn testdata"; exit 1; }
# Step 2d shows something only where the two releases read the zone apart
# today: the C library reads each file as the app and keploy do.
python3 -I -c 'import sys, zipfile; sys.stdout.buffer.write(zipfile.ZipFile(sys.argv[1]).read(sys.argv[2]))' "$GO_ZONEINFO" "$OLD_ZONE" >"$WORK/old/go-$$" &&
    [ "$(TZ="$OLD_DIR/$OLD_ZONE" date +%z)" != "$(TZ="$WORK/old/go-$$" date +%z)" ] ||
    { echo "::error::Go's $OLD_ZONE reads today as tzdata 2024a's does ($(TZ="$OLD_DIR/$OLD_ZONE" date +%z)): step 2d would read no other release"; exit 1; }
# LATER_DIR and SPLIT_DIR hold zic's build of keploy's newest tzdata.zi
# (pkg/models/testdata) made a release later than every one keploy knows,
# that tzdata.zi beside it: 2099a keeps Europe/Berlin at UTC+03:00 from
# 2027 (steps 2e, docker 8); 2099b parts Europe/Oslo from Berlin so (its
# lines Berlin's of 2099a), Berlin unchanged (docker 9). 2099a's Berlin and
# 2099b's Oslo are one file.
LATER_ZONE=Europe/Berlin
LATER_DIR="$WORK/later/zoneinfo"
SPLIT_DIR="$WORK/split/zoneinfo"
# later_zi <dir> <old> <new>: keploy's newest tzdata.zi, release 2099a, old
# made new, in dir, and zic's build of it there.
later_zi() {
    mkdir -p "$1" && python3 -I -c '
import sys
s = open(sys.argv[1]).read()
first, rest = s.split("\n", 1)
assert first.startswith("# version ") and rest.count(sys.argv[3]) == 1, "the tzdata.zi holds no such line"
open(sys.argv[2], "w").write("# version " + sys.argv[5] + "\n" + rest.replace(sys.argv[3], sys.argv[4]))
' "$ROOT/pkg/models/testdata/tzdata.zi" "$1/tzdata.zi" "$2" "$3" "$4" && zic -d "$1" "$1/tzdata.zi"
}
command -v zic >/dev/null || { echo "::error::no zic here: steps 2e, docker 8 and 9 build their zone data with it"; exit 1; }
later_zi "$LATER_DIR" $'1 DE CE%sT 1980\n1 E CE%sT\n' $'1 DE CE%sT 1980\n1 E CE%sT 2027\n3 - +03\n' 2099a &&
    later_zi "$SPLIT_DIR" $'L Europe/Berlin Europe/Oslo\n' $'Z Europe/Oslo 0:53:28 - LMT 1893 Ap\n1 c CE%sT 1945 May 24 2\n1 So CE%sT 1946\n1 DE CE%sT 1980\n1 E CE%sT 2027\n3 - +03\n' 2099b &&
    cmp -s "$LATER_DIR/$LATER_ZONE" "$SPLIT_DIR/Europe/Oslo" && ! cmp -s "$SPLIT_DIR/$LATER_ZONE" "$SPLIT_DIR/Europe/Oslo" ||
    { echo "::error::cannot build releases 2099a and 2099b of keploy's newest tzdata.zi, or their Berlin and Oslo are not one file"; exit 1; }

listening() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
# pid_running PID: whether PID still runs, by its state in /proc (keploy runs
# under sudo, so kill -0 from this user fails on a sudo that is alive).
pid_running() {
    local st
    case $1 in "" | *[!0-9]*) return 1 ;; esac
    { read -r st <"/proc/$1/stat"; } 2>/dev/null || return 1
    st=${st##*) }
    [ "${st%% *}" != Z ]
}
# wait_listen <port> <pid>: until something listens on port; fails at once
# when pid (the process that should start it) has exited.
wait_listen() {
    until listening "$1"; do
        pid_running "$2" || return 1
        sleep 0.2
    done
}
test_files() { find keploy/test-set-0/tests -maxdepth 1 -name '*.yaml' -size +0c 2>/dev/null | wc -l; }
# wait_tests <n> <pid>: until n test cases are written; fails at once when
# keploy (pid) has exited.
wait_tests() {
    until [ "$(test_files)" -ge "$1" ]; do
        pid_running "$2" || return 1
        sleep 0.2
    done
}
own() { sudo chown -R "$(id -u):$(id -g)" . 2>/dev/null || true; }
stop_pid() {
    local sudo_pid="$1" child
    child="$(pgrep -P "$sudo_pid" 2>/dev/null | head -n1)"
    [ -n "$child" ] && sudo kill -INT "$child" 2>/dev/null
    wait "$sudo_pid" 2>/dev/null
}
newest_run() { ls -1d keploy/reports/test-run-* 2>/dev/null | sort -t- -k3 -n | tail -n1; }
REPORT() { echo "$(newest_run)/test-set-0-report.yaml"; }
set_status() { grep -m1 '^status:' "$(REPORT)" 2>/dev/null | awk '{print $2}'; }

# kill_tree <pid>: SIGKILL to pid and every process under it.
kill_tree() {
    local c
    for c in $(pgrep -P "$1" 2>/dev/null); do kill_tree "$c"; done
    sudo kill -KILL "$1" 2>/dev/null
}
# bounded <log> <command...>: command (a sudo of keploy), its output in log,
# ended where it has not ended within REPLAY_LIMIT seconds: SIGINT to keploy
# (the sudo's child), then, 30 seconds on, SIGKILL to it and every process
# under it (the app). RC is the command's exit code, 124 where it was ended
# so: a keploy that never ends fails its step, never the job at its timeout.
bounded() {
    local log="$1" pid child n=0
    shift
    "$@" >"$log" 2>&1 </dev/null &
    pid=$!
    while pid_running "$pid"; do
        if [ "$n" -ge "$REPLAY_LIMIT" ]; then
            fail "$log: keploy did not end within ${REPLAY_LIMIT}s"
            child="$(pgrep -P "$pid" 2>/dev/null | head -n1)"
            [ -n "$child" ] && sudo kill -INT "$child" 2>/dev/null
            sleep 30
            [ -n "$child" ] && kill_tree "$child"
            wait "$pid" 2>/dev/null
            RC=124
            return
        fi
        sleep 1
        n=$((n + 1))
    done
    wait "$pid"
    RC=$?
}
# replay <name> <zone> <skew hours> [env...]: keploy test of the recording,
# the app in zone (a TZ: a name, or a zone file's path); RC is its exit
# code.
replay() {
    local name="$1" zone="$2" skew="$3" before
    shift 3
    before="$(newest_run)"
    # The switch is on only where a step asks for it, whatever the job's
    # environment holds.
    bounded "$name.log" sudo env -u KEPLOY_PROVEN PATH="$PATH" "${SUDO_ENV[@]}" "$@" "$REPLAY_BIN" test -c "env TZ=$zone SKEW_HOURS=$skew PAYMENTS_URL=http://127.0.0.1:$DEP ./order-app" \
        --delay 1 --health-path /healthz --generateGithubActions=false --disable-tele
    verify_ran "$(proven_of "$@")" "$name.log" || FAIL=$((FAIL + 1))
    own
    if [ "$(newest_run)" = "$before" ] || [ ! -f "$(REPORT)" ]; then
        fail "$name: keploy test wrote no report (exit $RC)"
        tail -30 "$name.log"
    fi
}
expect_pass() {
    if [ "$RC" -ne 0 ] || [ "$(set_status)" != PASSED ]; then
        fail "$1: the set must pass (exit $RC, status $(set_status))"
        grep -nE 'status:|field:|why:|differs|path:' "$(REPORT)" | head -30
        grep -m3 "a learned read of the application's clock does not hold" "$2.log"
    fi
}
# expect_clock_miss <step> <field>: the set failed on a call that got no
# recorded answer, and the report names field as what differs, with the
# reason a learned clock read gives.
expect_clock_miss() {
    if [ "$RC" -eq 0 ] || [ "$(set_status)" != FAILED ]; then
        fail "$1: the set must fail (exit $RC, status $(set_status))"
    fi
    FIELD="$2" yq -e '[.tests[].failure_info.unmatched_calls[]?.differs[]?] | any_c(. == strenv(FIELD))' "$(REPORT)" >/dev/null ||
        fail "$1: the report's differs do not name $2"
    FIELD="$2" yq -e '[.tests[].failure_info.unmatched_calls[]?.field_reasons[]? | select(.field == strenv(FIELD)) | .why] | any_c(test("was learned as .*the value this call carries is not a read of the application.s clock made in this call.s window"))' "$(REPORT)" >/dev/null ||
        fail "$1: the report gives $2 no reason of a learned clock read that changed"
}
# named <field>: whether the report names field as one a call differs in,
# with a reason or without.
named() { FIELD="$1" yq -e '[.tests[].failure_info.unmatched_calls[]? | (.differs[]?, .field_reasons[]?.field)] | any_c(. == strenv(FIELD))' "$(REPORT)" >/dev/null 2>&1; }

# The app's zones are read from the system's zone files, by the app (Go's
# time package) and by keploy (the process's root): a host without them
# would check no zone at all.
for z in "$REC_ZONE" Pacific/Honolulu "$DREC_ZONE" "$DLIVE_ZONE"; do
    [ -f "/usr/share/zoneinfo/$z" ] || { echo "::error::no zone file for $z: install tzdata"; exit 1; }
done
for p in "$APP" "$DEP"; do
    if listening "$p"; then echo "::error::port $p is in use"; exit 1; fi
done
REC_PID=""
DEP_PID=""
cleanup() {
    [ -n "$REC_PID" ] && stop_pid "$REC_PID"
    [ -n "$DEP_PID" ] && kill "$DEP_PID" 2>/dev/null
    if want docker || want compose; then
        [ -f "$COMPOSE_DIR/docker-compose.yml" ] && (cd "$COMPOSE_DIR" && compose_down)
        sudo docker rm -f "$RUN_NAME" "$CAPP" "$COTHER" >/dev/null 2>&1
        sudo docker rmi -f "$IMG_REC" "$IMG_LIVE" "$IMG_DST" "$IMG_OLD" "$IMG_COPY" "$IMG_LATER" "$IMG_SPLIT" >/dev/null 2>&1
    fi
    return 0
}
trap cleanup EXIT

native_leg() {
    echo "== record (KEPLOY_PROVEN=1, the app in $REC_ZONE) =="
    ./payments-svc >payments.log 2>&1 &
    DEP_PID=$!
    wait_listen "$DEP" "$DEP_PID" || { echo "::error::the payments service did not start"; exit 1; }
    sudo env PATH="$PATH" "${SUDO_ENV[@]}" KEPLOY_PROVEN=1 "$RECORD_BIN" record -c "env TZ=$REC_ZONE SKEW_HOURS=0 PAYMENTS_URL=http://127.0.0.1:$DEP ./order-app" \
        --generateGithubActions=false --disable-tele >record.log 2>&1 </dev/null &
    REC_PID=$!
    wait_listen "$APP" "$REC_PID" || { echo "::error::the app did not start under keploy record"; tail -30 record.log; exit 1; }
    for _ in 1 2; do
        out="$(curl -s "http://127.0.0.1:$APP/order")"
        [ "$out" = '{"order":"ok"}' ] || fail "GET /order answered '$out' while recording"
    done
    wait_tests 2 "$REC_PID" || fail "keploy record wrote $(test_files) test cases, want 2"
    stop_pid "$REC_PID"
    REC_PID=""
    kill "$DEP_PID" 2>/dev/null
    wait "$DEP_PID" 2>/dev/null
    DEP_PID=""
    own
    verify_ran 1 record.log || FAIL=$((FAIL + 1))
    MOCKS=keploy/test-set-0/mocks.yaml
    [ "$(grep -c '^kind: Http$' "$MOCKS" 2>/dev/null)" -ge 2 ] || { echo "::error::the charge calls were not recorded"; exit 1; }

    # OSS learns nothing at record: a clocks block record wrote would be met
    # with what learnclocks writes below, hiding that it learned.
    grep -q '^clocks:' "$MOCKS" && fail "keploy record wrote a clocks block, which OSS never learns: what learnclocks writes below would be met with it"
    # What learn learns of the two places and writes onto the mocks, in the
    # app's layouts (its main.go writes now.Format of these, in its TZ).
    KEPLOY_PROVEN=1 ./learnclocks -path keploy -set test-set-0 -zone "$REC_ZONE" -place body.day=2006-01-02 -place body.hour=2006-01-02T15 ||
        { echo "::error::learnclocks wrote no clocks block (its reason above)"; exit 1; }
    [ "$(grep -c '^clocks:$' "$MOCKS")" -ge 2 ] || { echo "::error::the clocks block is not on the charge mocks"; cat "$MOCKS"; exit 1; }
    grep -m1 -A14 '^clocks:$' "$MOCKS"

    echo "== 1. KEPLOY_PROVEN=1, the app in $REC_ZONE: passes =="
    replay samezone "$REC_ZONE" 0 KEPLOY_PROVEN=1
    expect_pass "step 1" samezone

    echo "== 2. KEPLOY_PROVEN=1, the app in Pacific/Honolulu: passes, its own zone read from its process =="
    replay appzone Pacific/Honolulu 0 KEPLOY_PROVEN=1
    expect_pass "step 2" appzone

    echo "== 2b. KEPLOY_PROVEN=1, the app's TZ naming a slim zone file of $DST_ZONE, a zone of summer time: passes, read in it =="
    replay slimzone "$SLIM_DIR/$DST_ZONE" 0 KEPLOY_PROVEN=1
    expect_pass "step 2b" slimzone

    echo "== 2c. switch off, keploy's own TZ naming a slim zone file of $DST_ZONE, the app in $REC_ZONE: passes =="
    replay ownzone "$REC_ZONE" 0 TZ="$SLIM_DIR/$DST_ZONE"
    expect_pass "step 2c" ownzone

    echo "== 2d. KEPLOY_PROVEN=1, keploy's own zone data Go's, the app's TZ naming tzdata 2024a's $OLD_ZONE, which Go's release changed: passes, read in it =="
    replay oldrelease "$OLD_DIR/$OLD_ZONE" 0 KEPLOY_PROVEN=1 ZONEINFO="$GO_ZONEINFO"
    expect_pass "step 2d" oldrelease

    echo "== 2e. KEPLOY_PROVEN=1, the app's TZ naming $LATER_ZONE of release 2099a, later than keploy knows, its tzdata.zi beside it: passes, read in it =="
    replay laterrelease "$LATER_DIR/$LATER_ZONE" 0 KEPLOY_PROVEN=1
    expect_pass "step 2e" laterrelease

    echo "== 3. KEPLOY_PROVEN=1, the hour three hours off: fails naming body.hour =="
    replay hourskew "$REC_ZONE" 3 KEPLOY_PROVEN=1
    expect_clock_miss "step 3" body.hour
    named body.day && fail "step 3: the report names body.day, a day three hours off is within one cut"

    echo "== 4. KEPLOY_PROVEN=1, the day three days off: fails naming body.day =="
    replay dayskew "$REC_ZONE" -72 KEPLOY_PROVEN=1
    expect_clock_miss "step 4" body.day

    echo "== 5. switch off, the day three days off: passes, as on main =="
    replay dayskewoff "$REC_ZONE" -72
    expect_pass "step 5" dayskewoff
    yq -e '[.tests[].failure_info.unmatched_calls[]?] | length == 0' "$(REPORT)" >/dev/null 2>&1 ||
        fail "step 5: the report lists a call keploy could not answer"

    echo "== 6. no data race =="
    for f in record.log samezone.log appzone.log slimzone.log ownzone.log hourskew.log dayskew.log dayskewoff.log; do
        grep -q 'WARNING: DATA RACE' "$f" 2>/dev/null && fail "data race in $f"
    done
}

# The docker and compose legs. build_images: the app's images, from
# scratch: the app (static), the zone files of the zones below, and
# /etc/localtime a link into them, as a distribution's image has it:
# $IMG_REC's links DREC_ZONE, $IMG_LIVE's DLIVE_ZONE (the host's files of
# both), $IMG_DST's DST_ZONE (SLIM_DIR's slim file of it alone), $IMG_OLD's
# OLD_ZONE (OLD_DIR's file of tzdata 2024a alone), $IMG_COPY's COPY_LINK
# (the host's file), over which a COPY of Go's own file of COPY_ZONE onto
# /etc/localtime writes, as a Dockerfile's COPY writes through a link;
# $IMG_LATER's LATER_ZONE (LATER_DIR's whole release), $IMG_SPLIT's
# LATER_ZONE (SPLIT_DIR's), over which a COPY of its own Europe/Oslo writes.
build_images() {
    local spec img link ctx z cid ok
    (cd "$SRC" && CGO_ENABLED=0 go build -o "$WORK/order-app-static" ./app) || return 1
    for spec in "$IMG_REC=$DREC_ZONE" "$IMG_LIVE=$DLIVE_ZONE" "$IMG_DST=$DST_ZONE" "$IMG_OLD=$OLD_ZONE" "$IMG_COPY=$COPY_LINK" "$IMG_LATER=$LATER_ZONE" "$IMG_SPLIT=$LATER_ZONE"; do
        img=${spec%%=*} link=${spec#*=}
        ctx="$WORK/img-${img%%:*}"
        mkdir -p "$ctx/root/etc" && cp "$WORK/order-app-static" "$ctx/root/order-app" || return 1
        mkdir -p "$ctx/root/usr/share/zoneinfo/${link%/*}" || return 1
        case $img in
        "$IMG_DST") cp "$SLIM_DIR/$link" "$ctx/root/usr/share/zoneinfo/$link" || return 1 ;;
        "$IMG_OLD") cp "$OLD_DIR/$link" "$ctx/root/usr/share/zoneinfo/$link" || return 1 ;;
        "$IMG_COPY") cp -L "/usr/share/zoneinfo/$link" "$ctx/root/usr/share/zoneinfo/$link" || return 1 ;;
        "$IMG_LATER") cp -a "$LATER_DIR/." "$ctx/root/usr/share/zoneinfo/" || return 1 ;;
        "$IMG_SPLIT") cp -a "$SPLIT_DIR/." "$ctx/root/usr/share/zoneinfo/" && cp "$SPLIT_DIR/Europe/Oslo" "$ctx/copied" || return 1 ;;
        *)
            for z in "$DREC_ZONE" "$DLIVE_ZONE"; do
                mkdir -p "$ctx/root/usr/share/zoneinfo/${z%/*}" && cp -L "/usr/share/zoneinfo/$z" "$ctx/root/usr/share/zoneinfo/$z" || return 1
            done
            ;;
        esac
        ln -sfn "/usr/share/zoneinfo/$link" "$ctx/root/etc/localtime" || return 1
        if [ "$img" = "$IMG_COPY" ] || [ "$img" = "$IMG_SPLIT" ]; then
            if [ "$img" = "$IMG_COPY" ]; then
                python3 -I -c 'import sys, zipfile; sys.stdout.buffer.write(zipfile.ZipFile(sys.argv[1]).read(sys.argv[2]))' "$GO_ZONEINFO" "$COPY_ZONE" >"$ctx/copied" || return 1
            fi
            printf 'FROM scratch\nCOPY root/ /\nCOPY copied /etc/localtime\nENTRYPOINT ["/order-app"]\n' >"$ctx/Dockerfile"
        else
            printf 'FROM scratch\nCOPY root/ /\nENTRYPOINT ["/order-app"]\n' >"$ctx/Dockerfile"
        fi
        sudo docker build -q -t "$img" "$ctx" >/dev/null || return 1
        # The link must reach the image as a link: keploy reads the zone it
        # names.
        cid="$(sudo docker create "$img")" || return 1
        ok=0
        sudo docker cp "$cid:/etc/localtime" - | tar -tvf - | grep "^l.* localtime -> /usr/share/zoneinfo/$link\$" >/dev/null && ok=1
        # IMG_COPY's and IMG_SPLIT's COPY wrote through the link: the file
        # it names holds the copied bytes, which steps 7 and 9 read.
        if { [ "$img" = "$IMG_COPY" ] || [ "$img" = "$IMG_SPLIT" ]; } && ! sudo docker cp "$cid:/usr/share/zoneinfo/$link" - | tar -xOf - | cmp -s - "$ctx/copied"; then
            ok=2
        fi
        sudo docker rm "$cid" >/dev/null
        [ "$ok" = 2 ] && { echo "::error::$img's /usr/share/zoneinfo/$link does not hold the file its Dockerfile copied onto /etc/localtime"; return 1; }
        [ "$ok" = 1 ] || { echo "::error::$img's /etc/localtime is no link to /usr/share/zoneinfo/$link"; return 1; }
    done
}
# learn_in <zone>: learnclocks writes the block learned with the app in zone
# onto the recording's charge mocks; body.day and body.hour must be learned
# in the application's zone alone (zone: app).
learn_in() {
    local m=keploy/test-set-0/mocks.yaml place zones
    [ "$(grep -c '^kind: Http$' "$m" 2>/dev/null)" -ge 2 ] || { fail "$LEG: the charge calls were not recorded"; return 1; }
    grep -q '^clocks:' "$m" && fail "$LEG: keploy record wrote a clocks block, which OSS never learns: what learnclocks writes below would be met with it"
    KEPLOY_PROVEN=1 "$WORK/learnclocks" -path keploy -set test-set-0 -zone "$1" -place body.day=2006-01-02 -place body.hour=2006-01-02T15 ||
        { fail "$LEG: learnclocks wrote no clocks block (its reason above)"; return 1; }
    for place in body.day body.hour; do
        # Each clocks block's zone of place, one line for each mock.
        zones="$(PLACE="$place" yq 'select(.clocks != null) | .clocks[strenv(PLACE)].zone' "$m" | grep -vx -- ---)"
        if [ "$(grep -c . <<<"$zones")" -lt 2 ] || grep -qvx app <<<"$zones"; then
            fail "$LEG: learnclocks did not learn $place in the application's zone alone (zone: app) on both charge mocks: $(tr '\n' ' ' <<<"$zones")"
            grep -m1 -A14 '^clocks:$' "$m"
            return 1
        fi
    done
}
# drecord <container> <command>: keploy record (KEPLOY_PROVEN=1), in the
# working directory, of the app that command runs in container, its payments
# service up on the Docker host: two GET /order calls. Then learn_in
# DREC_ZONE, the zone the app is recorded in.
drecord() {
    local ctr="$1" cmd="$2" out _
    PAYMENTS_ADDR="0.0.0.0:$DEP" "$WORK/payments-svc" >payments.log 2>&1 &
    DEP_PID=$!
    wait_listen "$DEP" "$DEP_PID" || { fail "$LEG: the payments service did not start"; return 1; }
    sudo env -u KEPLOY_PROVEN PATH="$PATH" "${SUDO_ENV[@]}" KEPLOY_PROVEN=1 "$RECORD_BIN" record -c "$cmd" --container-name "$ctr" \
        --generateGithubActions=false --disable-tele >record.log 2>&1 </dev/null &
    REC_PID=$!
    # The app's port is the agent's, published before the app runs: a call
    # that reaches no app gets no answer, and is recorded as no test.
    for _ in 1 2; do
        until out="$(curl -s "http://127.0.0.1:$APP/order")" && [ -n "$out" ]; do
            pid_running "$REC_PID" || { fail "$LEG: the app did not start under keploy record"; tail -30 record.log; return 1; }
            sleep 0.5
        done
        [ "$out" = '{"order":"ok"}' ] || fail "$LEG: GET /order answered '$out' while recording"
    done
    wait_tests 2 "$REC_PID" || fail "$LEG: keploy record wrote $(test_files) test cases, want 2"
    stop_pid "$REC_PID"
    REC_PID=""
    kill "$DEP_PID" 2>/dev/null
    wait "$DEP_PID" 2>/dev/null
    DEP_PID=""
    own
    verify_ran 1 record.log || FAIL=$((FAIL + 1))
    learn_in "$DREC_ZONE"
}
# agent_containers: the IDs of keploy agents' containers (keploy-v3-<random>),
# sorted.
agent_containers() { sudo docker ps -aq --filter 'name=^keploy-v3-' 2>/dev/null | sort; }
# dreplay <name> <container> <command>: keploy test (KEPLOY_PROVEN=1) of the
# recording in the working directory, the app run by command in container,
# its payments service down; RC is its exit code. CLI_ZONEINFO, where set,
# is the keploy CLI's own zone data (ZONEINFO).
dreplay() {
    local name="$1" ctr="$2" cmd="$3" before agents
    before="$(newest_run)"
    agents="$(agent_containers)"
    bounded "$name.log" sudo env -u KEPLOY_PROVEN -u ZONEINFO PATH="$PATH" "${SUDO_ENV[@]}" KEPLOY_PROVEN=1 ${CLI_ZONEINFO:+ZONEINFO="$CLI_ZONEINFO"} "$REPLAY_BIN" test -c "$cmd" --container-name "$ctr" \
        --delay 1 --health-path /healthz --generateGithubActions=false --disable-tele
    # A replay ended by bounded may leave its app's container, and the
    # agent's container its keploy started (keploy-v3-<random>), which holds
    # the app's port: removed, never one that was there before it.
    if [ "$RC" = 124 ]; then
        sudo docker rm -f "$RUN_NAME" "$CAPP" >/dev/null 2>&1
        comm -13 <(printf '%s\n' "$agents") <(agent_containers) | xargs -r sudo docker rm -f >/dev/null 2>&1
    fi
    verify_ran 1 "$name.log" || FAIL=$((FAIL + 1))
    own
    if [ "$(newest_run)" = "$before" ] || [ ! -f "$(REPORT)" ]; then
        fail "$name: keploy test wrote no report (exit $RC)"
        tail -30 "$name.log"
    fi
}
no_race() {
    local f
    for f in ./*.log; do
        grep -q 'WARNING: DATA RACE' "$f" 2>/dev/null && fail "data race in $LEG/${f#./}"
    done
    return 0
}
# checked_in <step> <field> <zone>: the report's reason for field says keploy
# checked its value in zone (the app container's, which keploy's CLI read).
checked_in() {
    FIELD="$2" ZONE="$3" yq -e '[.tests[].failure_info.unmatched_calls[]?.field_reasons[]? | select(.field == strenv(FIELD)) | .why] | any_c(contains("keploy checked the value in " + strenv(ZONE)))' "$(REPORT)" >/dev/null 2>&1 ||
        fail "$1: the report's reason for $2 does not say keploy checked it in $3, the app container's zone"
}
# unread_in <step> <field>: the set failed, and the report's reason for
# field says keploy could not read the app container's zone.
unread_in() {
    if [ "$RC" -eq 0 ] || [ "$(set_status)" != FAILED ]; then
        fail "$1: the set must fail (exit $RC, status $(set_status))"
    fi
    FIELD="$2" yq -e '[.tests[].failure_info.unmatched_calls[]?.field_reasons[]? | select(.field == strenv(FIELD)) | .why] | any_c(contains("keploy could not read the zone of the process that made this call"))' "$(REPORT)" >/dev/null 2>&1 ||
        fail "$1: the report's reason for $2 does not say keploy could not read the app container's zone"
}
# run_cmd <image> <skew> [docker run options]: the app's `docker run`.
run_cmd() {
    printf 'docker run --rm --name %s -p %s:%s -e SKEW_HOURS=%s -e PAYMENTS_URL=%s %s%s' \
        "$RUN_NAME" "$APP" "$APP" "$2" "$DOCKER_DEP_URL" "${3:+$3 }" "$1"
}
docker_leg() {
    LEG=docker
    mkdir -p "$WORK/docker" && cd "$WORK/docker" || return 1
    echo "== docker: record (KEPLOY_PROVEN=1, the app in $DREC_ZONE, its image's /etc/localtime) =="
    drecord "$RUN_NAME" "$(run_cmd "$IMG_REC" 0)" || return 1

    echo "== docker 1. KEPLOY_PROVEN=1, TZ=$DLIVE_ZONE over the image's link to $DREC_ZONE: passes, read in TZ's zone =="
    dreplay docker-tz "$RUN_NAME" "$(run_cmd "$IMG_REC" 0 "-e TZ=$DLIVE_ZONE")"
    expect_pass "docker 1" docker-tz

    echo "== docker 2. KEPLOY_PROVEN=1, the image linking $DLIVE_ZONE, no TZ: passes, read in its link's zone =="
    dreplay docker-link "$RUN_NAME" "$(run_cmd "$IMG_LIVE" 0)"
    expect_pass "docker 2" docker-link

    echo "== docker 3. KEPLOY_PROVEN=1, as 2, the day two days stale: fails naming body.day =="
    dreplay docker-stale "$RUN_NAME" "$(run_cmd "$IMG_LIVE" -49)"
    expect_clock_miss "docker 3" body.day
    checked_in "docker 3" body.day "$DLIVE_ZONE"

    echo "== docker 4. KEPLOY_PROVEN=1, the image linking a slim zone file of $DST_ZONE, a zone of summer time, no TZ: passes, read in it =="
    dreplay docker-dst "$RUN_NAME" "$(run_cmd "$IMG_DST" 0)"
    expect_pass "docker 4" docker-dst

    echo "== docker 5. KEPLOY_PROVEN=1, as 4, the day two days stale: fails naming body.day, checked in $DST_ZONE =="
    dreplay docker-dst-stale "$RUN_NAME" "$(run_cmd "$IMG_DST" -49)"
    expect_clock_miss "docker 5" body.day
    checked_in "docker 5" body.day "$DST_ZONE"

    echo "== docker 6. KEPLOY_PROVEN=1, the image linking tzdata 2024a's $OLD_ZONE, the CLI's own zone data Go's, which changed it: passes, read in it =="
    CLI_ZONEINFO="$GO_ZONEINFO" dreplay docker-old "$RUN_NAME" "$(run_cmd "$IMG_OLD" 0)"
    expect_pass "docker 6" docker-old

    echo "== docker 7. KEPLOY_PROVEN=1, the image's link to $COPY_LINK, a COPY of $COPY_ZONE's file onto /etc/localtime written through it: fails, the zone unknown =="
    dreplay docker-copy "$RUN_NAME" "$(run_cmd "$IMG_COPY" 0)"
    unread_in "docker 7" body.hour

    echo "== docker 8. KEPLOY_PROVEN=1, the image of release 2099a, later than keploy knows, linking $LATER_ZONE, its tzdata.zi saying so: passes, read in it =="
    dreplay docker-later "$RUN_NAME" "$(run_cmd "$IMG_LATER" 0)"
    expect_pass "docker 8" docker-later

    echo "== docker 9. KEPLOY_PROVEN=1, the image of release 2099b, which parts Europe/Oslo from $LATER_ZONE, a COPY of its Oslo onto /etc/localtime written through its link to $LATER_ZONE: fails, the zone unknown =="
    dreplay docker-split "$RUN_NAME" "$(run_cmd "$IMG_SPLIT" 0)"
    unread_in "docker 9" body.hour

    no_race
}
# compose_file <app image> <skew>: the project, in the working directory:
# the app's service, recorded by its container name, and a second service of
# the image linking DREC_ZONE, which runs beside it, makes no call and is not
# in the agent's PID namespace (keploy's rewrite gives that to the app's
# service alone).
compose_file() {
    cat >docker-compose.yml <<YML
services:
  app:
    image: $1
    container_name: $CAPP
    ports: ["$APP:$APP"]
    environment:
      - SKEW_HOURS=$2
      - PAYMENTS_URL=$DOCKER_DEP_URL
  other:
    image: $IMG_REC
    container_name: $COTHER
YML
}
compose_down() { sudo docker compose down --remove-orphans >/dev/null 2>&1; }
compose_leg() {
    LEG=compose
    mkdir -p "$COMPOSE_DIR" && cd "$COMPOSE_DIR" || return 1
    echo "== compose: record (KEPLOY_PROVEN=1, the app in $DREC_ZONE, its image's /etc/localtime) =="
    compose_file "$IMG_REC" 0
    drecord "$CAPP" "docker compose up" || { compose_down; return 1; }
    compose_down

    echo "== compose 1. KEPLOY_PROVEN=1, the app's image linking $DLIVE_ZONE beside a service in $DREC_ZONE: passes, read in the app's zone =="
    compose_file "$IMG_LIVE" 0
    dreplay compose-zones "$CAPP" "docker compose up"
    expect_pass "compose 1" compose-zones
    compose_down

    echo "== compose 2. KEPLOY_PROVEN=1, as 1, the day two days stale: fails naming body.day =="
    compose_file "$IMG_LIVE" -49
    dreplay compose-stale "$CAPP" "docker compose up"
    expect_clock_miss "compose 2" body.day
    checked_in "compose 2" body.day "$DLIVE_ZONE"
    compose_down

    no_race
}

if want docker || want compose; then
    sudo docker info >/dev/null 2>&1 || { echo "::error::the docker and compose legs need Docker"; exit 1; }
    build_images || { echo "::error::the app's images did not build"; exit 1; }
fi
if want native; then
    native_leg
fi
if want docker; then
    docker_leg || fail "the docker leg stopped before its end (its reason above)"
    cd "$WORK" || exit 1
fi
if want compose; then
    compose_leg || fail "the compose leg stopped before its end (its reason above)"
    cd "$WORK" || exit 1
fi

if [ "$FAIL" -ne 0 ]; then
    echo "proven clock-zone e2e: $FAIL failure(s); work dir $WORK"
    exit 1
fi
if want native; then
    echo "PASS (native): under KEPLOY_PROVEN=1 a learned read of the application's clock of no zone, cut to a day and to an hour, holds in the set's first test, is checked in the zone the app's own process runs in, and a value more than one cut off fails naming the field; without the switch nothing changes"
fi
if want docker || want compose; then
    legs=""
    want docker && legs=docker
    want compose && legs="${legs:+$legs and }compose"
    echo "PASS ($legs): under KEPLOY_PROVEN=1 a learned read of the application's clock of no zone, cut to a day and to an hour, is checked in the zone keploy's CLI read of the app's own container (its TZ over its /etc/localtime link; in compose, the --container-name service's), not the zone the learn step named, and a date two days stale fails naming the field"
fi
