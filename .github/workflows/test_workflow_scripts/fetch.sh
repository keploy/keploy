#!/usr/bin/env bash
# fetch.sh — the lanes' network fetches: each retried a bounded number of
# times, and none left hanging on a transfer that stopped moving.
#
#   bash fetch.sh clone URL DIR [REF]   shallow fetch of REF (a commit, branch
#                                       or tag; default: the default branch)
#   bash fetch.sh modules [DIR]         go mod download in DIR (default .)
#   bash fetch.sh go ARGS...            go ARGS, through go_retry (go-retry.sh)
#   bash fetch.sh pip ARGS...           $FETCH_PYTHON -m pip install ARGS
#                                       (FETCH_PYTHON: python3, or a venv's python)
#
# THE RULE -- the same as keploy/enterprise's .ci/scripts/fetch.sh and
# keploy/ebpf's .woodpecker/fetch.sh, so a fetch behaves the same in every
# keploy repo's lanes:
#
#   * At most 5 attempts, each from a fresh state (a clone starts from an
#     empty DIR), waiting attempt x 10 s between attempts and not after the
#     last. The wait spaces the attempts so one network blip does not burn
#     them all; it is never a budget on a fetch, which may take as long as it
#     keeps moving. Exhausting the attempts fails loudly with the last status.
#   * A transfer that stops moving is given up on progress, never on time: git
#     abandons an attempt whose transfer stayed under 1000 B/s for 30 s
#     (http.lowSpeedLimit/lowSpeedTime); pip abandons a connection that
#     delivered nothing for 30 s (its socket timeout) and, from pip 25.1,
#     resumes a cut download instead of restarting it (PIP_RESUME_RETRIES;
#     older pips ignore the variable).
#   * Go modules come through GOPROXY="https://proxy.golang.org|direct" (see
#     go-retry.sh): a pipe falls back to the module's origin on any proxy
#     error, where Go's default comma list falls back only on 404/410, so a
#     reset HTTP/2 stream from the proxy (keploy#4791) failed the fetch. A
#     GOPROXY the caller set is kept. Go fetches go through go_retry, which
#     also retries only failures that look transient, because it wraps builds
#     too.
#
# A fetch is idempotent and its failure mode is the network, so retrying it
# cannot hide a regression: a package or module that does not exist fails
# every attempt. Never wrap a test or an assertion in this.
#
# FETCH_ATTEMPTS / FETCH_BASE_DELAY override the count and the backoff base for
# clone and pip in the unit test (fetch-test.sh); no lane sets them.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FETCH_ATTEMPTS=${FETCH_ATTEMPTS:-5}
FETCH_BASE_DELAY=${FETCH_BASE_DELAY:-10}

usage() {
  echo "usage: fetch.sh clone URL DIR [REF] | modules [DIR] | go ARGS... | pip ARGS..." >&2
  exit 2
}

# retry_count WHAT CMD...: CMD, at most FETCH_ATTEMPTS times, attempt x
# FETCH_BASE_DELAY s between attempts; the last attempt's status otherwise.
retry_count() {
  local what=$1 attempt=1 rc
  shift
  while :; do
    rc=0
    "$@" || rc=$?
    [ "$rc" -eq 0 ] && return 0
    if [ "$attempt" -ge "$FETCH_ATTEMPTS" ]; then
      echo "::error::fetch.sh: $what failed after $attempt attempt(s) (exit $rc)" >&2
      return "$rc"
    fi
    echo "fetch.sh: $what failed (exit $rc); attempt $attempt/$FETCH_ATTEMPTS, retrying in $((FETCH_BASE_DELAY * attempt))s" >&2
    sleep "$((FETCH_BASE_DELAY * attempt))"
    attempt=$((attempt + 1))
  done
}

# shown URL: the URL with any credentials in it (https://TOKEN@host/...) left
# out, for the log.
shown() { printf '%s' "$1" | sed -E 's#://[^/@]*@#://#'; }

# One clone attempt, from an empty DIR.
clone_once() {
  local url=$1 dir=$2 ref=$3
  rm -rf "$dir"
  if [ -z "$ref" ]; then
    git clone --depth 1 --no-tags "$url" "$dir"
  else
    git init -q "$dir" &&
      git -C "$dir" remote add origin "$url" &&
      git -C "$dir" fetch -q --depth 1 --no-tags origin "$ref" &&
      git -C "$dir" checkout -q FETCH_HEAD
  fi
}

case ${1:-} in
  clone)
    [ $# -ge 3 ] && [ $# -le 4 ] || usage
    export GIT_HTTP_LOW_SPEED_LIMIT=1000 GIT_HTTP_LOW_SPEED_TIME=30
    retry_count "clone of $(shown "$2")" clone_once "$2" "$3" "${4:-}"
    echo "fetched $(shown "$2")${4:+ @ $4} -> $3 ($(git -C "$3" rev-parse HEAD))"
    ;;
  modules)
    [ $# -le 2 ] || usage
    # shellcheck source=go-retry.sh
    source "$HERE/go-retry.sh"
    cd "${2:-.}"
    go_retry mod download
    ;;
  go)
    [ $# -ge 2 ] || usage
    shift
    # shellcheck source=go-retry.sh
    source "$HERE/go-retry.sh"
    go_retry "$@"
    ;;
  pip)
    [ $# -ge 2 ] || usage
    shift
    export PIP_TIMEOUT="${PIP_TIMEOUT:-30}" PIP_RESUME_RETRIES="${PIP_RESUME_RETRIES:-5}"
    retry_count "pip install $*" "${FETCH_PYTHON:-python3}" -m pip install "$@"
    ;;
  *)
    usage
    ;;
esac
