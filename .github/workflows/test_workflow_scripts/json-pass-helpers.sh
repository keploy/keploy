# Helpers for the supplemental --storage-format json record/replay
# pass that the language-specific test scripts run after their
# default-format flow. Sourced via:
#
#   source "${GITHUB_WORKSPACE}/.github/workflows/test_workflow_scripts/json-pass-helpers.sh"
#
# The helpers honour the RECORD_BIN / REPLAY_BIN env vars set by
# .github/actions/download-binary, so they work transparently for both
# native-linux and docker scripts.

# json_pass_supported: returns 0 (true) when both record and replay
# binaries come from this branch's build (paths ending in build/keploy
# or build-no-race/keploy). The released keploy binary used in the
# compat-matrix cells does not yet ship --storage-format, so those
# cells must skip the json pass.
#
# Honours both naming conventions for binary env vars:
#   - RECORD_BIN / REPLAY_BIN      (most test scripts)
#   - RECORD_KEPLOY_BIN / REPLAY_KEPLOY_BIN  (fuzzer scripts)
json_pass_supported() {
    local _rec="${RECORD_BIN:-${RECORD_KEPLOY_BIN:-}}"
    local _rep="${REPLAY_BIN:-${REPLAY_KEPLOY_BIN:-}}"
    case "$_rec" in
        */build/keploy|*/build-no-race/keploy) ;;
        *) return 1 ;;
    esac
    case "$_rep" in
        */build/keploy|*/build-no-race/keploy) ;;
        *) return 1 ;;
    esac
    return 0
}

# json_scan_reports: scans every test-set-*-report.json file under
# ./keploy/reports/test-run-*/ and verifies status=PASSED. Requires
# jq (preinstalled on ubuntu runners).
#
# Returns 0 if every report passed; 1 if any are missing or non-PASSED.
# Echoes per-report status for the CI log.
json_scan_reports() {
    shopt -s nullglob
    local reports=( ./keploy/reports/test-run-*/test-set-*-report.json )
    shopt -u nullglob
    if [ ${#reports[@]} -eq 0 ]; then
        echo "::error::No json test-set reports found under ./keploy/reports/"
        return 1
    fi
    local rc=0
    local f s
    for f in "${reports[@]}"; do
        s=$(jq -r '.status' "$f" 2>/dev/null || echo "")
        echo "json report $(basename "$f"): ${s:-<unreadable>}"
        if [ "$s" != "PASSED" ]; then
            echo "::error::$(basename "$f") (json) status=${s:-<unreadable>}"
            rc=1
        fi
    done
    return $rc
}
