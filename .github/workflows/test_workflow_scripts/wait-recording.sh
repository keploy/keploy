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
    local quiet=0 size=-1 now xtrace=0
    # One trace line a second would drown a lane run with `set -x`.
    case $- in *x*) xtrace=1; set +x ;; esac
    while kill -0 "$pid" 2>/dev/null; do
        now=$( [ -n "$log" ] && stat -c %s "$log" 2>/dev/null || echo -1)
        if [ "$now" != "$size" ]; then
            size=$now
            quiet=0
        fi
        if [ "$quiet" -ge "$bound" ]; then
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
        quiet=$((quiet + 1))
    done
    wait "$pid" 2>/dev/null || true
    [ "$xtrace" = 1 ] && set -x
    return 0
}

# _recording_quit_writers sends SIGQUIT (a Go process dumps every goroutine to
# its stderr on it) to the processes whose stdout or stderr is the pipe tee
# (PID $1) reads, and lists them: the recording's own processes, never another
# job's.
_recording_quit_writers() {
    local pipe
    pipe=$(readlink "/proc/$1/fd/0" 2>/dev/null) || return 0
    case $pipe in pipe:*) ;; *) return 0 ;; esac
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
