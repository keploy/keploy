#!/bin/bash
# Shared by the lanes that background `keploy record ... | tee LOG &` and stop
# it with a signal. Source it, and right after starting the recording:
#
#     rec_pid=$!                   # tee's PID
#     rec_keploy=$(jobs -p %%)     # the pipeline's first process: keploy
#
# then signal rec_keploy (not a keploy found by name: on a shared runner that
# can be another job's), and wait:
#
#     wait_recording_end "$rec_pid" LOG
#
# (A lane that needs keploy record's own exit status joins keploy to its tee
# through a FIFO instead, both its own jobs: see
# golang/gin_mongo_after_client_close/golang-linux.sh.)
#
# A recording that is stopped drains what the agent captured before the stop,
# and takes as long as that does: on a loaded runner, longer than any fixed
# sleep. A lane that slept a fixed time instead started its next step while
# the last recording still held the app's port and the agent's, and checked
# the log before keploy had written its last lines to it. tee ends once every
# process writing to the pipe has exited -- keploy, its native agent and the
# app it started -- so waiting for tee is waiting for the recording to be
# over, and for its log to be complete.
#
# The bound (seconds, default 300; KEPLOY_RECORD_STOP_BOUND overrides it) is
# on silence, not on the stop: it restarts whenever LOG grows, and a stop that
# is still saving says so every few seconds. Past it the recording is hung:
# the lane dumps the goroutines of the processes writing to the pipe (and only
# those) and fails, rather than sitting silent until the job's timeout.
wait_recording_end() {
    local pid=$1 log=${2:-} bound=${3:-${KEPLOY_RECORD_STOP_BOUND:-300}}
    local last=$SECONDS size=-1 now xtrace=0
    # One trace line a second would drown a lane run with `set -x`.
    case $- in *x*) xtrace=1; set +x ;; esac
    while kill -0 "$pid" 2>/dev/null; do
        now=$( [ -n "$log" ] && stat -c %s "$log" 2>/dev/null || echo -1)
        if [ "$now" != "$size" ]; then
            size=$now
            last=$SECONDS
        fi
        # Seconds since LOG last grew, by the clock: a pass of this loop
        # takes longer than its sleep on a loaded runner.
        if [ $((SECONDS - last)) -ge "$bound" ]; then
            echo "::error::the recording said nothing for ${bound}s after it was stopped, and has not ended: keploy, its agent or the app it started is hung. The goroutines of the processes writing to its log follow."
            _recording_quit_writers "$pid"
            # Give the dumps time to reach the log through tee: they end the
            # processes, and with them tee.
            local w=0
            while kill -0 "$pid" 2>/dev/null && [ "$w" -lt 30 ]; do
                sleep 1
                w=$((w + 1))
            done
            [ "$xtrace" = 1 ] && set -x
            return 1
        fi
        sleep 1
    done
    wait "$pid" 2>/dev/null || true
    [ "$xtrace" = 1 ] && set -x
    return 0
}

# _recording_quit_writers sends SIGQUIT (a Go process dumps every goroutine to
# its stderr on it) to the processes whose stdout or stderr is the pipe tee
# (PID $1) reads, or the FIFO it reads (a lane that joins keploy to its tee
# through one), and lists them: the recording's own processes, never another
# job's.
_recording_quit_writers() {
    local pipe
    pipe=$(readlink "/proc/$1/fd/0" 2>/dev/null) || return 0
    case $pipe in
    pipe:*) ;;
    /*) [ -p "$pipe" ] || return 0 ;;
    *) return 0 ;;
    esac
    sudo bash -c '
        for fd in /proc/[0-9]*/fd/1 /proc/[0-9]*/fd/2; do
            [ "$(readlink "$fd" 2>/dev/null)" = "$1" ] || continue
            p=${fd#/proc/}; p=${p%%/*}
            [ "$p" = "$2" ] && continue
            echo "$p"
        done | sort -u | while read -r p; do
            ps -o pid,ppid,stat,etime,args -p "$p" | tail -n +2
            kill -QUIT "$p" 2>/dev/null || true
        done
    ' _ "$pipe" "$1" || true
}

