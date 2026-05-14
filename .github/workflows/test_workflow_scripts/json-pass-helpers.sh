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
# binaries advertise --storage-format. The released keploy binary used
# in the compat-matrix cells does not yet ship that flag, so those
# cells skip the json pass automatically.
#
# Honours both naming conventions for binary env vars, and falls back to
# `keploy` on PATH when neither is set (for scripts migrated off the
# RECORD_BIN/REPLAY_BIN convention):
#   - RECORD_BIN / REPLAY_BIN      (most test scripts)
#   - RECORD_KEPLOY_BIN / REPLAY_KEPLOY_BIN  (fuzzer scripts)
json_pass_supported() {
    local _rec="${RECORD_BIN:-${RECORD_KEPLOY_BIN:-keploy}}"
    local _rep="${REPLAY_BIN:-${REPLAY_KEPLOY_BIN:-keploy}}"
    "$_rec" --help 2>&1 | grep -q -- '--storage-format' || return 1
    "$_rep" --help 2>&1 | grep -q -- '--storage-format' || return 1
    return 0
}

# json_scan_reports: scans every test-set-*-report.json file under
# ./keploy/reports/test-run-*/ and verifies status=PASSED. Requires
# jq (preinstalled on ubuntu runners).
#
# Returns 0 if every report passed; 1 if any are missing or non-PASSED.
# Echoes per-report status for the CI log.
json_scan_reports() {
    # Use find for portability; shopt/setopt nullglob differ across bash/zsh.
    local rc=0
    local found=false
    local f s
    while IFS= read -r f; do
        [ -z "$f" ] && continue
        found=true
        s=$(jq -r '.status' "$f" 2>/dev/null || echo "")
        echo "json report $(basename "$f"): ${s:-<unreadable>}"
        if [ "$s" != "PASSED" ]; then
            echo "::error::$(basename "$f") (json) status=${s:-<unreadable>}"
            rc=1
        fi
    done < <(find ./keploy/reports -type f -path '*/test-run-*/test-set-*-report.json' 2>/dev/null)
    if [ "$found" != "true" ]; then
        echo "::error::No json test-set reports found under ./keploy/reports/"
        return 1
    fi
    return $rc
}
