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
#       signal ended it, as `wait` gives it), stops its watchdog, and sets
#       HANG_FIRED to 1 when the watchdog signalled the run, else 0.
#
# The watchdog beside the run sleeps SECONDS and, if the run still runs, says
# so on stderr (the run's stderr, so its log), in a line that starts with
# HANG_LINE (-q: says nothing, for a caller that reports it itself), and sends
# SIG (TERM unless given) to the run's process group, as timeout(1) does; with
# -k, KILL that many seconds later if it still runs. That the guard ended a
# run is told by HANG_LINE or HANG_FIRED, not by an exit code. CMD runs in a process group of its own, with the caller's
# stdin, and with SIGINT and SIGQUIT as the caller has them (bash ignores both
# in a background job of a script unless job control is on).
# The watchdog's sleep is the system's (command -p), not one on the caller's
# PATH: a lane run against fakes puts a sleep there that returns at once.

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

# hang_signal SIG PGID: sends SIG to the process group PGID. A run under
# sudo needs nothing more: sudo keeps this user's real uid while its command
# runs, so this user may signal it, and it relays the signal to its command
# (the row "a run under sudo", on sudo 1.9 and sudo-rs).
hang_signal() {
  kill -s "$1" -- "-$2" 2>/dev/null
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
  # and SIGQUIT as the caller has them.
  # Neither child runs the caller's EXIT trap (bash's subshells would, the
  # watchdog's when it is stopped). An external command is exec'd, so
  # HANG_PID is its pid and a signal sent there reaches it.
  set -m
  {
    trap - EXIT
    if [ "$(type -t "$1")" = file ]; then exec "$@"; fi
    "$@"
  } <&0 &
  HANG_PID=$!
  (
    trap - EXIT
    run=$HANG_PID
    command -p sleep "$secs"
    hang_running "$run" || exit 1
    # From here it has fired: the TERM hang_wait sends once the run ended
    # (in the -k sleep, or before the signal below returns) still ends it
    # with 0. Its sleep, a child, takes TERM as it is and ends at once.
    trap 'exit 0' TERM
    [ -z "$say" ] || echo "$HANG_LINE still running after ${secs}s, sending SIG$sig: $*" >&2
    hang_signal "$sig" "$run"
    if [ -n "$kill_after" ]; then
      command -p sleep "$kill_after"
      if hang_running "$run"; then
        [ -z "$say" ] || echo "$HANG_LINE still running ${kill_after}s after SIG$sig, sending SIGKILL: $*" >&2
        hang_signal KILL "$run"
      fi
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
    # Its group: the watchdog and the sleep it waits on. The watchdog exits 0
    # once it has signalled the run (TERMed after that too), 1 when it found
    # the run ended; TERMed while it waits out SECONDS, 143.
    kill -s TERM -- "-$wd" 2>/dev/null
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
