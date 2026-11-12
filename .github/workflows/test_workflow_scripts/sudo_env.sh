# shellcheck shell=bash
# Sourced by every CI script that runs keploy through sudo.
#
# sudo hands the command it runs only the variables its command line names:
# sudo-rs (Ubuntu 26.04's sudo) ignores -E, so no script uses it. SUDO_ENV is,
# as NAME=value for `sudo env`, what every such run names besides its own
# variables: the switches a workflow sets for keploy (SUDO_SWITCHES), each only
# while it is set, an empty value included.
# - KEPLOY_EXACT_VIEW_VERIFY=1 runs the agent's exact view in verify mode under
#   the proven switch; keploy reads it from its own environment
#   (provenswitch.AgentArgs).
# - KEPLOY_PG_V3_LOG_BIND_VALUES=1 puts the raw bind values in the Postgres
#   replayer's miss lines.
# A workflow that sets another KEPLOY_ variable in an env: block adds it here,
# or to the names keploy does not read in sudo_env.tests.sh, whose case for
# that fails until it is in one of them.
SUDO_SWITCHES=(KEPLOY_EXACT_VIEW_VERIFY KEPLOY_PG_V3_LOG_BIND_VALUES)
SUDO_ENV=()
for _sudo_switch in "${SUDO_SWITCHES[@]}"; do
  if [ -n "${!_sudo_switch+x}" ]; then
    SUDO_ENV+=("$_sudo_switch=${!_sudo_switch}")
  fi
done
unset _sudo_switch

# VERIFY_LINE: what an agent prints when it runs its exact view in verify
# mode (cli/provider/cmd.go).
VERIFY_LINE="the exact view's verify mode is on in this agent"

# verify_ran PROVEN LOG [AGENT_LOG]: a run under the proven switch (PROVEN is
# 1) while KEPLOY_EXACT_VIEW_VERIFY=1 has its agent's verify line in LOG.
# Called after each such run whose agent started: a sudo line that drops the
# variable then fails its lane instead of passing without verify mode. Says
# why and returns 1 when the line is missing. LOG is what the keploy command
# printed: an agent in docker prints there too, through docker's attach
# stream (a `docker run` app) or compose's (a compose app, after the agent's
# service name), so one check serves a native and a docker agent alike.
#
# AGENT_LOG is for a compose app whose command attaches to its own service
# alone (`docker compose up app`): compose then prints none of the agent's
# lines, and the agent writes them to its debug file in the keploy folder
# (keploy/agent-debug.log, KEPLOY_DEBUG_FILE), which each agent empties as it
# starts, and creates readable by all (0644). The line passes there too. verify_ran removes AGENT_LOG once it has
# read it, so a later run's check reads only what that run's own agent wrote,
# and fails when it cannot remove it.
verify_ran() {
  [ "$1" = 1 ] && [ "${KEPLOY_EXACT_VIEW_VERIFY:-}" = 1 ] || return 0
  local found=
  grep -aqF "$VERIFY_LINE" "$2" 2>/dev/null && found=1
  if [ -n "${3:-}" ]; then
    grep -aqF "$VERIFY_LINE" "$3" 2>/dev/null && found=1
    # The keploy folder is root's: sudo removes what this user cannot.
    rm -f "$3" 2>/dev/null || sudo -n rm -f "$3" 2>/dev/null
    if [ -e "$3" ]; then
      echo "FAIL: $3: could not remove the agent's debug log once read, so a later run's check could read this run's line"
      return 1
    fi
  fi
  [ -n "$found" ] && return 0
  echo "FAIL: $2${3:+ (and $3)}: keploy ran under the proven switch with KEPLOY_EXACT_VIEW_VERIFY=1, and its agent never said its exact view's verify mode is on: the variable did not reach keploy"
  return 1
}

# proven_of [NAME=value...]: the value `env NAME=value...` gives
# KEPLOY_PROVEN, the last one named; empty when none is. For a run whose
# switch comes in its caller's arguments: verify_ran "$(proven_of "$@")" LOG.
proven_of() {
  local a v=
  for a in "$@"; do
    case $a in KEPLOY_PROVEN=*) v=${a#KEPLOY_PROVEN=} ;; esac
  done
  echo "$v"
}

# Loaded: a lane checks this status and that verify_ran is defined.
true
