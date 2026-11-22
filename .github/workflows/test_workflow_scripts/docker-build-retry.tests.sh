#!/usr/bin/env bash
# Tests for docker-build-retry.sh's docker_build_retry: which failed builds it
# builds again, and which it fails on the first attempt. Plain bash with no
# Docker, so it runs unchanged on the self-hosted macOS runners' bash 3.2 and
# BSD tools and on Linux (macos-ci-scripts.yml runs it on both), and locally:
#
#   bash .github/workflows/test_workflow_scripts/docker-build-retry.tests.sh
#
# Each case is a "build" that prints a build log from docker-build-retry.tests.d
# and fails, run through docker_build_retry with no backoff, under the shell
# options the lanes use (set -euo pipefail) and under none.
#
# The logs in docker-build-retry.tests.d:
#   buildkit-pip-read-timeout-macos.log  job 119763088290's `docker build`
#       (run 39760264652, the macOS flask-mongo lane), verbatim, timestamps
#       cut: pip's read timeout part-way through a wheel, which the helper
#       refused to build again ("not a transient fetch").
#   pip-*.log, go-compile-error.log, py-compile-error.log,
#   py-attribute-error.log, py-syntax-error-names-timeout-class.log  real
#       output of pip (24.2, 25.0.1, 25.1.1 and 26.0.1, against
#       pip-stubs.py's servers and 192.0.2.1), `go build`, `python -m
#       py_compile` and python, written by capture.sh, which says which pip
#       wrote which; and
#       buildkit-pip-missing-package.log and
#       buildkit-recovered-pip-retry-then-syntax-error.log, which capture.sh
#       makes of them in the framing of the first log.
#   buildkit-docker-hub-rate-limit.log, buildkit-go-proxy-stream-error.log,
#   buildkit-recovered-go-io-timeout-then-compile-error.log  written by hand
#       in the framing of echo-sql's compose build (job 119763088304): a
#       Docker Hub 429 at "load metadata", the module proxy's stream error
#       that docker-build-retry.sh quotes, and a `go mod download` loop that
#       met an i/o timeout and went on before the compile failed.
#   buildkit-two-failed-steps-pip-timeout-and-compile-error.log  written by
#       hand: a compose build of two services in which both steps that
#       failed say ERROR, the first log's pip step (its lines as they are,
#       but for the step's label) and go-compile-error.log's compile error.
#   buildkit-failed-step-number-prefix.log  written by hand, cut to the two
#       steps that matter: the failed step is #2, and #21 met an i/o
#       timeout and went on.
set -u

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
helper="$here/docker-build-retry.sh"
logs="$here/docker-build-retry.tests.d"
work="$(mktemp -d "${TMPDIR:-/tmp}/docker-build-retry-tests.XXXXXX")"
trap 'rm -rf "$work"' EXIT
echo "bash $BASH_VERSION on $(uname -s)"

failures=0
fail() {
  echo "FAIL - $*"
  failures=$((failures + 1))
}

# fake_build: one attempt of a build. Attempt N prints the file named by the
# Nth line of $work/outputs and exits with the Nth line of $work/codes. The
# build runs in a pipeline, so it counts its attempts in $work/attempts.
fake_build() {
  local n out code
  n=$(($(cat "$work/attempts") + 1))
  echo "$n" >"$work/attempts"
  out="$(sed -n "${n}p" "$work/outputs")"
  code="$(sed -n "${n}p" "$work/codes")"
  if [ -z "$code" ]; then
    echo "fake_build: no attempt $n was expected" >&2
    return 99
  fi
  [ -n "$out" ] && cat "$out"
  return "$code"
}

