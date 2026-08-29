#!/usr/bin/env bash
# Self-test for docker-build-retry.sh's failure classifier.
#
# Both directions of getting this wrong are invisible from a green pipeline:
#
#   too narrow -> a transient registry failure fails the whole lane. Run
#                 33064807178 died that way: a 502 from registry-1.docker.io
#                 was classed "not a rate limit" and not retried.
#   too broad  -> a genuine compile break is retried four times, taking minutes
#                 and burying the real error, then blamed on infrastructure.
#
# The second is the easy one to reintroduce, because BuildKit retries 5xx
# internally and narrates the recovered attempt into the build output - so a
# log can contain a 502 and still have failed for an entirely different reason.
# CASE R1 below is that log.
#
# Drives docker_build_retry_is_transient against real files, exactly as the
# wrapper does, rather than matching the pattern directly - so a pattern that
# stops being used, or a classifier that stops reading the terminating error,
# fails here.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
source "$HERE/docker-build-retry.sh" || { echo "::error::cannot source docker-build-retry.sh"; exit 1; }
: "${DOCKER_BUILD_RETRY_RE:?classifier pattern is unset after sourcing}"
if ! declare -f docker_build_retry_is_transient > /dev/null; then
  echo "::error::docker_build_retry_is_transient is not defined after sourcing"
  exit 1
fi

fails=0
cases=0
tmp="$(mktemp -d "${TMPDIR:-/tmp}/retry-selftest.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT

expect() { # expect <RETRY|ABORT> <description> <log text>
  local want="$1" desc="$2" text="$3" got f
  cases=$((cases + 1))
  f="$tmp/log"
  printf '%s\n' "$text" > "$f"
  if docker_build_retry_is_transient "$f"; then got=RETRY; else got=ABORT; fi
  if [ "$got" != "$want" ]; then
    printf 'FAIL  %-54s got %s, want %s\n' "$desc" "$got" "$want"
    fails=$((fails + 1))
  else
    printf 'ok    %-54s %s\n' "$desc" "$got"
  fi
}

# --- the regression that motivated terminal-error classification -----------
# A 502 BuildKit retried and recovered from, followed by a real RUN failure.
# Whole-log matching says RETRY here; only the terminating error says ABORT.
expect ABORT "R1 recovered 502 + genuine RUN failure" \
'#4 0.031 error: failed to copy: httpReadSeeker: failed open: unexpected status from GET request to https://registry-1.docker.io/v2/lib/blobs/sha256:b05: 502 Bad Gateway
#4 0.031 retrying in 1s
#4 sha256:b05 2.23MB / 2.23MB 0.0s done
#5 ERROR: process "/bin/sh -c go build ./..." did not complete successfully: exit code: 2
ERROR: failed to build: failed to solve: process "/bin/sh -c go build ./..." did not complete successfully: exit code: 2'

expect ABORT "R2 recovered 502 + genuine compile error" \
'#6 12.4 error: unexpected status from HEAD request to https://registry-1.docker.io/v2/x/manifests/y: 503 Service Unavailable
#6 12.9 retrying in 2s
#7 14.2 ./main.go:12:2: undefined: fooBar
ERROR: failed to build: failed to solve: process "/bin/sh -c go build" did not complete successfully: exit code: 1'

# --- fatal registry failures: must retry -----------------------------------
# Verbatim shape from run 33064807178 (manifest resolve is NOT wrapped by
# BuildKit's retryhandler, so a single 502 there is terminal).
expect RETRY "fatal 502 at load metadata (run 33064807178)" \
'#3 ERROR: unexpected status from HEAD request to https://registry-1.docker.io/v2/library/golang/manifests/1.20-bookworm: 502 Bad Gateway
failed to solve: golang:1.20-bookworm: failed to resolve source metadata for docker.io/library/golang:1.20-bookworm: unexpected status from HEAD request to https://registry-1.docker.io/v2/library/golang/manifests/1.20-bookworm: 502 Bad Gateway'
expect RETRY "fatal 503" \
'failed to solve: x: unexpected status from GET request to https://registry-1.docker.io/v2/x: 503 Service Unavailable'
expect RETRY "fatal 504" \
'failed to solve: y: unexpected status from HEAD request to https://ghcr.io/v2/y: 504 Gateway Time-out'
# containerd's OTHER phrasing, from the blob/config path. Which one an outage
# produces depends on the containerd version behind the daemon, so both must
# be covered (containerd remotes/docker/fetcher.go vs remotes/errors).
expect RETRY "fatal 502, 'unexpected status code' phrasing" \
'failed to solve: failed to copy: httpReadSeeker: failed open: unexpected status code https://registry-1.docker.io/v2/library/golang/blobs/sha256:abc: 502 Bad Gateway'
expect RETRY "truncated blob transfer (unexpected EOF)" \
'ERROR: failed to build: failed to solve: failed to compute cache key: short read: expected 2226327 bytes but got 32: unexpected EOF'
expect RETRY "connection reset during token fetch" \
'failed to solve: failed to authorize: failed to fetch oauth token: Post "https://auth.docker.io/token": read tcp 10.1.0.4:5->3.4.5.6:443: read: connection reset by peer'
expect RETRY "dial i/o timeout" \
'failed to solve: failed to do request: Head "https://registry-1.docker.io/v2/x": dial tcp 3.4.5.6:443: i/o timeout'
expect RETRY "DNS server misbehaving" \
'failed to solve: lookup registry-1.docker.io on 127.0.0.53:53: server misbehaving'
expect RETRY "context deadline exceeded" \
'ERROR: failed to build: failed to solve: context deadline exceeded'
expect RETRY "http client header timeout" \
'failed to solve: Get "https://registry-1.docker.io/v2/": net/http: Client.Timeout exceeded while awaiting headers'
expect RETRY "TLS handshake timeout" \
'failed to solve: failed to do request: net/http: TLS handshake timeout'

