#!/usr/bin/env bash
# Shared bounded retry for the go commands that download modules.
#
# Go has no built-in retry for module download, so ONE transient error from
# proxy.golang.org kills the whole job — and it kills whichever lanes happen to
# be resolving modules at that moment, which reads as a code failure in an
# unrelated PR. Observed on run 34349029943, where three lanes died inside two
# minutes on three different modules:
#
#   go.mongodb.org/mongo-driver@v1.8.1: read "https://proxy.golang.org/...zip":
#     stream error: stream ID 1; INTERNAL_ERROR; received from peer
#
# Wraps `go <args>`, not `go build` specifically, because the download is just
# as often done by `go mod tidy`, `go mod download`, `go install` or
# `go list -m -u` — and in a script under `set -e` an unwrapped `go mod tidy`
# aborts before a wrapped build is ever reached, so guarding only the build
# guards nothing.
#
# WHY ONLY TRANSIENT FAILURES ARE RETRIED
#
# Same reasoning docker-build-retry.sh sets out for its own regex: retrying
# every failure turns a genuine 30-second compile break into a multi-minute one
# and prints the compiler error four times, pushing the real message screens
# above the tail of the log. A failure that does not look transient aborts on
# the first attempt with its own exit code. The pattern is shared with
# docker-build-retry.sh rather than reinvented.
#
# WHY THE PROXY IS RETRIED BEFORE GOING DIRECT
#
# The observed failure is a transient HTTP/2 stream error, so the same proxy on
# a fresh connection is both the cheapest retry (a zip GET, not a full VCS
# fetch) and the likeliest to work. `direct` is also strictly riskier: for a
# module whose upstream was renamed, deleted or retagged, direct fails
# PERMANENTLY where the proxy would still serve it, turning a recoverable blip
# into a red lane with a more confusing error. So: proxy on attempts 1-2,
# direct from 3 — the schedule of the inline download_go_modules this replaces.
#
# Set GO_RETRY_DIRECT_FROM to a number past max attempts to never go direct.
# check-deprecated-deps.sh needs that: `go list -m -u all` asks about every
# module in the graph, and direct would git ls-remote hundreds of upstreams.
#
# Usage:  source .../go-retry.sh
#         go_retry build -o app .
#         go_retry mod tidy
#
# NEVER wrap `go test`. Retrying a suite hides a flaky test instead of fixing
# it, and the transient regex is matched against stdout as well as stderr — so
# ordinary test output containing "connection reset by peer" would arm the retry
# on a genuine failure.

# The transport subset of docker-build-retry.sh's DOCKER_BUILD_RETRY_RE (its
# rate-limit terms are Docker-specific), plus the two proxy HTTP errors go
# surfaces. NOT a copy of that list, and deliberately narrower in one place:
#
#   `unexpected EOF` is matched ONLY when qualified by a URL. cmd/compile
#   prints "syntax error: unexpected EOF, expected }" for any unterminated
#   brace — verified against the real toolchain — and docker-build-retry.sh
#   documents removing that bare pattern because it retried a syntax error four
#   times. But a truncated proxy zip genuinely surfaces as
#   `read "https://proxy.golang.org/…zip": unexpected EOF`, which is transient
#   and worth retrying. cmd/compile never emits a URL, so anchoring on the read
#   keeps the transient case and excludes the compile break.
#
#   429/500/504 are included because proxy.golang.org really does return them —
#   it rate-limits by egress IP, which on a shared-NAT runner is the same
#   scenario docker-build-retry.sh documents for Docker Hub. Matched on wording,
#   never on bare digits, which also occur in byte counts and timestamps.
#
#   Bare `no such host` is excluded: it lives in DOCKER_PULL_RETRY_RE, not the
#   build one, because a dead host is usually permanent.
GO_RETRY_RE=${GO_RETRY_RE:-'stream error: stream id|INTERNAL_ERROR; received from peer|tls handshake timeout|i/o timeout|connection reset by peer|500 Internal Server Error|502 Bad Gateway|503 Service Unavailable|504 Gateway Time-?out|429 Too Many Requests|too many requests|read "https?://[^"]*": unexpected EOF'}

