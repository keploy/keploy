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
