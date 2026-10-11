#!/bin/bash
source "$(dirname "${BASH_SOURCE[0]}")/../../go-retry.sh"

echo "$RECORD_BIN"
echo "$REPLAY_BIN"

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh
echo "iid.sh executed"

# Checkout a different branch
git fetch origin
git checkout origin/chore/add-sse-preflight-sample

# Check if there is a keploy-config file, if there is, delete it.
if [ -f "./keploy.yml" ]; then
    rm ./keploy.yml
fi

rm -rf keploy/

# Build go binaries
go_retry build -o sse-preflight-server ./cmd/server
go_retry build -o sse-preflight-client ./cmd/client
echo "go binaries built"

# Generate the keploy-config file.
# The step above removed any config, so this always generates; the guard is
# here so the line stays correct if that removal is ever dropped.
[ -f keploy.yml ] || sudo "$RECORD_BIN" config --generate

send_request() {
    sleep 6
    app_started=false
    while [ "$app_started" = false ]; do
        if curl -s http://localhost:8000/health; then
            app_started=true
        fi
        sleep 3
    done

    echo "App started"

    # Send CORS preflight (OPTIONS) via the client binary (as per README)
    ./sse-preflight-client \
        --url "http://localhost:8047/subscribe/student/events?doubtId=repro" \
        --host "doubt-service.example.com"

    # Wait for Keploy to record the tcs and mocks.
    sleep 7
    pid=$(pgrep keploy)
    echo "$pid Keploy PID"
    echo "Killing Keploy"
    sudo kill $pid
}

do_record() {
    local extra_flags="${1:-}"
    local label="${extra_flags:+_json}"
    local log="record${label}.txt"
    send_request &
    # shellcheck disable=SC2086
    "$RECORD_BIN" record $extra_flags -c "./sse-preflight-server" --generateGithubActions=false 2>&1 | tee "$log"
    if grep "ERROR" "$log"; then
        echo "Error found in pipeline..."
        cat "$log"
        exit 1
    fi
    if grep "WARNING: DATA RACE" "$log"; then
        echo "Race condition detected in recording, stopping pipeline..."
        cat "$log"
        exit 1
    fi
    sleep 5
    wait
    echo "Recorded test case and mocks${label:+ (json)}"
}

do_record

# shellcheck disable=SC1091
source "${GITHUB_WORKSPACE:-${PWD%/samples-*}}/.github/workflows/test_workflow_scripts/json-pass-helpers.sh"

if json_pass_supported; then
    do_record "--storage-format json"
fi

# Start the app in test mode.
"$REPLAY_BIN" test -c "./sse-preflight-server" --generateGithubActions=false 2>&1 | tee test_logs.txt

if grep "ERROR" "test_logs.txt"; then
    echo "Error found in pipeline..."
    cat "test_logs.txt"
    exit 1
fi

if grep "WARNING: DATA RACE" "test_logs.txt"; then
    echo "Race condition detected in test, stopping pipeline..."
    cat "test_logs.txt"
    exit 1
fi

all_passed=true

# Get the test results from the testReport file.
for report_file in ./keploy/reports/test-run-0/test-set-*-report.yaml; do
    [ -e "$report_file" ] || { echo "No report files found."; all_passed=false; break; }

    test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
    test_set_name=$(basename "$report_file" -report.yaml)

    echo "Test status for ${test_set_name}: $test_status"

    if [ "$test_status" != "PASSED" ]; then
        all_passed=false
        echo "${test_set_name} did not pass."
        break
    fi
done

# Fail fast on yaml results before attempting json.
if [ "$all_passed" != true ]; then
    cat "test_logs.txt"
    exit 1
fi

# A test set skipped whole, under the proven switch (KEPLOY_PROVEN=1, the
# proven model's staging gate). Without the switch such a set's report holds
# only counts, so neither the report nor `keploy report --format junit` can name
# a skipped test or say why. With it, every test of the set is one IGNORED row,
# and JUnit has one <skipped> case per test. Only a replay binary built from
# this tree has the switch. keploy.yml is restored afterwards.
case "${REPLAY_BIN:-}" in
    */build/keploy|*/build-no-race/keploy)
        cp keploy.yml keploy.yml.before-ignore
        yq -i '.test.ignoredTests = {"test-set-0": []}' keploy.yml
        rm -rf ./keploy/reports
        KEPLOY_PROVEN=1 "$REPLAY_BIN" test -c "./sse-preflight-server" --generateGithubActions=false 2>&1 | tee test_logs_ignored.txt
        mv keploy.yml.before-ignore keploy.yml
        if grep "WARNING: DATA RACE" test_logs_ignored.txt; then
            echo "::error::Race condition detected in the ignored-set replay"
            exit 1
        fi
        if grep -q "failed to get testcase results" test_logs_ignored.txt; then
            echo "::error::the ignored set left the run without results to read; each skipped test should be an IGNORED row"
            cat test_logs_ignored.txt
            exit 1
        fi
        ignored_report=./keploy/reports/test-run-0/test-set-0-report.yaml
        recorded=$(find ./keploy/test-set-0/tests -name '*.yaml' | wc -l)
        if [ "$(yq '.status' "$ignored_report")" != "IGNORED" ] || [ "$recorded" -lt 1 ]; then
            echo "::error::test-set-0 is $(yq '.status' "$ignored_report") with $recorded recorded tests; want IGNORED with at least one"
            cat "$ignored_report"
            exit 1
        fi
        rows=$(yq '[.tests[] | select(.status == "IGNORED")] | length' "$ignored_report")
        if [ "$rows" -ne "$recorded" ] || [ "$(yq '.tests | length' "$ignored_report")" -ne "$recorded" ]; then
            echo "::error::under KEPLOY_PROVEN=1 the skipped set's report has $rows IGNORED rows for $recorded recorded tests; want one per test"
            cat "$ignored_report"
            exit 1
        fi
        "$REPLAY_BIN" report --format junit > junit_ignored.xml 2> junit_ignored_err.txt
        junit_skipped=$(grep -c '<skipped' junit_ignored.xml || true)
        if [ "$junit_skipped" -lt "$recorded" ]; then
            echo "::error::keploy report --format junit has $junit_skipped <skipped> cases; want one for each of the $recorded skipped tests"
            cat junit_ignored.xml
            exit 1
        fi
        echo "✓ under KEPLOY_PROVEN=1 the skipped set is reported test by test ($rows IGNORED rows, $junit_skipped JUnit <skipped> cases)"
        ;;
esac

if json_pass_supported; then
    "$REPLAY_BIN" test --storage-format json -c "./sse-preflight-server" --generateGithubActions=false 2>&1 | tee test_logs_json.txt
    if grep "ERROR" "test_logs_json.txt"; then
        echo "Error found in pipeline (json replay)..."
        cat "test_logs_json.txt"
        exit 1
    fi
    if grep "WARNING: DATA RACE" "test_logs_json.txt"; then
        echo "Race condition detected in json test, stopping pipeline..."
        cat "test_logs_json.txt"
        exit 1
    fi
    if ! json_scan_reports; then
        cat test_logs_json.txt
        exit 1
    fi
    echo "All tests passed (yaml + json)"
else
    echo "All tests passed (yaml only — json pass skipped for compat-matrix cell)"
fi
exit 0
