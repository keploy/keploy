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
# WHY ONLY RATE LIMITS ARE RETRIED
#
# Retrying every failure would turn a genuine 30-second compile break into
# a multi-minute one and push the compiler error far up the log. So a
# non-rate-limit failure aborts on the first attempt, with its exit code.
#
# The match is on the *wording* and never on a bare "429": those three
# digits also occur in ISO-8601 timestamps and layer byte counts in this
# very output, and matching them would silently reclassify real breakage
# as a rate limit.
DOCKER_BUILD_RETRY_RE='toomanyrequests|too many requests|pull rate limit|rate limit exceeded'

# docker_build_retry <command> [args...]
#
# Runs the build, echoing its output, and retries with exponential backoff
# only while the failure looks like a Docker Hub rate limit.
docker_build_retry() {
    local _max="${DOCKER_BUILD_RETRY_ATTEMPTS:-4}"
    local _backoff="${DOCKER_BUILD_RETRY_BACKOFF:-15}"
    local _attempt=1
    local _rc=0
    local _log
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

        if ! grep -qiE "$DOCKER_BUILD_RETRY_RE" "$_log"; then
            echo "::error::'$*' failed with exit code ${_rc}. Not a Docker Hub rate limit, so not retrying."
            rm -f "$_log"
            exit 1
        fi

        if [ "$_attempt" -ge "$_max" ]; then
            echo "::error::'$*' failed after ${_max} attempts: Docker Hub pull rate limit (HTTP 429) did not clear. This is an infrastructure limit on the runner's egress IP, not a defect in this change."
            rm -f "$_log"
            exit 1
        fi

        echo "Docker Hub rate limit hit (exit ${_rc}); retrying in ${_backoff}s…"
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
DOCKER_PULL_RETRY_RE="${DOCKER_BUILD_RETRY_RE}|client\.timeout|context deadline exceeded|i/o timeout|connection reset|connection refused|temporary failure|no such host|tls handshake timeout|unexpected eof|request canceled"

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
