# Retry wrapper for image builds that pull public base images.
#
# Sourced via:
#
#   source "${GITHUB_WORKSPACE:-${PWD%/samples-*}}/.github/workflows/test_workflow_scripts/docker-build-retry.sh"
#
# WHY THIS EXISTS
#
# Docker Hub meters anonymous pulls at 100 manifest requests per rolling
# 3600s window, keyed to the *egress IP* rather than to the machine or the
# job. Every runner behind a shared NAT therefore spends from one bucket,
# so a build can 429 because of traffic this repo never generated. When it
# happens BuildKit fails at `[internal] load metadata for <image>` and no
# image is produced, after which every later step in the script fails for a
# reason that has nothing to do with the real cause.
#
# Note this is a *mitigation, not a cure*: a retry cannot manufacture pull
# budget. It helps because the window is rolling rather than an hourly
# reset, so capacity trickles back continuously. The durable fixes are an
# authenticated pull (which moves the meter off the shared IP onto an
# account) or a pull-through registry mirror; both need credentials or
# daemon config that this script cannot provide.
#
# WHY ONLY TRANSIENT FETCHES ARE RETRIED
#
# Retrying every failure would turn a genuine 30-second compile break into
# a multi-minute one and push the compiler error far up the log. So a
# failure that is not a rate limit or a fetch that failed in transport (both
# below) aborts on the first attempt (exit 1; the ::error:: line names the
# build's own exit code).
#
# The match is on the *wording* and never on a bare "429": those three
# digits also occur in ISO-8601 timestamps and layer byte counts in this
# very output, and matching them would silently reclassify real breakage
# as a rate limit.
#
# THE MODULE PROXY IS THE OTHER TRANSIENT DEPENDENCY FETCH
#
# A build inside these images downloads Go modules, and proxy.golang.org
# fails the same way a registry does. Observed on the echo-sql lane, after
# the build had sat for two minutes:
#
#   github.com/valyala/fasttemplate@v1.2.1:
#     read "https://proxy.golang.org/@v/v1.2.1.zip":
#     stream error: stream ID 43; INTERNAL_ERROR; received from peer
#
# Only the HTTP/2 TRANSPORT wording is matched, and the reason is narrow: a
# fetch that dies in transport says so, whereas a compiler failure never does.
# Note the failure above is still emitted from a compiler line (it surfaces
# while type-checking an import 120s in), so "the compile had not started" is
# NOT the discriminator — the transport phrasing is.
#
# Two patterns were tried and removed after they collided:
#
#   unexpected eof   — cmd/compile prints "syntax error: unexpected EOF,
#                      expected }" for ANY unterminated brace, and bash prints
#                      it for an unbalanced quote in a RUN line. Matching it
#                      retried a syntax error four times and pushed the
#                      compiler line four builds up the log — exactly what the
#                      paragraph above exists to prevent.
#   proxy.golang.org — a hostname, not an error. Under the default
#                      GOPROXY=...,direct a permanent dependency error (bad
#                      checksum, unknown revision, missing module) falls through
#                      to git and never prints it, so it buys nothing; but one
#                      `go mod download -x` or an echoed ENV line would put it
#                      in the log and make that lane retry everything.
#
# Like the rate-limit handling above, this is a MITIGATION, not a cure. The cure
# is that the sample Dockerfiles run `go build` with no
# --mount=type=cache,target=/go/pkg/mod, so every lane re-fetches the whole
# module graph over the network on every build; a cache mount or vendoring in
# the samples repos would remove the exposure instead of retrying it.
#
# PIP TIMES OUT THE SAME WAY
#
# The Python lanes' images run `pip3 install -r requirements.txt`, and PyPI
# fails in transport just as the module proxy does. Observed on the macOS
# flask-mongo lane (job 119763088290), where all but the first wheel came down
# at 35-58 kB/s and pip's 15 s read timeout ran out part-way through jinja2:
#
#   pip._vendor.urllib3.exceptions.ReadTimeoutError:
#     HTTPSConnectionPool(host='files.pythonhosted.org', port=443): Read timed out.
#
# pip fetches with urllib3, which names a fetch that timed out by its exception
# class, ReadTimeoutError or ConnectTimeoutError. Only the places where pip
# prints the class for a fetch that failed for good are matched:
#   - the last line of the traceback pip dies with, ".exceptions.
#     ReadTimeoutError: HTTPSConnectionPool(...", or HTTPConnectionPool for a
#     plain-HTTP index: a download that timed out part-way, before pip 25.1
#     (the line above, from the lanes' python:3.9 image; pip 25.0.1 prints
#     the same). With the pool after it, not the class alone: a SyntaxError
#     prints the source line it failed on, and a line such as "except
#     urllib3.exceptions.ReadTimeoutError: as err:" is the developer's code,
#     not a fetch;
#   - "(Caused by ConnectTimeoutError(...))", or ReadTimeoutError, in the
#     error pip gives up a file download with, its retries spent;
#   - the Retrying line of a request's last try, "Retrying (Retry(total=0,
#     ...)) after connection broken by 'ReadTimeoutError(...)'". When that try
#     fails too on an index page, pip's own last words are "No matching
#     distribution found", the very words of a package that does not exist,
#     and this line is what tells the two apart.
# A Retrying line with tries left (total=4 down to 1) is dropped before the
# match, whatever broke the connection, a timeout or a reset: pip gets past
# most of those, and the same `pip install` may then fail for a reason of its
# own, such as a package that does not exist. Only a request that failed five
# times and then got through would still make such a failure look transient.
# pip 25.1 and later stop a download that stalls or is cut short without
# naming the class, and say so in their own code, "error: incomplete-download"
# ("This is an issue with network connectivity, not pip"), matched too (pip
# 25.1.1 says it at once, 26.0.1 after resuming five times). A plain "timed
# out" is not: it is anybody's words, a test's or a tool's.
#
# Not matched: pip 24.2 and 25.0.1 take a download cut short for a whole one
# and then report "Wheel '...' is invalid", which is also what a broken wheel
# gets; that stays a failure. Nor is a failure that only names a class, such as
# Python's "Did you mean: 'ReadTimeoutError'?" or the SyntaxError above.
#
# As for Go modules, this is a mitigation; the cure is in the samples repos.
# flask-mongo's Dockerfile runs `COPY . .` before `pip3 install -r
# requirements.txt`, and the lane rewrites files in that context before every
# build, so the install layer is never reused and every build fetches every
# wheel again. Copying requirements.txt alone and installing before the rest
# (with --mount=type=cache,target=/root/.cache/pip) would leave the shared
# Docker daemon of each Mac nothing to fetch from PyPI.
#
# docker-build-retry.tests.sh runs this helper over pip's own output for each
# of these (docker-build-retry.tests.d, which says where each log comes from).
#
# ONLY THE STEP THAT FAILED IS READ
#
# A step can meet a transient fault, get past it, and succeed: a `go mod
# download` retry loop prints its i/o timeout and goes on. If a later step then
# fails for a real reason, the whole log still holds the earlier step's words,
# and a compile break would be retried four times. So when the log is
# BuildKit's (every line of a step starts with its number, "#8 ...", and a step
# that failed says "#8 ERROR: ..."), only the failed steps' lines are matched.
# A Docker Hub rate limit is among them: BuildKit puts it on the failed step's
# own ERROR line ("#2 ERROR: ... 429 Too Many Requests"). Any other log is
# matched whole. Within the failed step only pip's recovered faults are told
# apart (above): a Go fetch fault the step got past, in a `RUN go mod download
# && go build`, still makes that step's compile break look transient.
#
# Scope caveat: the compose call sites build several services at once, and a
# failure in one cancels the others, so a service whose fetch failed in
# transport retries the build of the others too — including a sibling's
# genuine compile break, when it failed before it was cancelled.
DOCKER_BUILD_RETRY_RE="toomanyrequests|too many requests|pull rate limit|rate limit exceeded|stream error: stream id|INTERNAL_ERROR; received from peer|tls handshake timeout|i/o timeout|connection reset by peer|exceptions\\.ReadTimeoutError: HTTPS?ConnectionPool\\(|Caused by (Read|Connect)TimeoutError\\(|Retry\\(total=0, [^)]*\\)\\) after connection broken by '(Read|Connect)TimeoutError\\(|error: incomplete-download"

