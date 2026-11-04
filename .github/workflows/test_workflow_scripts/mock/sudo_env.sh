# shellcheck shell=bash
# Sourced by the mock lanes that run keploy through sudo.
#
# sudo hands the command it runs only the variables its command line names:
# sudo-rs (Ubuntu 26.04's sudo) ignores -E, so no lane uses it. SUDO_ENV is,
# as NAME=value for `sudo env`, what every such run names besides its own
# variables: the switches a workflow sets for every keploy run, each only
# while it is set. KEPLOY_EXACT_VIEW_VERIFY=1 runs the agent's exact view in
# verify mode under the proven switch; keploy reads it from its own
# environment (provenswitch.AgentArgs).
SUDO_ENV=()
if [ -n "${KEPLOY_EXACT_VIEW_VERIFY+x}" ]; then
  SUDO_ENV+=("KEPLOY_EXACT_VIEW_VERIFY=$KEPLOY_EXACT_VIEW_VERIFY")
fi

# VERIFY_LINE: what an agent prints when it runs its exact view in verify
# mode (cli/provider/cmd.go).
VERIFY_LINE="the exact view's verify mode is on in this agent"

# verify_ran PROVEN LOG: a run under the proven switch (PROVEN is 1) while
# KEPLOY_EXACT_VIEW_VERIFY=1 has its agent's verify line in LOG. Called after
# each such run whose agent started: a sudo line that drops the variable then
# fails its lane instead of passing without verify mode. Says why and
# returns 1 when the line is missing.
verify_ran() {
  [ "$1" = 1 ] && [ "${KEPLOY_EXACT_VIEW_VERIFY:-}" = 1 ] || return 0
  grep -aqF "$VERIFY_LINE" "$2" 2>/dev/null && return 0
  echo "FAIL: $2: keploy ran under the proven switch with KEPLOY_EXACT_VIEW_VERIFY=1, and its agent never said its exact view's verify mode is on: the variable did not reach keploy"
  return 1
}

# Loaded: a lane checks this status and that verify_ran is defined.
true
