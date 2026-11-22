# shellcheck shell=bash
# Shared by the lanes that start `keploy record` and then wait for the app it
# starts: the Java lanes, the gRPC fuzzer's and the Python ones. Source it:
#
#   source "${GITHUB_WORKSPACE}/.github/workflows/test_workflow_scripts/keploy-start-wait.sh"
#
# keploy starts its agent first and the app only once the agent is ready. On a
# starved runner the agent's start alone can take minutes. A lane that started
# the app's wait when it launched keploy spent the app's budget on the agent's
# start, and failed with "the app did not come up" while keploy was still
# starting. So a lane waits for the agent's ready line first
# (await_agent_ready), and starts the app's wait, with its full budget, from
# there.
#
# keploy removes its log (keploy-logs-*.txt) and the agent's start report
# (boot.json, in a keploy-agent-stop-* directory) when it exits, and both are
# root's. So a lane that gives up on the agent or the app copies them into its
# working directory (save_keploy_start_evidence) before it stops keploy, and
# its workflow uploads that directory.

# The line keploy record prints once its agent is ready, just before it starts
# the app (pkg/service/record/record.go). Every release since v3.0.3 prints it.
KEPLOY_AGENT_READY_LINE='Keploy agent is ready to record test cases and mocks.'

# Where save_keploy_start_evidence copies to, relative to the working
# directory: each lane's workflow uploads this directory.
KEPLOY_START_EVIDENCE_DIR=${KEPLOY_START_EVIDENCE_DIR:-keploy-start-evidence}

# _kse_alive PID: PID is a live process, by its state in /proc (keploy can run
# under sudo, so kill -0 from this user fails on a sudo that is alive). A
# zombie has exited.
_kse_alive() {
    local st
    case $1 in "" | *[!0-9]*) return 1 ;; esac
    { read -r st <"/proc/$1/stat"; } 2>/dev/null || return 1
    st=${st##*) }
    [ "${st%% *}" != Z ]
}

# save_keploy_start_evidence [DIR]: copies into DIR (default
# KEPLOY_START_EVIDENCE_DIR) this runner's keploy logs, each agent's boot.json
# (as boot-<its directory's suffix>.json), the processes running now (which
# step a stalled start sits in) and the java a root shell finds on PATH,
# readable by this user for the upload. The temp directories searched are
# KEPLOY_START_EVIDENCE_ROOTS (default: TMPDIR, then /tmp; a docker agent's
# keploy-agent-boot.json is in /tmp). Never fails: it runs on a failure path.
save_keploy_start_evidence() {
    local dir=${1:-$KEPLOY_START_EVIDENCE_DIR}
    local roots=${KEPLOY_START_EVIDENCE_ROOTS:-${TMPDIR:-/tmp} /tmp}
    if ! mkdir -p "$dir" 2>/dev/null; then
        echo "could not make $dir for keploy's logs and start reports"
        return 0
    fi
    ps -eo pid,ppid,etimes,stat,args --forest >"$dir/ps.txt" 2>&1 || true
    # shellcheck disable=SC2016,SC2086 # $1... are the root shell's; roots split
    sudo -n sh -c '
        dir=$1; shift
        command -v java >"$dir/root-java.txt" 2>&1 || echo "no java on root PATH ($PATH)" >"$dir/root-java.txt"
        for root in "$@"; do
            # A pattern that matches nothing stays as it is: its cp fails,
            # unheard.
            for f in "$root"/keploy-logs-*.txt; do
                cp -f "$f" "$dir/" 2>/dev/null
            done
            for f in "$root"/keploy-agent-stop-*/boot.json; do
                d=${f%/boot.json}
                cp -f "$f" "$dir/boot-${d##*/keploy-agent-stop-}.json" 2>/dev/null
            done
            # A docker agent'"'"'s, written where keploy reads it.
            cp -f "$root/keploy-agent-boot.json" "$dir/" 2>/dev/null
        done
        exit 0
    ' sh "$dir" $roots 2>/dev/null || true
    sudo -n chown -R "$(id -u):$(id -g)" "$dir" 2>/dev/null || true
    chmod -R a+rX "$dir" 2>/dev/null || true
    echo "keploy's logs and start reports are in $dir: $(ls "$dir" 2>/dev/null | tr '\n' ' ')"
    return 0
}

