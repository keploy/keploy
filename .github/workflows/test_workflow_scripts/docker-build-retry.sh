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
# WHY ONLY THE TERMINATING ERROR IS CLASSIFIED
#
# Retrying every failure would turn a genuine 30-second compile break into a
# multi-minute one and push the compiler error far up the log. So a failure
# that is not demonstrably the registry's aborts on the first attempt.
#
# Rate limiting is one such failure; it is not the only one. Run 33064807178
# (run_golang_docker / echo-sql) died on a 502 from Docker Hub's own gateway,
# at the same `[internal] load metadata` phase this wrapper exists to survive,
# and the wrapper stood aside because it was not a 429.
#
# Broadening to 5xx is NOT as simple as adding it to the pattern, and the
# reason is the crux of this file. BuildKit RETRIES 5xx internally
# (util/resolver/retryhandler) and NARRATES each recovered attempt into the
# visible build output:
#
#   #4 0.031 error: failed to copy: httpReadSeeker: failed open: unexpected
#            status from GET request to https://.../blobs/sha256:...: 502 Bad Gateway
#   #4 0.031 retrying in 1s
#   #4 sha256:... 2.23MB / 2.23MB done          <- recovered, build continued
#   #5 ERROR: process "/bin/sh -c go build ./..." did not complete successfully
#
# A whole-log grep sees that 502 and retries a real compile break four times,
# then blames infrastructure. 429 never had this problem because BuildKit does
# not retry it, so a rate-limit line was always fatal and could never appear in
# a log that then failed for another reason. 5xx is exactly the class it does
# retry and narrate.
#
# So classify the TERMINATING error, not the log. BuildKit ends a failed build
# with a `failed to solve:` line carrying the ultimate cause, and that line
# contains the 502 in the fatal case and does not in the recovered case. That
# single distinction is what makes broadening safe.
#
# Matching stays anchored on wording plus context, never bare digits: "502" and
# "429" both occur in byte counts and ISO-8601 timestamps. A status code counts
# only inside containerd's own phrasing terminating a registry URL. Both of its
# phrasings are covered - `unexpected status from <VERB> request to <url>` from
# the manifest/token path and `unexpected status code <url>` from the blob
# path - because which one an outage produces depends on the containerd version
# behind the daemon.
#
# 5xx only. A 4xx other than 429 is configuration, not weather: 401
# unauthorized and 404 for a nonexistent tag must fail on the first attempt.
DOCKER_BUILD_RETRY_RE='toomanyrequests|too many requests|pull rate limit|rate limit exceeded'\
'|unexpected status (from [A-Z]+ request to|code) https?://[^ ]+: 5[0-9][0-9]'\
'|received unexpected HTTP status: 5[0-9][0-9]'\
'|TLS handshake timeout|i/o timeout|context deadline exceeded'\
'|connection reset by peer|unexpected EOF|server misbehaving'\
'|Client\.Timeout exceeded while awaiting headers'

# docker_build_retry_terminal_error <logfile>
#
# Prints the part of a failed build log that states why it failed, so the
# classifier votes on the cause rather than on anything the build narrated on
# the way past. BuildKit's terminal line is `failed to solve:` (wrapped by
# `ERROR: failed to build:` under compose). If neither is present the command
# was not buildx - a bare `docker pull`, say - so fall back to the tail, which
# for those tools is the error.
docker_build_retry_terminal_error() {
    local _f="$1" _terminal
    # `failed to solve:` / `ERROR: failed to build:` are BuildKit. `returned a
    # non-zero code:` is the classic builder, which still runs on older engines
    # and some self-hosted runners. Without that third marker the classic path
    # always fell through to the tail below, and `go mod download`, apt, npm and
    # pip all narrate-and-recover exactly the transports listed in the pattern -
    # so a recovered i/o timeout could outvote a compile error sitting one line
    # further down.
    _terminal="$(grep -E 'failed to solve:|ERROR: failed to build:|returned a non-zero code:|^ERROR: ' "$_f" 2>/dev/null)"
    if [ -n "$_terminal" ]; then
        printf '%s\n' "$_terminal"
    else
        tail -n 15 "$_f" 2>/dev/null
    fi
}

# docker_build_retry_is_transient <logfile>
#
# True when the terminating error looks like the registry rather than the
# build. The self-test drives this function, not the pattern, so the pattern
# cannot drift out of use without the test noticing.
docker_build_retry_is_transient() {
    : "${DOCKER_BUILD_RETRY_RE:?classifier pattern is unset}"
    grep -qiE "$DOCKER_BUILD_RETRY_RE" <<< "$(docker_build_retry_terminal_error "$1")"
}


# docker_build_retry <command> [args...]
#
# Runs the build, echoing its output, and retries with exponential backoff
# only while the TERMINATING error looks like a transient registry failure.
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

        if ! docker_build_retry_is_transient "$_log"; then
            echo "::error::'$*' failed with exit code ${_rc}. The terminating error is not a transient registry failure, so not retrying."
            rm -f "$_log"
            exit "$_rc"
        fi

        if [ "$_attempt" -ge "$_max" ]; then
            echo "::error::'$*' failed after ${_max} attempts: a transient registry failure (rate limit, or a 5xx/timeout from the registry) did not clear. This is registry-side infrastructure, not a defect in this change."
            rm -f "$_log"
            exit 1
        fi

        echo "Transient registry failure (exit ${_rc}); retrying in ${_backoff}s…"
        sleep "$_backoff"
        _attempt=$((_attempt + 1))
        _backoff=$((_backoff * 2))
    done
}
