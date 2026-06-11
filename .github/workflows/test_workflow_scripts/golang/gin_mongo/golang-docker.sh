#!/bin/bash

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

# Start mongo before starting keploy.
docker network create keploy-network
docker run --name mongoDb --rm --net keploy-network -p 27017:27017 -d mongo

# Generate the keploy-config file.
$RECORD_BIN config --generate

# Update the global noise to ts.
config_file="./keploy.yml"
sed -i 's/global: {}/global: {"body": {"ts":[]}}/' "$config_file"

# Remove any preexisting keploy tests and mocks.
sudo rm -rf keploy/
docker logs mongoDb &

# Start keploy in record mode.
docker build -t gin-mongo .
docker rm -f ginApp 2>/dev/null || true

container_kill() {
    # pid=$(pgrep -f "keploy record")
    # echo "$pid Keploy PID" 
    # echo "Killing keploy"
    # sudo kill $pid
    REC_PID="$(pgrep -n -f "$(basename "${RECORD_BIN:-keploy}") record" || true)"
    echo "$REC_PID Keploy PID"
    echo "Killing keploy"
    sudo kill -INT "$REC_PID" 2>/dev/null || true
}

send_request(){
    sleep 30
    app_started=false
    while [ "$app_started" = false ]; do
        if curl -X GET http://localhost:8080/CJBKJd92; then
            app_started=true
        fi
        sleep 3
    done
    echo "App started"
    # Start making curl calls to record the testcases and mocks.
    curl --request POST \
      --url http://localhost:8080/url \
      --header 'content-type: application/json' \
      --data '{
      "url": "https://google.com"
    }'

    curl --request POST \
      --url http://localhost:8080/url \
      --header 'content-type: application/json' \
      --data '{
      "url": "https://facebook.com"
    }'

    curl -X GET http://localhost:8080/CJBKJd92

    # Test email verification endpoint
    curl --request GET \
      --url 'http://localhost:8080/verify-email?email=test@gmail.com' \
      --header 'Accept: application/json'

    curl --request GET \
      --url 'http://localhost:8080/verify-email?email=admin@yahoo.com' \
      --header 'Accept: application/json'

    # Wait for 5 seconds for keploy to record the tcs and mocks.
    sleep 5
    container_kill
    wait
}

do_record_iteration() {
    local i="$1"
    local extra_flags="${2:-}"
    local label="${extra_flags:+_json}"
    local container_name="ginApp_${i}${label}"
    local log="${container_name}.txt"
    send_request &
    # shellcheck disable=SC2086
    $RECORD_BIN record $extra_flags -c "docker run -p 8080:8080 --net keploy-network --rm --name $container_name gin-mongo" --container-name "$container_name"    &> "$log"

    if grep "WARNING: DATA RACE" "$log"; then
        echo "Race condition detected in recording, stopping pipeline..."
        cat "$log"
        exit 1
    fi
    if grep "ERROR" "$log"; then
        echo "Error found in pipeline..."
        cat "$log"
        exit 1
    fi
    sleep 5
    echo "Recorded test case and mocks for iteration ${i}${label:+ (json)}"
}

for i in {1..2}; do
    do_record_iteration "$i"
done

# shellcheck disable=SC1091
source "${GITHUB_WORKSPACE:-${PWD%/samples-*}}/.github/workflows/test_workflow_scripts/json-pass-helpers.sh"

if json_pass_supported; then
    for i in {1..2}; do
        do_record_iteration "$i" "--storage-format json"
    done
fi

# Keep MongoDB running during test replay. Keploy will serve mocks for
# matched requests; unmatched requests fall through to the real database
# which returns the same data recorded earlier, preventing flaky failures
# caused by non-deterministic mock matching across test sets.

# Start the keploy in test mode.
test_container="ginApp_test"
$REPLAY_BIN test -c 'docker run --rm -p 8080:8080 --net keploy-network --name ginApp_test gin-mongo' --containerName "$test_container" --apiTimeout 60 --delay 20 --generate-github-actions=false &> "${test_container}.txt"

if grep "ERROR" "${test_container}.txt"; then
    echo "Error found in pipeline..."
    cat "${test_container}.txt"
    exit 1
fi

if grep "WARNING: DATA RACE" "${test_container}.txt"; then
    echo "Race condition detected in test, stopping pipeline..."
    cat "${test_container}.txt"
    exit 1
fi

all_passed=true