# run_build <mode> <log:code>...: docker_build_retry over fake_build, one
# log:code per attempt it may make: a log in docker-build-retry.tests.d, an
# absolute path, or - for none. mode strict
# runs it under set -euo pipefail, as the lanes do; plain under no options.
# Leaves its output in $out, its exit code in $rc and the attempts in $tries.
run_build() {
  local mode="$1" spec
  shift
  : >"$work/outputs"
  : >"$work/codes"
  echo 0 >"$work/attempts"
  for spec in "$@"; do
    case "${spec%%:*}" in
      -) echo >>"$work/outputs" ;;
      /*) echo "${spec%%:*}" >>"$work/outputs" ;;
      *) echo "$logs/${spec%%:*}" >>"$work/outputs" ;;
    esac
    echo "${spec##*:}" >>"$work/codes"
  done
  (
    if [ "$mode" = strict ]; then set -euo pipefail; fi
    # shellcheck source=docker-build-retry.sh
    source "$helper"
    DOCKER_BUILD_RETRY_ATTEMPTS=$# DOCKER_BUILD_RETRY_BACKOFF=0 docker_build_retry fake_build
  ) >"$work/out" 2>&1
  rc=$?
  out="$(LC_ALL=C tr -d '\000' <"$work/out")"
  tries="$(cat "$work/attempts")"
}

# retried <log>: a build that fails so is built again, and fails at the end
# with the "after N attempts" error once every attempt failed so.
retried() {
  local mode
  [ -f "$logs/$1" ] || { fail "$1: no such log"; return; }
  for mode in strict plain; do
    run_build "$mode" "$1:1" "$1:1"
    if [ "$tries" -ne 2 ] || [ "$rc" -ne 1 ] ||
      ! grep -q 'Transient fetch failure' <<<"$out" ||
      ! grep -q "failed after 2 attempts" <<<"$out"; then
      fail "$1 ($mode): want two attempts and exit 1 after both; got $tries attempt(s), exit $rc"
      grep -E '^docker_build_retry:|::error::|Transient' <<<"$out" | sed 's/^/    | /' | cut -c1-200
      continue
    fi
    # ...and a build that then succeeds is a success.
    run_build "$mode" "$1:1" "-:0"
    if [ "$tries" -ne 2 ] || [ "$rc" -ne 0 ]; then
      fail "$1 ($mode): want the second attempt's success; got $tries attempt(s), exit $rc"
      continue
    fi
    echo "ok   - $1 ($mode): built again"
  done
}

# refused <log>: a build that fails so is not built again: one attempt, exit
# 1, the "Not retrying" error.
refused() {
  local mode
  [ -f "$logs/$1" ] || { fail "$1: no such log"; return; }
  for mode in strict plain; do
    run_build "$mode" "$1:1" "-:0"
    if [ "$tries" -ne 1 ] || [ "$rc" -ne 1 ] || ! grep -q 'Not retrying' <<<"$out"; then
      fail "$1 ($mode): want one attempt and exit 1 with 'Not retrying'; got $tries attempt(s), exit $rc"
      grep -E '^docker_build_retry:|::error::|Transient' <<<"$out" | sed 's/^/    | /' | cut -c1-200
      continue
    fi
    echo "ok   - $1 ($mode): not built again"
  done
}

# whole_log_matches <log>: the control for a case refused because only the
# failed step is read: the whole log does hold a word the match takes.
whole_log_matches() {
  if ! (source "$helper" && grep -qiE "$DOCKER_BUILD_RETRY_RE" "$logs/$1"); then
    fail "$1 holds no word DOCKER_BUILD_RETRY_RE takes, so its case proves nothing"
  else
    echo "ok   - $1: the whole log holds a word the match takes (the control)"
  fi
}

# The failure the helper refused on the macOS lane, pip's other fetches that
# fail in transport for good, and the failures the helper was first made for.
retried buildkit-pip-read-timeout-macos.log
retried pip-index-read-timeout.log
retried pip-index-connect-timeout.log
retried pip-wheel-connect-timeout.log
retried pip-wheel-connect-timeout-no-retries.log
retried pip-wheel-hang-retries-spent.log
retried pip-wheel-hang-no-retries.log
retried pip-wheel-read-timeout-traceback.log
retried pip-wheel-read-timeout-incomplete-download.log
retried pip-wheel-read-timeout-resumed-incomplete-download.log
retried pip-wheel-cut-short-incomplete-download.log
retried pip-wheel-cut-short-resumed-incomplete-download.log
retried buildkit-docker-hub-rate-limit.log
retried buildkit-go-proxy-stream-error.log

# Failures that are not transport. The Go one is the syntax error a match on
# "unexpected EOF" once built again four times.
refused pip-missing-package.log
refused buildkit-pip-missing-package.log
refused pip-wheel-cut-short-invalid-wheel.log
refused pip-wheel-cut-short-invalid-wheel-pip24.log
refused go-compile-error.log
refused py-compile-error.log
# Failures that name a timeout class without being a timeout.
refused py-attribute-error.log
refused py-syntax-error-names-timeout-class.log

# A timeout or a reset that pip got past, with tries left, is not why the
# step failed: a package that does not exist, in the same `pip install`, is
# not built again.
refused pip-recovered-retry-then-missing-package.log
refused pip-recovered-reset-then-missing-package.log

# A step that met a transient fault and got past it is not the step that
# failed: the error of a later step is not built again. In the pip one, pip got
# through on its last try, a line the match does take.
refused buildkit-recovered-pip-retry-then-syntax-error.log
whole_log_matches buildkit-recovered-pip-retry-then-syntax-error.log
refused buildkit-recovered-go-io-timeout-then-compile-error.log
whole_log_matches buildkit-recovered-go-io-timeout-then-compile-error.log
# ...and a step whose number begins with the failed step's (#21, #2) is
# another step.
refused buildkit-failed-step-number-prefix.log
whole_log_matches buildkit-failed-step-number-prefix.log

# Every step that failed is read, not only the first: when two services'
# builds fail together, one on a compile error and one on a pip timeout, the
# build is built again (docker-build-retry.sh's scope caveat).
retried buildkit-two-failed-steps-pip-timeout-and-compile-error.log

# Bytes that are not text (a NUL, invalid UTF-8), in an earlier step and in
# the failed step itself, do not hide the failed step's lines from the match.
binary="$work/binary.log"
macos="$logs/buildkit-pip-read-timeout-macos.log"
at="$(grep -n '^#8 \[4/4\]' "$macos" | cut -d: -f1)"
{
  printf '#6 [2/4] WORKDIR /app\n#6 0.101 \000\377\376 not text\n#6 DONE 0.1s\n\n'
  sed -n "1,${at}p" "$macos"
  printf '#8 1.100 \000\377\376 not text\n'
  sed -n "$((at + 1)),\$p" "$macos"
} >"$binary"
for mode in strict plain; do
  run_build "$mode" "$binary:1" "-:0"
  if [ "$tries" -ne 2 ] || [ "$rc" -ne 0 ]; then
    fail "a log with bytes that are not text, the failed step's too ($mode): want it built again; got $tries attempt(s), exit $rc"
  else
    echo "ok   - a log with bytes that are not text, the failed step's too ($mode): built again"
  fi
done

# ...nor on the failed step's own "#N ERROR:" line (a RUN line that is not
# UTF-8), in a UTF-8 locale, as the runners have, unless the helper reads the
# line as bytes. GNU sed then takes the step number to be "8", the bytes and
# the rest of the line, so the failed step's lines are not read and a timeout
# is refused; BSD sed stops on "illegal byte sequence", so the whole log is
# read and a compile break after a timeout the build got past is built again.
# One case for each.
utf8="$(locale -a 2>/dev/null | grep -iE '^(c|en_us)\.utf-?8$' | head -n 1)"
if [ -z "$utf8" ]; then
  fail "no C or en_US UTF-8 locale here (locale -a), so the case of an ERROR line that is not UTF-8 cannot run"
else
  error_line="$work/binary-error-line.log"
  LC_ALL=C sed "s/^\\(#8 ERROR: .*requirements\\.txt\\)/\\1 $(printf '\377\376')/" "$macos" >"$error_line"
  if ! LC_ALL=C grep -q "$(printf '^#8 ERROR: .*\377\376')" "$error_line"; then
    fail "could not put bytes that are not text on the #8 ERROR: line, so its case proves nothing"
  fi
  for mode in strict plain; do
    LC_ALL="$utf8" run_build "$mode" "$error_line:1" "-:0"
    if [ "$tries" -ne 2 ] || [ "$rc" -ne 0 ]; then
      fail "a failed step whose ERROR line is not text, in $utf8 ($mode): want it built again; got $tries attempt(s), exit $rc"
    else
      echo "ok   - a failed step whose ERROR line is not text, in $utf8 ($mode): built again"
    fi
  done
  recovered="$work/binary-error-line-recovered.log"
  LC_ALL=C sed "s/^\\(#10 ERROR: .*go build -o main \\.\\)/\\1 $(printf '\377\376')/" \
    "$logs/buildkit-recovered-go-io-timeout-then-compile-error.log" >"$recovered"
  if ! LC_ALL=C grep -q "$(printf '^#10 ERROR: .*\377\376')" "$recovered"; then
    fail "could not put bytes that are not text on the #10 ERROR: line, so its case proves nothing"
  fi
  for mode in strict plain; do
    LC_ALL="$utf8" run_build "$mode" "$recovered:1" "-:0"
    if [ "$tries" -ne 1 ] || [ "$rc" -ne 1 ] || ! grep -q 'Not retrying' <<<"$out"; then
      fail "a compile break after a recovered timeout, its ERROR line not text, in $utf8 ($mode): want one attempt and exit 1 with 'Not retrying'; got $tries attempt(s), exit $rc"
    else
      echo "ok   - a compile break after a recovered timeout, its ERROR line not text, in $utf8 ($mode): not built again"
    fi
  done
fi

if [ "$failures" -gt 0 ]; then
  echo "$failures case(s) failed"
  exit 1
fi
echo "all docker_build_retry cases passed"
