# shellcheck shell=bash
# Sourced by the mock lanes: which half of a lane this run runs.
#   MOCK_LANE_SWITCH=off   only its runs with the proven switch off
#                          (KEPLOY_PROVEN=0: the default on Windows and macOS)
#   MOCK_LANE_SWITCH=on    only its runs under KEPLOY_PROVEN=1 (keploy enterprise's
#                          default on Linux)
#   unset, or both         both, as a run on a developer's machine does
# CI runs each half in a job of its own (mock_linux.yml), so how long one half
# takes never decides whether the other finishes.
#
# Every keploy a lane runs states its switch. The runs of the on half say
# KEPLOY_PROVEN=1 themselves; every other run is the off half's, and runs with
# the switch off because this file exports KEPLOY_PROVEN=0: with it unset a
# Linux keploy of a build that runs release 1's flow (keploy enterprise) runs
# the proven model, which would turn the off half into a second on half.
export KEPLOY_PROVEN=0
LANE_SWITCH="${MOCK_LANE_SWITCH:-both}"
case "$LANE_SWITCH" in
  off|on|both) ;;
  *)
    echo "FAIL: MOCK_LANE_SWITCH=$LANE_SWITCH: want off, on or both"
    exit 1
    ;;
esac

# half <off|on>: this run runs that half.
half() {
  [ "$LANE_SWITCH" = both ] || [ "$LANE_SWITCH" = "$1" ]
}

# SWITCH_VALUES: the KEPLOY_PROVEN values this run runs a both-ways check
# with: 0 (off) and 1 (on).
SWITCH_VALUES=()
if half off; then SWITCH_VALUES+=(0); fi
if half on; then SWITCH_VALUES+=(1); fi

# Loaded: a lane checks this status and that half is defined.
true