for i in {0..1}
do
    # Define the report file for each test set
    report_file="./keploy/reports/test-run-0/test-set-$i-report.yaml"

    # Extract the test status
    test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')

    # Print the status for debugging
    echo "Test status for test-set-$i: $test_status"

    # Check if any test set did not pass
    if [ "$test_status" != "PASSED" ]; then
        all_passed=false
        echo "Test-set-$i did not pass."
        break # Exit the loop early as all tests need to pass
    fi
done

# Check the overall test status and exit accordingly
if [ "$all_passed" = true ]; then
    echo "All tests passed (baseline run, no --keep-app-alive)"
else
    cat "${test_container}.txt"
    exit 1
fi

if json_pass_supported; then
    $REPLAY_BIN test --storage-format json -c 'docker run --rm -p 8080:8080 --net keploy-network --name ginApp_test_json gin-mongo' --containerName "ginApp_test_json" --apiTimeout 60 --delay 20 --generate-github-actions=false &> "${test_container}_json.txt"
    if grep "ERROR" "${test_container}_json.txt"; then
        cat "${test_container}_json.txt"
        exit 1
    fi
    if grep "WARNING: DATA RACE" "${test_container}_json.txt"; then
        cat "${test_container}_json.txt"
        exit 1
    fi
    if ! json_scan_reports; then
        cat "${test_container}_json.txt"
        exit 1
    fi
    echo "All tests passed (yaml + json)"
else
    echo "All tests passed (yaml only — json pass skipped for compat-matrix cell)"
fi

# ── --keep-app-alive regression coverage (docker-run cmdType) ───────────────
# See gin_mongo/golang-linux.sh for the full rationale. Same shape:
#   - second replay with --keep-app-alive scoped to a single test-set
#     so cross-test-set state bleed (the bug the flag is designed to
#     surface in real autoreplay) can't trip the assertion
#   - capture the replay exit code AND verify a new test-run-* dir
#     was created — guards against the binary rejecting the flag and
#     re-using the baseline's report dir, which on the previous
#     iteration of this script caused record_build_replay_latest to
#     falsely report PASSED against the stale BASELINE report
#   - same script runs for all three matrix variants
#       record_build_replay_build   — passes
#       record_latest_replay_build  — passes
#       record_build_replay_latest  — REPLAY_BIN is keploy-latest with
#                                     no flag → exit non-zero / no new
#                                     report dir → expected red
echo "===== --keep-app-alive regression run ====="
prev_report_count=$(ls -d ./keploy/reports/test-run-* 2>/dev/null | wc -l)

ka_container="ginApp_test_keep_app_alive"
$REPLAY_BIN test -c "docker run --rm -p 8080:8080 --net keploy-network --name $ka_container gin-mongo" \
    --containerName "$ka_container" \
    --apiTimeout 60 \
    --delay 20 \
    --keep-app-alive \
    --test-sets test-set-0 \
    --generate-github-actions=false &> "${ka_container}.txt"
replay_status=$?

if [ "$replay_status" -ne 0 ]; then
    echo "::error::--keep-app-alive replay failed with exit code ${replay_status}"
    cat "${ka_container}.txt"
    exit "$replay_status"
fi

if grep "WARNING: DATA RACE" "${ka_container}.txt"; then
    echo "Race condition detected in --keep-app-alive replay, stopping pipeline..."
    cat "${ka_container}.txt"
    exit 1
fi

new_report_count=$(ls -d ./keploy/reports/test-run-* 2>/dev/null | wc -l)
if [ "$new_report_count" -le "$prev_report_count" ]; then
    echo "::error::--keep-app-alive replay did not produce a new test-run-* directory (prev=${prev_report_count}, new=${new_report_count}). Most likely the binary rejected the --keep-app-alive flag."
    cat "${ka_container}.txt"
    exit 1
fi

latest_report_dir="$(ls -td ./keploy/reports/test-run-* 2>/dev/null | head -n 1)"
echo "--keep-app-alive report dir: $latest_report_dir"

report_file="$latest_report_dir/test-set-0-report.yaml"
if [ ! -f "$report_file" ]; then
    echo "::error::Missing $report_file"
    cat "${ka_container}.txt"
    exit 1
fi
test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
echo "[--keep-app-alive] Test status for test-set-0: $test_status"
if [ "$test_status" != "PASSED" ]; then
    echo "[--keep-app-alive] Test-set-0 did not pass."
    cat "${ka_container}.txt"
    exit 1
fi

echo "All tests passed (--keep-app-alive run)"
exit 0