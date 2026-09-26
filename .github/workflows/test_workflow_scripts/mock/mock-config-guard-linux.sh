#!/usr/bin/env bash
# E2E: a keploy.yml, an override or a report that is not a regular file -- a
# FIFO, or a symlink to /dev/zero (git stores the symlink; it cannot store a
# FIFO) -- is refused FAST, with a message that names what is wrong, instead of
# blocking keploy or running it out of memory; a report as large as keploy
# writes, as in large-report, is read as it always was.
#
# Before these reads were bounded, every keploy command in such a repo blocked
# for good (a FIFO keploy.yml: the open itself blocks), or ran out of memory (a
# keploy.yml linked to /dev/zero). The VS Code extension runs
# keploy in whatever folder is open. `keploy report` is the witness: it reads
# keploy.yml (PreProcessFlags, shared by every command) and then the reports,
# and needs no agent, privileges or network.
#
# Every case runs the binary in a throwaway container with no network, a
# memory limit and a time limit, as the host user, so a regression shows here
# as a case that did not return by itself -- killed at the time or memory
# limit -- and never as a hang or an OOM that takes the runner down:
#   fifo-config      keploy.yml is a FIFO
#   devzero-config   keploy.yml is a link to /dev/zero
#   fifo-override    <dir>.keploy.yml, merged over keploy.yml, is a FIFO
#   fifo-report      a run's report is a FIFO
#   devzero-report   a run's report is a link to /dev/zero
#   fifo-reports-dir keploy/reports, which keploy lists, is a FIFO
#   large-report     a 75 MB report, shaped like one keploy writes, is read
#   control          an ordinary keploy.yml is read, and the command succeeds
#
# Against v3.6.71, which read each of these to its end, every case before
# large-report is killed at the time or memory limit.
#
# Env: RECORD_BIN (the keploy binary under test). E2E_IMAGE overrides the
# image, which defaults to the runner's own distribution so the binary finds
# the libc it was built against.
set -uo pipefail

BIN="${RECORD_BIN:-keploy}"
BIN="$(command -v "$BIN" || echo "$BIN")"
# shellcheck disable=SC1091
IMAGE="${E2E_IMAGE:-$(. /etc/os-release && echo "${ID}:${VERSION_ID}")}"
WORK="$(mktemp -d)"
FAIL=0
trap 'rm -rf "$WORK"' EXIT

# A case that returns by itself returns in about a second; the limit leaves
# room for a loaded runner and the container's start.
FAST_SECONDS=10
KILL_SECONDS=30

fail() {
  echo "FAIL: $*"
  FAIL=1
}

if [ -z "${E2E_IMAGE:-}" ]; then
  docker pull -q "$IMAGE" >/dev/null || { echo "FAIL: could not pull $IMAGE"; exit 1; }
fi
cp "$BIN" "$WORK/keploy" || { echo "FAIL: no keploy binary at $BIN"; exit 1; }

# mkcase <case>: an empty repository for the case, with a throwaway HOME.
mkcase() {
  mkdir -p "$WORK/$1/home"
}

# run <case> [seconds] [memory]: `keploy report` in the case's repository,
# killed at the time limit (KILL_SECONDS unless given) or the memory limit
# (1 GiB unless given). Leaves its exit code, its whole seconds and its log.
run() {
  local dir="$WORK/$1" start mem="${3:-1g}"
  start=$(date +%s)
  docker run --rm --network none --memory "$mem" --memory-swap "$mem" \
    --user "$(id -u):$(id -g)" -e HOME="/work/$1/home" \
    -v "$WORK:/work" -w "/work/$1" "$IMAGE" \
    timeout -s KILL "${2:-$KILL_SECONDS}" /work/keploy report --disable-tele >"$dir/out.log" 2>&1
  echo $? >"$dir/exit"
  echo $(($(date +%s) - start)) >"$dir/seconds"
  echo "$1: exit $(cat "$dir/exit") after $(cat "$dir/seconds")s"
}

# returned <case>: the case returned by itself, and fast.
returned() {
  local dir="$WORK/$1" code secs
  code="$(cat "$dir/exit")"
  secs="$(cat "$dir/seconds")"
  case "$code" in
    137) fail "$1: keploy did not return by itself (killed at the ${KILL_SECONDS}s or 1 GiB limit): the read is not bounded"; return 1 ;;
    125|126|127) fail "$1: the container did not run keploy (exit $code)"; tail -n 5 "$dir/out.log"; return 1 ;;
  esac
  if [ "$secs" -gt "$FAST_SECONDS" ]; then
    fail "$1: keploy took ${secs}s, more than ${FAST_SECONDS}s"
    return 1
  fi
}

# says <case> <message>: the case's log says <message>.
says() {
  if ! grep -aqF "$2" "$WORK/$1/out.log"; then
    fail "$1: the log does not say \"$2\""
    tail -n 5 "$WORK/$1/out.log"
  fi
}

