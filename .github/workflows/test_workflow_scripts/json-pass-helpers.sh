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
    # Capture first, match second — deliberately NOT a pipeline.
    #
    # This probe failing SILENTLY is the bad outcome: json_pass_supported
    # returns 1, the caller prints "json pass skipped for compat-matrix cell",
    # and the entire JSON record+replay pass vanishes with the lane still green.
    # Under `set -o pipefail` — which 18 of this helper's 31 callers set — a
    # pipeline does exactly that: pipefail promotes the left side's status, so a
    # keploy that prints --storage-format while exiting non-zero fails the probe.
    # (`grep -q` would add a second, INTERMITTENT route: it exits at the first
    # match and SIGPIPEs a writer that has not finished. Measured here that route
    # is LIVE — keploy emits its --help in ~500 separate write()s, none as large
    # as 1KB, and the first --storage-format lands ~93% of the way through the
    # merged stream, so grep can exit with ~700 bytes still unwritten. Measured
    # rc=141 on roughly 1-8% of trials on a quiet machine (N=1000 per binary,
    # two binaries, across several runs) and 16%+ under CPU contention — a
    # scheduling race whose rate tracks host load, so treat any single figure
    # here as conditional on the runner, not as a property of the command.
    # Total output fitting the 64KiB pipe buffer does NOT save an incremental
    # writer; only the writer finishing before the reader exits does, and that
    # is a race. `grep -c` reads to EOF, so the form replaced
    # here never took that route (60/60 rc=0) — but it stayed exposed to the
    # pipefail promotion above, which is why this is a capture, not a -c/-q swap.)
    # Command substitution takes only the binary's status, and `|| true`
    # discards it on purpose — the question is "does the help text mention the
    # flag", never "did --help succeed".
    local _help
    _help="$("$_rec" --help 2>&1)" || true
    case "$_help" in *--storage-format*) ;; *) return 1 ;; esac
    _help="$("$_rep" --help 2>&1)" || true
    case "$_help" in *--storage-format*) ;; *) return 1 ;; esac
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
