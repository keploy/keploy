#!/usr/bin/env bash
# Tests for echo_sql/golang-docker.sh, the lane "echo-sql" of
# golang_docker.yml, with the fakes of ../golang-docker-fakes.sh (read its
# header). Run by linux-ci-scripts.yml, or locally:
#
#   bash .github/workflows/test_workflow_scripts/golang/echo_sql/golang-docker.tests.sh
#
# Under `bash -e` a keploy test that exits non-zero ended the step at that
# line, before the script's checks could print its log: the step failed with
# no line saying what had failed. Every keploy test's exit has to be named,
# with the reports and the command's log.
#
# every command succeeds, every report passes
#   The script has to end 0, past its last check; under pipefail too, which
#   the step does not set today, so that setting it never ends the step at a
#   count of test runs taken before there is one.
# keploy test (the first replay) exits 4 with an ERROR line in its log
#   The ERROR check runs first: the script has to exit 1 with its message and
#   keploy test's log, whatever keploy's code.
# keploy test (the first replay) fails test-set-1's test-5
#   The script has to exit with keploy test's code, name keploy test and that
#   code, the report that failed and its failed test (not the tests that
#   passed, before it and after it), and print keploy test's log; and name no
#   report the run did not write.
# keploy test --storage-format json fails test-set-1's test-2
#   The script has to exit with its code, naming that replay, the failed test
#   of the json report (not the tests that passed, before it and after it),
#   and print its log.
# keploy test --storage-format json fails before it writes a test run
#   The script has to name that replay and print its log, and print no report
#   of the yaml replay's test run.
# the --keep-app-alive replay fails
#   The script has to exit with its code, with its own message for that
#   replay, which a `replay_status=$?` after the command never reached under
#   `bash -e`.
# the --keep-app-alive replay exits 0 but writes no test run
#   The script has to fail it, counting the test runs before and after it.
#
# Each failing command runs twice, exiting 1 and then another code: the step
# has to exit with keploy's own code, and a code of 1 has to fail it too. In
# no case may bash raise an error in the lane script or a script it sources.

# Read by golang-docker-fakes.sh.
# shellcheck disable=SC2034
LANE_SCRIPT=golang/echo_sql/golang-docker.sh
LANE_APP=echo-sql
# shellcheck source=../golang-docker-fakes.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../golang-docker-fakes.sh"

run_case pass FAKE_JSON=1
expect pass 0 \
    "All tests passed (baseline run, no --keep-app-alive)" \
    "All tests passed (yaml + json)" \
    "All tests passed (--keep-app-alive run)"

LANE_SHELL_OPTS="-e -o pipefail" run_case pass-pipefail FAKE_JSON=1
expect pass-pipefail 0 \
    "All tests passed (baseline run, no --keep-app-alive)" \
    "All tests passed (yaml + json)" \
    "All tests passed (--keep-app-alive run)"

run_case replay-error-line FAKE_TEST_RC_1=4 FAKE_TEST_ERROR_1=1
expect replay-error-line 1 \
    "Error found in pipeline..." \
    "ERROR fake keploy test 1 error" \
    "fake keploy test 1 log" \
    -- "::error::keploy test exited"

for code in 1 4; do
    run_case "replay-fails-$code" FAKE_JSON=1 FAKE_TEST_RC_1="$code" \
        FAKE_TEST_1="0:PASSED 1:FAILED:test-5"
    expect "replay-fails-$code" "$code" \
        "::error::keploy test exited $code" \
        "test-run-0/test-set-1-report.yaml: FAILED" \
        "  failed: test-5" \
        "fake keploy test 1 log" \
        -- "failed: test-1" "failed: test-9" "report.json"
done

for code in 1 5; do
    run_case "json-replay-fails-$code" FAKE_JSON=1 FAKE_TEST_RC_2="$code" \
        FAKE_TEST_2="0:PASSED 1:FAILED:test-2"
    expect "json-replay-fails-$code" "$code" \
        "::error::keploy test --storage-format json exited $code" \
        "test-run-1/test-set-1-report.json: FAILED" \
        "  failed: test-2" \
        "fake keploy test 2 log" \
        -- "failed: test-1" "failed: test-9" "report.yaml"
done

run_case json-replay-writes-no-run FAKE_JSON=1 FAKE_TEST_RC_2=1 FAKE_TEST_NORUN_2=1
expect json-replay-writes-no-run 1 \
    "::error::keploy test --storage-format json exited 1" \
    "fake keploy test 2 log" \
    -- "test-run-"

for code in 1 3; do
    run_case "keep-app-alive-fails-$code" FAKE_TEST_RC_2="$code"
    expect "keep-app-alive-fails-$code" "$code" \
        "::error::--keep-app-alive replay failed with exit code $code" \
        "fake keploy test 2 log"
done

run_case keep-app-alive-writes-no-run FAKE_JSON=1 FAKE_TEST_NORUN_3=1
expect keep-app-alive-writes-no-run 1 \
    "::error::--keep-app-alive replay did not produce a new test-run-* directory (prev=2, new=2)" \
    "fake keploy test 3 log"

finish