# _docker_build_failure_text <log>: what the transient-fault match reads in a
# failed build's log: in BuildKit's output, the lines of the steps that failed
# (#N ERROR: ...); in any other, the whole log; either without pip's Retrying
# lines that had tries left. Bytes are read as bytes and NULs dropped, so a
# stray NUL or invalid UTF-8 does not turn a step's lines into "Binary file
# matches" (nor make bash warn that it ignored a null byte). Always succeeds,
# so a caller under `set -e` reads its output.
_docker_build_failure_text() {
    local _log="$1" _ids _id
    _ids="$(LC_ALL=C sed -n 's/^#\([0-9][0-9]*\) ERROR: .*/\1/p' "$_log" | LC_ALL=C sort -u)" || true
    {
        if [ -z "$_ids" ]; then
            cat "$_log"
        else
            for _id in $_ids; do
                LC_ALL=C grep -a "^#${_id} " "$_log" || true
            done
        fi
    } | LC_ALL=C tr -d '\000' | { LC_ALL=C grep -v 'Retrying (Retry(total=[1-9]' || true; }
    return 0
}

# docker_build_retry <command> [args...]
#
# Runs the build, echoing its output, and retries with exponential backoff
# only while the failed step looks transient — a Docker Hub rate limit, or a
# module, registry or PyPI fetch that failed in transport rather than in the
# compiler.
docker_build_retry() {
    local _max="${DOCKER_BUILD_RETRY_ATTEMPTS:-4}"
    local _backoff="${DOCKER_BUILD_RETRY_BACKOFF:-15}"
    local _attempt=1
    local _rc=0
    local _log
    local _failure
    local _restore_errexit=0

    # Several callers run under `set -e`; some do not. Suspend it around the
    # build so a failure lands in $_rc for inspection instead of killing the
    # shell, then put the caller's setting back exactly as it was.
    case "$-" in *e*) _restore_errexit=1 ;; esac

    # Explicit template: BSD mktemp on the macOS runners has no default one.
    _log="$(mktemp "${TMPDIR:-/tmp}/docker-build-retry.XXXXXX")"

    while : ; do
        echo "docker_build_retry: $* (attempt ${_attempt}/${_max})"

        set +e
        "$@" 2>&1 | tee "$_log"
        _rc=${PIPESTATUS[0]}
        if [ "$_restore_errexit" -eq 1 ]; then set -e; fi

        if [ "$_rc" -eq 0 ]; then
            rm -f "$_log"
            return 0
        fi

        # Read into a variable first, so the match does not depend on how
        # the text's writers end: `grep -q` on a pipe quits at its first
        # match, and the callers run under pipefail. Matched as bytes, as
        # it was read: in a UTF-8 locale an invalid byte inside a line can
        # keep that line from matching.
        _failure="$(_docker_build_failure_text "$_log")"
        if ! LC_ALL=C grep -qiE "$DOCKER_BUILD_RETRY_RE" <<<"$_failure"; then
            echo "::error::'$*' failed with exit code ${_rc}. The failure above is not a transient fetch (rate limit or transport), so retrying would only bury it. Not retrying."
            rm -f "$_log"
            exit 1
        fi

        if [ "$_attempt" -ge "$_max" ]; then
            echo "::error::'$*' failed after ${_max} attempts: the last failure is above. A fetch that never clears is usually an egress-IP rate limit or an upstream outage rather than a defect in this change — but read the message rather than assuming."
            rm -f "$_log"
            exit 1
        fi

        echo "Transient fetch failure (exit ${_rc}); retrying in ${_backoff}s…"
        sleep "$_backoff"
        _attempt=$((_attempt + 1))
        _backoff=$((_backoff * 2))
    done
}

