# Sourced by the mock lanes: which half of a lane this run runs.
#   MOCK_LANE_SWITCH=off   only its runs without the proven switch, today's default
#   MOCK_LANE_SWITCH=on    only its runs under KEPLOY_PROVEN=1, the flip's default
#   unset, or both         both, as a run on a developer's machine does
# CI runs each half in a job of its own (mock_linux.yml), so how long one half
# takes never decides whether the other finishes.
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
# with: "" (off) and 1 (on).
SWITCH_VALUES=()
if half off; then SWITCH_VALUES+=(""); fi
if half on; then SWITCH_VALUES+=(1); fi
