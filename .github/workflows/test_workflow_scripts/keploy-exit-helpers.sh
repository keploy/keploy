# shellcheck shell=bash
# Helpers for the golang_docker.yml lane scripts (gin_mongo and echo_sql
# golang-docker.sh), which their workflow step runs under `bash -e`. Sourced
# via:
#
#   source "${GITHUB_WORKSPACE}/.github/workflows/test_workflow_scripts/keploy-exit-helpers.sh"
#
# Under `bash -e` a keploy command that exited non-zero ended the step at that
# line, before the script's checks below it could print its log: the step
# failed with no line saying what had failed. A script takes each keploy
# command's exit status instead (`|| rc=$?`), and keploy_exited names a
# non-zero one, with the test-set reports of the test run it wrote and the
# command's log.

# test_run_count: the number of test runs keploy test has written under
# ./keploy/reports. ls exits non-zero when there is none yet; `|| true` keeps
# that from ending a step that sets pipefail.
test_run_count() {
    { ls -d ./keploy/reports/test-run-* 2>/dev/null || true; } | wc -l
}

# keploy_exited RC WHAT LOG [RUNS_BEFORE]: prints an ::error:: line naming
# WHAT and RC, the reports of the test run WHAT wrote, and LOG, then exits RC.
# RUNS_BEFORE is test_run_count taken before a keploy test. Only a test run
# past that count, the one the command wrote, has its reports printed, so a
# replay that wrote no test run prints no earlier run's reports. keploy
# record, which runs before any keploy test and writes no test run, gives
# none.
keploy_exited() {
    local rc=$1 what=$2 log=$3 runs_before=${4:-0} run f
    echo "::error::${what} exited ${rc}; the test-set reports and its log (${log}) follow"
    if [ "$(test_run_count)" -gt "$runs_before" ]; then
        run="$(ls -td ./keploy/reports/test-run-* 2>/dev/null | head -n 1 || true)"
        for f in "$run"/test-set-*-report.yaml "$run"/test-set-*-report.json; do
            [ -f "$f" ] || continue
            case "$f" in
            *.json)
                echo "$f: $(jq -r '.status' "$f" 2>/dev/null || echo '<unreadable>')"
                jq -r '.tests[]? | select(.status == "FAILED") | "  failed: " + .testCaseID' "$f" 2>/dev/null || true
                ;;
            *)
                echo "$f: $(grep -m 1 'status:' "$f" | awk '{print $2}')"
                awk '/^ +status: FAILED/ {f = 1} f && /test_case_id:/ {print "  failed: " $2; f = 0}' "$f"
                ;;
            esac
        done
    fi
    cat "$log"
    exit "$rc"
}
