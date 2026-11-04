#!/usr/bin/env bash
# Tests for gin_mongo/golang-docker.sh, the lane "gin-mongo" of
# golang_docker.yml, with the fakes of ../golang-docker-fakes.sh (read its
# header). Run by linux-ci-scripts.yml, or locally:
#
#   bash .github/workflows/test_workflow_scripts/golang/gin_mongo/golang-docker.tests.sh
#
# Under `bash -e` a keploy command that exits non-zero ended the step at that
# line, before the script's checks could print its log: the step failed with
# no line saying what had failed (a keploy test that failed a test set, in a
# run whose record had answered one request with an error). Every keploy
# command's exit has to be named, with the reports and the command's log.
#
# every command succeeds, every report passes
#   The script has to end 0, past its last check; under pipefail too, which
#   the step does not set today, so that setting it never ends the step at a
#   count of test runs taken before there is one.
# keploy test (the first replay or the json one) exits non-zero with an ERROR
# or a DATA RACE line in its log
#   Those checks run first: the script has to exit 1 with the line and the
#   log, whatever keploy's code, and name no keploy test exit.
# keploy record exits 2 with an ERROR or a DATA RACE line in its log
#   Those checks run first too: the script has to exit 1 with that check's
#   message, and name no record exit.
# keploy test (the first replay) fails test-set-2's test-5
#   The script has to exit with keploy test's code, name keploy test and that
#   code, the report that failed and its failed test (not the tests that
#   passed, before it and after it), and print keploy test's log; and name no
#   report the run did not write.
# keploy record of iteration 2, then of the first json iteration, fails
#   The script has to exit with the record's code, naming that record (the
#   json one as json), print its log, and not say the iteration was recorded.
# keploy test --storage-format json fails test-set-3's test-2
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
LANE_SCRIPT=golang/gin_mongo/golang-docker.sh
LANE_APP=gin-mongo
# shellcheck source=../golang-docker-fakes.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../golang-docker-fakes.sh"

run_case pass FAKE_JSON=1 FAKE_TEST_2="0:PASSED 1:PASSED 2:PASSED 3:PASSED"
expect pass 0 "All tests passed (yaml + json)" "All tests passed (--keep-app-alive run)"

LANE_SHELL_OPTS="-e -o pipefail" run_case pass-pipefail FAKE_JSON=1 \
    FAKE_TEST_2="0:PASSED 1:PASSED 2:PASSED 3:PASSED"
expect pass-pipefail 0 "All tests passed (yaml + json)" "All tests passed (--keep-app-alive run)"

run_case replay-error-line FAKE_TEST_RC_1=4 FAKE_TEST_ERROR_1=1
expect replay-error-line 1 \
    "Error found in pipeline..." \
    "ERROR fake keploy test 1 error" \
    "fake keploy test 1 log" \
    -- "::error::keploy test exited"

run_case replay-race-line FAKE_TEST_RC_1=4 FAKE_TEST_RACE_1=1
expect replay-race-line 1 \
    "Race condition detected in test, stopping pipeline..." \
    "WARNING: DATA RACE in fake keploy test 1" \
    "fake keploy test 1 log" \
    -- "::error::keploy test exited"

run_case json-replay-error-line FAKE_JSON=1 FAKE_TEST_RC_2=5 FAKE_TEST_ERROR_2=1
expect json-replay-error-line 1 \
    "ERROR fake keploy test 2 error" \
    "fake keploy test 2 log" \
    -- "::error::keploy test --storage-format json exited"

run_case json-replay-race-line FAKE_JSON=1 FAKE_TEST_RC_2=5 FAKE_TEST_RACE_2=1
expect json-replay-race-line 1 \
    "WARNING: DATA RACE in fake keploy test 2" \
    "fake keploy test 2 log" \
    -- "::error::keploy test --storage-format json exited"

run_case record-error-line FAKE_RECORD_RC_2=2 FAKE_RECORD_SAYS_2="ERROR fake keploy record error"
expect record-error-line 1 \
    "Error found in pipeline..." \
    "fake keploy record 2 log" \
    -- "::error::keploy record" "Recorded test case and mocks for iteration 2"

run_case record-race-line FAKE_RECORD_RC_2=2 FAKE_RECORD_SAYS_2="WARNING: DATA RACE in fake keploy record"
expect record-race-line 1 \
    "Race condition detected in recording, stopping pipeline..." \
    "fake keploy record 2 log" \
    -- "::error::keploy record" "Recorded test case and mocks for iteration 2"

for code in 1 4; do
    run_case "replay-fails-$code" FAKE_JSON=1 FAKE_TEST_RC_1="$code" \
        FAKE_TEST_1="0:PASSED 1:PASSED 2:FAILED:test-5 3:PASSED"
    expect "replay-fails-$code" "$code" \
        "::error::keploy test exited $code" \
        "test-run-0/test-set-2-report.yaml: FAILED" \
        "  failed: test-5" \
        "fake keploy test 1 log" \
        -- "failed: test-1" "failed: test-9" "report.json"
done

for code in 1 2; do
    run_case "record-fails-$code" FAKE_RECORD_RC_2="$code"
    expect "record-fails-$code" "$code" \
        "::error::keploy record (iteration 2) exited $code" \
        "fake keploy record 2 log" \
        -- "Recorded test case and mocks for iteration 2" "test-run-"
done

for code in 1 6; do
    run_case "json-record-fails-$code" FAKE_JSON=1 FAKE_RECORD_RC_3="$code"
    expect "json-record-fails-$code" "$code" \
        "::error::keploy record (iteration 1, json) exited $code" \
        "fake keploy record 3 log" \
        -- "Recorded test case and mocks for iteration 1 (json)" "test-run-"
done

for code in 1 5; do
    run_case "json-replay-fails-$code" FAKE_JSON=1 FAKE_TEST_RC_2="$code" \
        FAKE_TEST_2="0:PASSED 1:PASSED 2:PASSED 3:FAILED:test-2"
    expect "json-replay-fails-$code" "$code" \
        "::error::keploy test --storage-format json exited $code" \
        "test-run-1/test-set-3-report.json: FAILED" \
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