# refuses <case> <message>...: keploy stopped at once, non-zero, saying each
# <message>.
refuses() {
  local c=$1 m
  shift
  returned "$c" || return
  if [ "$(cat "$WORK/$c/exit")" -eq 0 ]; then
    fail "$c: keploy exited 0 on a file it cannot read"
  fi
  for m in "$@"; do
    says "$c" "$m"
  done
}

# succeeds <case>: keploy returned fast, and exited 0.
succeeds() {
  returned "$1" || return
  if [ "$(cat "$WORK/$1/exit")" -ne 0 ]; then
    fail "$1: keploy report exited $(cat "$WORK/$1/exit")"
    tail -n 5 "$WORK/$1/out.log"
  fi
}

mkcase fifo-config
mkfifo "$WORK/fifo-config/keploy.yml"
run fifo-config
refuses fifo-config "keploy.yml: not a regular file: it is a FIFO"

mkcase devzero-config
ln -s /dev/zero "$WORK/devzero-config/keploy.yml"
run devzero-config
refuses devzero-config "keploy.yml: not a regular file: it is a device"

mkcase fifo-override
printf 'path: ""\n' >"$WORK/fifo-override/keploy.yml"
mkfifo "$WORK/fifo-override/fifo-override.keploy.yml"
run fifo-override
refuses fifo-override "failed to merge override config file" "fifo-override.keploy.yml: not a regular file: it is a FIFO"

# A report, or the reports directory, keploy cannot read is reported as an
# error, as any unreadable report is. (`keploy report` then exits 0, as it
# does for any report it cannot read; that is not what this checks.)
for c in fifo-report devzero-report fifo-reports-dir; do
  mkcase "$c"
  printf 'path: ""\n' >"$WORK/$c/keploy.yml"
  mkdir -p "$WORK/$c/keploy"
done
mkdir -p "$WORK/fifo-report/keploy/reports/test-run-0" "$WORK/devzero-report/keploy/reports/test-run-0"
mkfifo "$WORK/fifo-report/keploy/reports/test-run-0/test-set-0-report.yaml"
ln -s /dev/zero "$WORK/devzero-report/keploy/reports/test-run-0/test-set-0-report.yaml"
mkfifo "$WORK/fifo-reports-dir/keploy/reports"
for c in fifo-report devzero-report fifo-reports-dir; do
  run "$c"
  m="not a regular file"
  [ "$c" = fifo-reports-dir ] && m="not a directory: it is a FIFO"
  returned "$c" && says "$c" "$m"
done

mkcase large-report
printf 'path: ""\n' >"$WORK/large-report/keploy.yml"
mkdir -p "$WORK/large-report/keploy/reports/test-run-0"
# 1200 passing tests, each with a 20 KB JSON body that the matcher records as
# expected and actual too, then a failed one: 75 MB, shaped like a report
# keploy writes (reportdb's TestGetReportReadsALargeReportKeployWrites reads
# back one that keploy's own InsertReport wrote).
# `keploy report` prints the failed test only if it read the report.
awk 'BEGIN {
  body = "{\"items\":["
  for (i = 0; length(body) < 20480; i++) body = body sprintf("%s{\"id\":%d,\"name\":\"item-%d\"}", (i ? "," : ""), i, i)
  body = body "]}"
  print "version: api.keploy.io/v1beta1"; print "name: test-set-0-report"; print "status: FAILED"
  print "success: 1200"; print "failure: 1"; print "total: 1201"; print "tests:"
  for (t = 0; t <= 1200; t++) {
    status = t < 1200 ? "PASSED" : "FAILED"; actual = t < 1200 ? body : "{}"
    print "    - kind: Http"; print "      name: test-set-0"; printf "      status: %s\n", status
    printf "      test_case_id: test-%d\n", t
    print "      resp:"; print "        status_code: 200"; printf "        body: \047%s\047\n", actual
    print "      result:"; print "        body_result:"; printf "            - normal: %s\n", (t < 1200 ? "true" : "false")
    print "              type: JSON"
    printf "              expected: \047%s\047\n", body; printf "              actual: \047%s\047\n", actual
  }
}' >"$WORK/large-report/keploy/reports/test-run-0/test-set-0-report.yaml"
# Reading 75 MB takes CI's race-built binary about 25 s and 780 MB (1 to 3 s
# and 260 MB without -race). This case shows keploy reads what it writes, not
# that a read is bounded, so it runs with room: 180 s and 2 GiB.
run large-report 180 2g
if [ "$(cat "$WORK/large-report/exit")" -ne 0 ] || ! grep -aqF "test-1200" "$WORK/large-report/out.log"; then
  fail "large-report: keploy did not read a report it writes (exit $(cat "$WORK/large-report/exit"))"
  tail -n 5 "$WORK/large-report/out.log"
fi

mkcase control
printf 'path: ""\nappName: demo\n' >"$WORK/control/keploy.yml"
run control
succeeds control

if [ "$FAIL" -ne 0 ]; then
  echo "config-guard e2e FAILED"
  exit 1
fi
echo "config-guard e2e passed"
