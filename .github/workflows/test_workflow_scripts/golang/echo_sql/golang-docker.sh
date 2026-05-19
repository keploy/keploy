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
# Second replay run with --keep-app-alive set. The flag starts the
# compose stack ONCE for the whole replay (lift-app behaviour) instead
# of restarting it per test-set; this is the cmdType the flag was
# originally written for (production globality autoreplay uses this
# path). Asserts every test-set in the new report directory (test-run-1)
# is PASSED.
echo "===== --keep-app-alive regression run ====="
# Wipe any leftover compose state from the baseline run before the
# second one — docker compose up will reuse stopped containers
# otherwise and the new run won't be a clean lifecycle test.
docker compose down -v --remove-orphans >/dev/null 2>&1 || true

ka_container="echoApp_keep_app_alive"
$REPLAY_BIN test -c "docker compose up" \
    --containerName "$ka_container" \
    --apiTimeout 60 \
    --delay 15 \
    --keep-app-alive \
    --generate-github-actions=false &> "${ka_container}.txt"

if grep "ERROR" "${ka_container}.txt"; then
    echo "Error found in --keep-app-alive replay..."
    cat "${ka_container}.txt"
    exit 1
fi

if grep "WARNING: DATA RACE" "${ka_container}.txt"; then
    echo "Race condition detected in --keep-app-alive replay, stopping pipeline..."
    cat "${ka_container}.txt"
    exit 1
fi

# Reports from the second run land in test-run-1 because the baseline
# above already produced test-run-0. Glob the latest test-run-* in case
# keploy's numbering ever drifts.
latest_report_dir="$(ls -td ./keploy/reports/test-run-* 2>/dev/null | head -n 1 || true)"
if [ -z "$latest_report_dir" ]; then
    echo "::error::No test-run-* report directory after --keep-app-alive replay"
    exit 1
fi
echo "--keep-app-alive report dir: $latest_report_dir"

ka_all_passed=true
for i in {0..1}; do
    report_file="$latest_report_dir/test-set-$i-report.yaml"
    if [ ! -f "$report_file" ]; then
        echo "::error::Missing $report_file"
        ka_all_passed=false
        break
    fi
    test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
    echo "[--keep-app-alive] Test status for test-set-$i: $test_status"
    if [ "$test_status" != "PASSED" ]; then
        ka_all_passed=false
        echo "[--keep-app-alive] Test-set-$i did not pass."
        break
    fi
done

if [ "$ka_all_passed" = true ]; then
    echo "All tests passed (--keep-app-alive run)"
    exit 0
else
    cat "${ka_container}.txt"
    exit 1
fi