# await_agent_ready LOG [PID | @FILE]: waits until LOG (keploy record's output)
# has KEPLOY_AGENT_READY_LINE. Fails, after saving the evidence
# (save_keploy_start_evidence), when PID (keploy's, if given) exits first, or
# after KEPLOY_AGENT_READY_BOUND seconds (default 600): a start that takes
# longer is stuck, not slow. Says every KEPLOY_AGENT_WAIT_SAY_EVERY seconds
# (default 30) that it still waits, with LOG's last line, which names the
# agent's step (keploy prints the step a slow start is in). An agent ready
# only after KEPLOY_AGENT_READY_WARN_AFTER seconds or more (default 60) is
# said in a ::warning::, so a slower start shows on the run, though the wait
# goes on to its bound. A lane that runs keploy in the foreground, its request
# driver in the background, gives @FILE: keploy writes its PID there as it
# launches (exec_noting_pid), and the wait reads it from FILE once FILE names
# one, waiting as with no PID until then. The lane removes FILE, and empties
# LOG, before it starts the driver, so both are this run's.
await_agent_ready() {
    local log=$1 pid=${2:-} bound=${KEPLOY_AGENT_READY_BOUND:-600}
    local every=${KEPLOY_AGENT_WAIT_SAY_EVERY:-30} warn=${KEPLOY_AGENT_READY_WARN_AFTER:-60}
    local start=$SECONDS next=$every xtrace=0 pidfile='' took
    case $- in *x*) xtrace=1; set +x ;; esac
    case $pid in @*) pidfile=${pid#@} pid='' ;; esac
    echo "Waiting for keploy's agent to be ready (its line in $log, at most ${bound}s)"
    while ! grep -qF -- "$KEPLOY_AGENT_READY_LINE" "$log" 2>/dev/null; do
        if [ -z "$pid" ] && [ -n "$pidfile" ]; then
            { read -r pid <"$pidfile"; } 2>/dev/null || :
        fi
        if [ -n "$pid" ] && ! _kse_alive "$pid"; then
            # The line can come with the exit.
            grep -qF -- "$KEPLOY_AGENT_READY_LINE" "$log" 2>/dev/null && break
            echo "::error::keploy ($pid) exited before its agent was ready. The last lines of $log:"
            tail -n 40 "$log" 2>/dev/null || true
            save_keploy_start_evidence
            [ "$xtrace" = 1 ] && set -x
            return 1
        fi
        if [ $((SECONDS - start)) -ge "$bound" ]; then
            echo "::error::keploy's agent was not ready after $((SECONDS - start))s (the bound is ${bound}s). The last lines of $log:"
            tail -n 40 "$log" 2>/dev/null || true
            save_keploy_start_evidence
            [ "$xtrace" = 1 ] && set -x
            return 1
        fi
        if [ $((SECONDS - start)) -ge "$next" ]; then
            echo "Still waiting for keploy's agent ($((SECONDS - start))s); the last line of $log: $(tail -n 1 "$log" 2>/dev/null | cut -c1-300)"
            next=$((next + every))
        fi
        sleep 1
    done
    took=$((SECONDS - start))
    echo "keploy's agent is ready after ${took}s; the app's wait starts now."
    if [ "$took" -ge "$warn" ]; then
        echo "::warning::keploy's agent was ready only after ${took}s (${warn}s or more; the bound is ${bound}s): this runner is slow, or keploy's start is slower. $log names the step each wait was in."
    fi
    [ "$xtrace" = 1 ] && set -x
    return 0
}

# exec_noting_pid FILE CMD [ARG...]: writes this shell's PID to FILE, then runs
# CMD in its place (exec), so FILE names CMD's process while it runs. For a
# lane that runs keploy in the foreground, piped to tee, and its request
# driver in the background:
#
#   rm -f "$app.pid"; send_request &    # which runs: await_agent_ready "$app.txt" "@$app.pid"
#   exec_noting_pid "$app.pid" "$RECORD_BIN" record ... 2>&1 | tee "$app.txt"
#
# FILE is written whole (renamed into place), so the wait never reads half a
# PID. Run it only as a pipeline's element, or in another subshell: in the
# lane's own shell it would replace that shell, so there it refuses, and
# fails. A FILE it cannot write is said, and CMD still runs: its driver then
# waits as with no PID.
exec_noting_pid() {
    local file=$1
    shift
    if [ "$BASHPID" = "$$" ]; then
        echo "::error::exec_noting_pid runs only in a subshell (a pipeline's element): here it would replace the lane's shell"
        return 1
    fi
    if ! { echo "$BASHPID" >"$file.$BASHPID" && mv -f "$file.$BASHPID" "$file"; } 2>/dev/null; then
        echo "could not write keploy's PID to $file: its driver waits for the ready line as with no PID"
    fi
    exec "$@"
}