# WHAT ELSE IS WORTH RETRYING WHEN FETCHING AN IMAGE
#
# A build fails at "load metadata" when Docker Hub meters it. Fetching an
# image fails the same way, but also on plain network faults that say nothing
# about the image — the daemon could not reach the registry at all:
#
#   docker: Error response from daemon: Get "https://registry-1.docker.io/v2/":
#     net/http: request canceled while waiting for connection
#     (Client.Timeout exceeded while awaiting headers)
#
# That is what this covers. Via `docker run` it surfaces as exit 125 — the
# daemon refusing to create the container — so the lane goes red having
# executed no test and asserted nothing.
#
# The same "only retry what is transient" rule applies, and matters more here
# because the permanent failures are ordinary mistakes: an unknown manifest
# (bad tag), denied access (private image), or no matching platform (an arm64
# runner, an amd64-only image). Retrying those wastes minutes and then blames
# a network fault that never happened, sending the next reader after the wrong
# cause. They abort on the first attempt, showing docker's own message.
DOCKER_PULL_RETRY_RE="${DOCKER_BUILD_RETRY_RE}|client\.timeout|context deadline exceeded|connection refused|temporary failure|no such host|unexpected eof|request canceled"

# _docker_fetch_retry <what> <command> [args...]
#
# Shared body of docker_pull_retry / docker_compose_pull_retry: run the fetch,
# retry only while the failure looks transient, abort otherwise. <what> names
# the target in the messages.
_docker_fetch_retry() {
    local _what="$1"; shift
    local _max="${DOCKER_PULL_RETRY_ATTEMPTS:-4}"
    local _backoff="${DOCKER_PULL_RETRY_BACKOFF:-5}"
    local _attempt=1
    local _rc=0
    local _log
    local _restore_errexit=0

    case "$-" in *e*) _restore_errexit=1 ;; esac
    _log="$(mktemp "${TMPDIR:-/tmp}/docker-pull-retry.XXXXXX")"

    while : ; do
        echo "fetching ${_what}: $* (attempt ${_attempt}/${_max})"

        set +e
        "$@" 2>&1 | tee "$_log"
        _rc=${PIPESTATUS[0]}
        if [ "$_restore_errexit" -eq 1 ]; then set -e; fi

        if [ "$_rc" -eq 0 ]; then
            rm -f "$_log"
            return 0
        fi

        if ! grep -qiE "$DOCKER_PULL_RETRY_RE" "$_log"; then
            echo "::error::fetching ${_what} failed with exit code ${_rc} for a reason retrying will not clear. Docker's own message is above and carries the diagnosis. If it points at the daemon rather than the image, check 'docker info' and free disk on the runner."
            rm -f "$_log"
            exit 1
        fi

        if [ "$_attempt" -ge "$_max" ]; then
            echo "::error::fetching ${_what} did not succeed in ${_max} attempts; the last failure is above. The lane never ran its test."
            rm -f "$_log"
            exit 1
        fi

        echo "Transient registry failure fetching ${_what} (exit ${_rc}); retrying in ${_backoff}s…"
        sleep "$_backoff"
        _attempt=$((_attempt + 1))
        _backoff=$((_backoff * 2))
    done
}