go_retry() {
  local attempt=1
  local max_attempts="${GO_RETRY_MAX_ATTEMPTS:-4}"
  local direct_from="${GO_RETRY_DIRECT_FROM:-3}"
  local sleep_sec="${GO_RETRY_BACKOFF:-5}"
  local proxy rc out_f err_f fifo_f tee_pid
  # Explicit template so the names are greppable in a log and portable to
  # BSD mktemp; no lane that sources this runs on macOS today.
  out_f="$(mktemp "${TMPDIR:-/tmp}/go-retry-out.XXXXXX")"
  err_f="$(mktemp "${TMPDIR:-/tmp}/go-retry-err.XXXXXX")"
  fifo_f="${err_f}.fifo"
  # Streams stay SEPARATE. Merging them would corrupt any caller that captures
  # the command's output — check-deprecated-deps.sh does
  # `output=$(go_retry list -m -u all)`, and go writes "go: downloading …"
  # progress to stderr, which would land in the parsed value. Every message
  # this function emits itself goes to stderr for the same reason.

  while [ "$attempt" -le "$max_attempts" ]; do
    # Never override a GOPROXY the caller set on purpose (GOPROXY=off is an
    # assertion that nothing may touch the network).
    if [ -n "${GOPROXY:-}" ] || [ "$attempt" -lt "$direct_from" ]; then
      proxy="${GOPROXY:-https://proxy.golang.org,direct}"
    else
      proxy="direct"
    fi

    # An `if` condition is exempt from errexit, so a failing go does not abort
    # the CALLER while this function decides whether to retry. Deliberately not
    # `set +e; …; set -e`: that would turn errexit ON for callers that never
    # had it, and their next failure anywhere would abort the script.
    # stderr streams LIVE and is captured. `go: downloading …` progress is the
    # only signal of what a stalled download is doing, and a step that hits
    # timeout-minutes is SIGKILLed — with plain redirection nothing would have
    # been replayed yet and the command's entire output would be lost, which is
    # exactly the case this file exists for. stdout stays file-only so a
    # caller's `output=$(go_retry …)` is unaffected.
    #
    # A FIFO with a real background PID, NOT `2> >(tee …)`: process
    # substitution does not set $!, so the `wait $!` such a version needs
    # degrades to a bare `wait` that blocks on EVERY child — and these scripts
    # background the application under test, so that deadlocks the lane.
    mkfifo "$fifo_f" || { echo "::error::go_retry: cannot create fifo" >&2; rm -f "$out_f" "$err_f" "$fifo_f"; return 1; }
    tee "$err_f" >&2 < "$fifo_f" &
    tee_pid=$!
    if GOPROXY="$proxy" go "$@" >"$out_f" 2>"$fifo_f"; then
      rc=0
    else
      rc=$?
    fi
    # Unbounded by design: it is what guarantees err_f is complete before the
    # transient-match grep below reads it. A process that outlived go while
    # holding the inherited stderr fd would stall here, but go reaps its own
    # children, so the only candidates are git/ssh helpers on the direct-mode
    # attempts, which the runners do not spawn.
    wait "$tee_pid" 2>/dev/null || true
    rm -f "$fifo_f"
    # stdout reaches the caller ONLY on success. A failed attempt's partial
    # stdout would otherwise land in `output=$(go_retry …)` — `go list -m -u all`
    # streams module lines as it resolves, so a mid-way failure would inject
    # hundreds of them into the captured value, two or three times over.
    if [ "$rc" -eq 0 ]; then
      cat "$out_f"
    else
      cat "$out_f" >&2
    fi

    if [ "$rc" -eq 0 ]; then
      rm -f "$out_f" "$err_f" "$fifo_f"
      return 0
    fi
    if ! grep -qiE "$GO_RETRY_RE" "$out_f" "$err_f"; then
      echo "::error::go $* failed (exit ${rc}) and the output does not look transient — not retrying" >&2
      rm -f "$out_f" "$err_f" "$fifo_f"
      return "$rc"
    fi
    if [ "$attempt" -ge "$max_attempts" ]; then
      echo "::error::go $* failed after ${max_attempts} attempts (last GOPROXY=${proxy})" >&2
      rm -f "$out_f" "$err_f" "$fifo_f"
      return "$rc"
    fi
    echo "go $* attempt ${attempt} failed transiently; retrying in ${sleep_sec}s (attempt $((attempt + 1))/${max_attempts})…" >&2
    sleep "$sleep_sec"
    sleep_sec=$((sleep_sec * 2))
    attempt=$((attempt + 1))
  done
  # Reached only when max_attempts < 1 (e.g. a bad override): never silently
  # report success for a command that was never run.
  echo "::error::go $* was never attempted (GO_RETRY_MAX_ATTEMPTS=${max_attempts})" >&2
  rm -f "$out_f" "$err_f" "$fifo_f"
  return 1
}
