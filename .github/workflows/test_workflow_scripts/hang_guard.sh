# shellcheck shell=bash
# A hang guard for a run whose exit code a lane checks, the same on GNU and
# uutils coreutils. Sourced by such a lane, or run as a command by bash
# (`bash hang_guard.sh [-s SIG] [-k SECONDS] SECONDS CMD...`, inside a
# container, say), when nothing signals the run but the guard.
#
# Such a run's exit code is read by `wait` on its pid, never taken from
# timeout(1): Ubuntu 26.04's timeout (uutils coreutils 0.10.0) does not end as
# GNU's (9.4) does. Sent SIGINT, SIGTERM or SIGHUP, it relays it and then exits
# 128 + the signal even when its command exits 0 (GNU: the command's code),
# with --preserve-status or --foreground too; and when its own time limit
# sends KILL it exits 124 (GNU: 137). So a lane that stops keploy with an
# interrupt read 130 where keploy exited 0.
#
#   bounded [-q] [-s SIG] [-k SECONDS] SECONDS CMD...
#       runs CMD and returns its exit status.
#   hang_start [-q] [-s SIG] [-k SECONDS] SECONDS CMD...
#       starts CMD in the background and sets HANG_PID to its pid: signal it
#       there (sudo relays what it is sent to its command), then
#   hang_wait PID
#       waits for it and returns its exit status (128 + the signal when a
#       signal ended it, as `wait` gives it), waits for its watchdog, and sets
#       HANG_FIRED to 1 when the watchdog signalled the run, else 0.
#
# The watchdog beside the run waits SECONDS (whole seconds) and, if the run
# still runs, says so on stderr (the run's stderr, so its log), in a line that
# starts with HANG_LINE (-q: says nothing, for a caller that reports it
# itself), and sends SIG (TERM unless given) to the run's process group, as
# timeout(1) does, root's command under sudo included (hang_signal); with
# -k, KILL that many seconds later if it still runs.
# That the guard ended a run is told by HANG_LINE or HANG_FIRED, not by an
# exit code. CMD runs in a process group of its own, with the caller's stdin,
# and with SIGINT and SIGQUIT as the caller has them (bash ignores both in a
# background job of a script unless job control is on).
#
# Nothing signals the watchdog: it looks at the run every fifth of a second
# and ends by itself, at most that long after the run ends. A child bash has
# just forked still has its parent's signal handlers until it sets up its own,
# and once a script sets an EXIT trap bash catches TERM, INT, HUP and the
# other ending signals to run it: such a signal landing in that moment runs
# the caller's EXIT trap in the child (a lane's cleanup, mid-run), as TERMing
# the watchdog of a run that ended at once did. The run itself is signalled
# by the guard only once SECONDS have passed, and by the caller once it is
# ready. The watchdog's sleep is the system's (command -p), not one on the
# caller's PATH: a lane run against fakes puts a sleep there that returns at
# once.

HANG_LINE="hang guard:"
declare -gA HANG_WATCHDOG=()

# hang_running PID: whether PID still runs, by its state in /proc, which shows
# root's processes to this user too: gone, or a zombie, is not running.
hang_running() {
  local st
  case $1 in "" | *[!0-9]*) return 1 ;; esac
  { read -r st <"/proc/$1/stat"; } 2>/dev/null || return 1
  st=${st##*) }
  [ "${st%% *}" != Z ]
}

# hang_until PID SECONDS: 0 when PID still runs SECONDS from now, 1 as soon
# as it does not (looked at every fifth of a second).
hang_until() {
  local end=$((${EPOCHREALTIME//[!0-9]/} + $2 * 1000000))
  while hang_running "$1"; do
    ((${EPOCHREALTIME//[!0-9]/} < end)) || return 0
    command -p sleep 0.2
  done
  return 1
}

# hang_foreign PGID: 0 when a member of the process group PGID has a real
# uid that is not this user's (root's command under sudo), by /proc; never
# for root, who may signal any. A process of this user's real uid is one
# this user may signal.
hang_foreign() {
  local d st pg k ruid
  [ "$EUID" != 0 ] || return 1
  for d in /proc/[0-9]*; do
    # one gone since the glob decides nothing: neither file reads
    { read -r st <"$d/stat"; } 2>/dev/null || continue
    st=${st##*) }
    read -r _ _ pg _ <<<"$st"
    [ "$pg" = "$1" ] || continue
    while read -r k ruid _; do
      [ "$k" = Uid: ] || continue
      [ "$ruid" = "$EUID" ] || return 0
    done 2>/dev/null <"$d/status"
  done
  return 1
}

# hang_signal SIG PGID: sends SIG to the process group PGID, root's command
# under sudo included. This user may signal sudo, which keeps this user's
# real uid while its command runs and relays what it is sent to its
# command, but not KILL: KILL ends sudo alone and leaves root's command
# running under pid 1. So while a member of the group is another user's,
# SIG goes through `sudo -n kill` too (a runner has password-free sudo); if
# that fails, the guard says root's command may still run (the rows "a run
# under sudo", on sudo 1.9 and sudo-rs). A command that sudo runs in a
# terminal of its own (use_pty, when sudo has one) is in another session,
# and so not in the group: no lane runs one under the guard.
hang_signal() {
  kill -s "$1" -- "-$2" 2>/dev/null
  hang_foreign "$2" || return 0
  sudo -n kill -s "$1" -- "-$2" 2>/dev/null ||
    echo "$HANG_LINE could not send SIG$1 to another user's process in the run's group (sudo -n kill failed): it may still run" >&2
}

hang_start() {
  local sig=TERM kill_after="" secs had_m="" say=1
  while :; do
    case $1 in
      -q) say=""; shift ;;
      -s) sig=$2; shift 2 ;;
      -k) kill_after=$2; shift 2 ;;
      *) break ;;
    esac
  done
  secs=$1
  shift
  case $- in *m*) had_m=1 ;; esac
  # Job control gives the run a process group of its own and keeps SIGINT
  # and SIGQUIT as the caller has them. An external command is exec'd, so
  # HANG_PID is its pid and a signal sent there reaches it.
  set -m
  {
    if [ "$(type -t "$1")" = file ]; then exec "$@"; fi
    "$@"
  } <&0 &
  HANG_PID=$!
  # The watchdog ends 0 once it has signalled the run, 1 when the run ended
  # first.
  (
    run=$HANG_PID
    hang_until "$run" "$secs" || exit 1
    [ -z "$say" ] || echo "$HANG_LINE still running after ${secs}s, sending SIG$sig: $*" >&2
    hang_signal "$sig" "$run"
    if [ -n "$kill_after" ] && hang_until "$run" "$kill_after"; then
      [ -z "$say" ] || echo "$HANG_LINE still running ${kill_after}s after SIG$sig, sending SIGKILL: $*" >&2
      hang_signal KILL "$run"
    fi
    exit 0
  ) </dev/null &
  HANG_WATCHDOG[$HANG_PID]=$!
  [ -n "$had_m" ] || set +m
}

hang_wait() {
  local pid=$1 rc wd=${HANG_WATCHDOG[$1]:-}
  wait "$pid"
  rc=$?
  HANG_FIRED=0
  if [ -n "$wd" ]; then
    if wait "$wd" 2>/dev/null; then HANG_FIRED=1; fi
    unset "HANG_WATCHDOG[$pid]"
  fi
  return "$rc"
}

bounded() {
  hang_start "$@"
  hang_wait "$HANG_PID"
}

# Run as a command: bound the command it is given and exit with its code.
if [ "${BASH_SOURCE[0]}" = "$0" ]; then
  bounded "$@"
  exit
fi