# --- rate limits: each clause needs its own fixture ------------------------
expect RETRY "rate limit: toomanyrequests" \
'failed to solve: golang:1.20: toomanyrequests: You have reached your pull limit.'
expect RETRY "rate limit: 'too many requests'" \
'failed to solve: error getting credentials: too many requests'
expect RETRY "rate limit: 'pull rate limit'" \
'failed to solve: docker.io/library/golang: You have reached your pull rate limit, see https://docs.docker.com/'
expect RETRY "rate limit: 'rate limit exceeded'" \
'failed to solve: registry rate limit exceeded, try again later'

# --- real breakage: must abort on the first attempt ------------------------
expect ABORT "Go compile error" \
'ERROR: failed to build: failed to solve: process "/bin/sh -c go build" did not complete successfully: exit code: 1
./main.go:12:2: undefined: fooBar'
expect ABORT "Dockerfile parse error" \
'dockerfile parse error line 3: unknown instruction: FROMM'
expect ABORT "apt failure inside a layer" \
'#7 3.2 E: Unable to locate package libfoo
ERROR: failed to build: failed to solve: process "/bin/sh -c apt-get install -y libfoo" did not complete successfully: exit code: 100'
# 4xx other than 429 is configuration, not weather: fail fast.
expect ABORT "registry 401 (bad or missing credentials)" \
'failed to solve: unexpected status from HEAD request to https://registry-1.docker.io/v2/p/manifests/x: 401 Unauthorized'
expect ABORT "registry 404 (tag does not exist)" \
'failed to solve: unexpected status from HEAD request to https://registry-1.docker.io/v2/p/manifests/nope: 404 Not Found'

# --- bare digits and unanchored text must never classify -------------------
expect ABORT "502 in a transferred byte count" \
'#8 sha256:abc transferring context: 502 B done
ERROR: failed to build: failed to solve: process "/bin/sh -c make" did not complete successfully: exit code: 2'
expect ABORT "429 in an ISO-8601 timestamp" \
'2026-08-27T11:06:08.4429000Z #5 DONE 0.4s
ERROR: failed to build: failed to solve: process "/bin/sh -c make" did not complete successfully: exit code: 2'
expect ABORT "503 in a layer size" \
'#12 extracting sha256:deadbeef 503 MB / 900 MB
ERROR: failed to build: failed to solve: process "/bin/sh -c make" did not complete successfully: exit code: 2'
# The URL anchoring is load-bearing: a 5xx not terminating a registry URL is
# not a registry failure. Without the anchor this line would retry.
expect ABORT "5xx-looking text with no registry URL" \
'ERROR: failed to build: failed to solve: process "/bin/sh -c ./run.sh" did not complete successfully: server returned: 502'
expect ABORT "app log mentioning an unexpected status, no URL" \
'ERROR: failed to build: failed to solve: unexpected status: 500 from the fixture server'

# --- the anchoring itself, which the header calls load-bearing -------------
# Each of these passes if the corresponding guard is loosened, so they are
# what stops the pattern drifting wider one convenience at a time.
# Guards `https?://`: a 5xx after a bare word is not a registry failure.
expect ABORT "5xx after a non-URL token" \
'failed to solve: unexpected status code app-backend: 500 Internal Server Error'
# Guards `[^ ]+` (URL must not span spaces): otherwise a 404 here plus an
# unrelated 502 later on the same line would combine into a match.
expect ABORT "fatal 404 with an unrelated 5xx later on the line" \
'failed to solve: unexpected status code https://reg/v2/x: 404 Not Found; upstream said: 502'
# Guards `[A-Z]+` (the HTTP verb is one token): arbitrary prose between
# "status from" and "request to" is not containerd's phrasing.
expect ABORT "prose between 'status from' and 'request to'" \
'failed to solve: unexpected status from the cache; request to https://reg/v2/x: 502'
# Guards the full "TLS handshake timeout" phrase: a bare "timeout" appears in
# ordinary build commands.
expect ABORT "the word timeout inside a RUN command" \
'ERROR: failed to build: failed to solve: process "/bin/sh -c go test -timeout 30s ./..." did not complete successfully: exit code: 1'

if [ "$fails" -ne 0 ]; then
  echo "::error::docker-build-retry classifier self-test: $fails case(s) failed"
  exit 1
fi
echo "docker-build-retry classifier self-test: all $cases cases passed"
