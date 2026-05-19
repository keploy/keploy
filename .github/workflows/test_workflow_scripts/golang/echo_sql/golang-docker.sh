#!/bin/bash

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

# Build Docker Image
docker compose build

# Remove any preexisting keploy tests and mocks.
sudo rm -rf keploy/

# Generate the keploy-config file.
$RECORD_BIN config --generate

# Update the global noise to ts in the config file.
config_file="./keploy.yml"
sed -i 's/global: {}/global: {"body": {"ts":[]}}/' "$config_file"

container_kill() {
    REC_PID="$(pgrep -n -f "$(basename "${RECORD_BIN:-keploy}") record" || true)"
    echo "$REC_PID Keploy PID"
    echo "Killing keploy"
    sudo kill -INT "$REC_PID" 2>/dev/null || true
}

send_request(){
    sleep 10
    app_started=false
    while [ "$app_started" = false ]; do
        if curl -X GET http://localhost:8082/health; then
            app_started=true
        fi
        sleep 3
    done
    echo "App started"
    # Make curl calls to record the test cases and mocks.
    curl --request POST \
      --url http://localhost:8082/url \
      --header 'content-type: application/json' \
      --data '{
      "url": "https://google.com"
    }'

    curl --request POST \
      --url http://localhost:8082/url \
      --header 'content-type: application/json' \
      --data '{
      "url": "https://facebook.com"
    }'

    curl -X GET http://localhost:8082/health

    # Wait for 5 seconds for keploy to record the test cases and mocks.
    sleep 5
    container_kill
    wait
}


for i in {1..2}; do
    container_name="echoApp"
    send_request &
    # get_container_health &
    $RECORD_BIN record -c "docker compose up" --container-name "$container_name" --generateGithubActions=false |& tee "${container_name}.txt"

    if grep "WARNING: DATA RACE" "${container_name}.txt"; then
        echo "Race condition detected in recording, stopping pipeline..."
        cat "${container_name}.txt"
        exit 1
    fi
    if grep "ERROR" "${container_name}.txt"; then
        echo "Error found in pipeline..."
        cat "${container_name}.txt"
        cat "docker-compose-tmp.yaml"
        exit 1
    fi
    sleep 5

    echo "Recorded test case and mocks for iteration ${i}"
done

# Shutdown services before test mode - Keploy should use mocks for dependencies
echo "Shutting down docker compose services before test mode..."
docker compose down
echo "Services stopped - Keploy should now use mocks for dependency interactions"

# Start keploy in test mode.
test_container="echoApp"
$REPLAY_BIN test -c 'docker compose up' --containerName "$test_container" --apiTimeout 60 --delay 15 --generate-github-actions=false &> "${test_container}.txt"

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

# ── --keep-app-alive regression coverage (docker-compose cmdType) ───────────
# See gin_mongo/golang-linux.sh for the full rationale. Same shape:
#   - second replay with --keep-app-alive scoped to a single test-set
#     so cross-test-set state bleed (postgres rows recorded during
#     test-set-0 would otherwise leak into test-set-1's expected
#     "before" state when the app survives the boundary) can't trip
#     the assertion
#   - capture the replay exit code AND verify a new test-run-* dir
#     was created — guards against the binary rejecting the flag and
#     re-using the baseline's report dir
#   - same script for all three matrix variants — keploy-latest's
#     missing flag causes a non-zero exit and no new test-run-* dir,
#     so the leg goes red as designed
echo "===== --keep-app-alive regression run ====="
# Wipe any leftover compose state from the baseline run before the
# second one — docker compose up will reuse stopped containers
# otherwise and the new run won't be a clean lifecycle test.
docker compose down -v --remove-orphans >/dev/null 2>&1 || true

prev_report_count=$(ls -d ./keploy/reports/test-run-* 2>/dev/null | wc -l)

ka_container="echoApp_keep_app_alive"
$REPLAY_BIN test -c "docker compose up" \
    --containerName "$ka_container" \
    --apiTimeout 60 \
    --delay 15 \
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