# docker_pull_retry <image>
#
# Fetch an image before the `docker run` that would otherwise pull it
# implicitly.
#
# This does NOT retry a test. It runs before the application starts and
# short-circuits the moment the image is local, so once it returns the lane
# executes exactly once and any later failure stands on its own.
docker_pull_retry() {
    local _image="${1:-}"

    if [ -z "$_image" ]; then
        echo "::error::docker_pull_retry: called with no image"
        exit 1
    fi

    # Already resident from an earlier step or a warm runner cache. Returning
    # here also spends no manifest request, which is the budget the rate limit
    # above meters.
    if docker image inspect "$_image" >/dev/null 2>&1; then
        return 0
    fi

    _docker_fetch_retry "$_image" docker pull "$_image"
}

# docker_compose_pull_retry [compose args...]
#
# The compose equivalent: `docker compose up -d` pulls implicitly too, so a
# compose-started dependency is exposed to exactly the same registry failure
# as a `docker run` one. Pull first so the failure is attributed and retried.
# Unlike the single-image form this cannot short-circuit on one `docker image
# inspect`, so it defers to compose's own per-service resident check via
# --policy missing (see below).
docker_compose_pull_retry() {
    # --policy missing matters: `docker compose pull` defaults to "always" and
    # re-contacts the registry even when every image is already local, so an
    # unconditional pull SPENDS the same metered budget the rate-limit handling
    # above exists to protect. `docker compose up` itself defaults to "missing",
    # so without this flag guarding a lane would cost it more requests than
    # leaving it unguarded. Needs compose >= v2.22.
    _docker_fetch_retry "the compose services" docker compose pull --policy missing "$@"
}