# await_ready NAME PIDS LOG BAD PROBE...: wait until PROBE (a command) succeeds,
# for as long as what has to make it succeed is alive and working:
#   - PIDS (one or more, space-separated): the processes it needs, such as a
#     recording's tee (rec_pid above) or an app the lane started itself. One
#     that exits first fails the wait at once, with LOG's last lines.
#   - LOG: what keploy (or that app) writes as it works. If BAD (a command run
#     as `BAD LOG`; - for none) finds a line there the lane fails on, such as
#     an ERROR, the wait fails at once with it.
#   - The bound (seconds, default 300; KEPLOY_READY_SILENCE_BOUND overrides
#     it) is on silence, as wait_recording_end's is: it restarts whenever LOG
#     grows. So LOG must be what the work writes, not what PROBE's own
#     traffic makes the app write, or a probe that keeps failing keeps it
#     going: probe for what the work brings about (a port that opens), and
#     judge an answer once. Past it, with PROBE still failing, the wait is hung: for each of
#     PIDS that is a recording's tee, the goroutines of the processes writing
#     to it are dumped (_recording_quit_writers), and the wait fails.
# No count of tries or seconds decides. A loaded runner's agent can spend a
# minute loading its eBPF programs, saying so as it goes ("kernel call is
# taking unusually long"). The app starts only after that, and a lane that
# gave up after ten tries three seconds apart failed with "Application failed
# to start" while keploy was still on its way.
await_ready() {
    local name=$1 pids=$2 log=$3 bad=$4
    shift 4
    local bound=${KEPLOY_READY_SILENCE_BOUND:-300} last=$SECONDS size=-1 now p xtrace=0
    case $- in *x*) xtrace=1; set +x ;; esac
    while ! "$@" >/dev/null 2>&1; do
        for p in $pids; do
            if ! kill -0 "$p" 2>/dev/null; then
                echo "::error::$name: process $p exited before it was ready. The last lines of $log:"
                tail -n 40 "$log" 2>/dev/null || true
                [ "$xtrace" = 1 ] && set -x
                return 1
            fi
        done
        if [ "$bad" != - ] && "$bad" "$log" >/dev/null 2>&1; then
            echo "::error::$name: $log shows a failure before it was ready:"
            "$bad" "$log" || true
            [ "$xtrace" = 1 ] && set -x
            return 1
        fi
        now=$(stat -c %s "$log" 2>/dev/null || echo -1)
        if [ "$now" != "$size" ]; then
            size=$now
            last=$SECONDS
        fi
        # By the clock, as in wait_recording_end: one pass can take as long
        # as PROBE does.
        if [ $((SECONDS - last)) -ge "$bound" ]; then
            echo "::error::$name: not ready, and $log said nothing for ${bound}s: hung. The goroutines of a recording's processes follow."
            for p in $pids; do
                # Only a tee's: another process's stdin can be any pipe.
                [ "$(cat "/proc/$p/comm" 2>/dev/null)" = tee ] && _recording_quit_writers "$p"
            done
            tail -n 40 "$log" 2>/dev/null || true
            [ "$xtrace" = 1 ] && set -x
            return 1
        fi
        sleep 1
    done
    echo "$name: ready."
    [ "$xtrace" = 1 ] && set -x
    return 0
}

# log_captured LOG SLUG[:N]...: LOG (a recording's) says keploy captured at
# least N (default 1) test cases named SLUG-<counter> for each SLUG, as its
# "Keploy has captured test cases" line names them: get-health-1,
# grpc-userservice-getusers-2. With await_ready, a lane stops a recording once
# keploy has written what it drove, not after a fixed sleep.
# log_missing LOG SLUG[:N]...: prints each SLUG[:N] log_captured would still
# wait for.
log_missing() {
    local log=$1 want
    shift
    for want in "$@"; do log_captured "$log" "$want" || printf '%s ' "$want"; done
}
log_captured() {
    local log=$1 want slug n
    shift
    for want in "$@"; do
        slug=${want%:*} n=1
        [ "$slug" = "$want" ] || n=${want##*:}
        [ "$(grep -o -E "Keploy has captured test cases[^{]*\{[^}]*\"testcase name\": \"${slug}-[0-9]+\"" "$log" 2>/dev/null | wc -l)" -ge "$n" ] || return 1
    done
}
