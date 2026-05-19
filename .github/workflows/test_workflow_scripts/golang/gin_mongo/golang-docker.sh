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

for i in {1..2}; do
    container_name="ginApp_${i}"
    send_request &
    $RECORD_BIN record -c "docker run -p 8080:8080 --net keploy-network --rm --name $container_name gin-mongo" --container-name "$container_name"    &> "${container_name}.txt"

    if grep "WARNING: DATA RACE" "${container_name}.txt"; then
        echo "Race condition detected in recording, stopping pipeline..."
        cat "${container_name}.txt"
        exit 1
    fi
    if grep "ERROR" "${container_name}.txt"; then
        echo "Error found in pipeline..."
        cat "${container_name}.txt"
        exit 1
    fi
    sleep 5

    echo "Recorded test case and mocks for iteration ${i}"
done

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

# ── --keep-app-alive regression coverage (docker-run cmdType) ───────────────
# Second replay run with --keep-app-alive set. The flag starts the app
# container ONCE for the whole replay (lift-app behaviour) instead of
# restarting it per test-set. This script exercises the docker-run
# cmdType (note: `docker run --rm ...` in -c), so it pins the one-shot
# RunApplication path for that cmdType. Asserts every test-set in the
# new report directory (test-run-1) is PASSED.
echo "===== --keep-app-alive regression run ====="
ka_container="ginApp_test_keep_app_alive"
$REPLAY_BIN test -c "docker run --rm -p 8080:8080 --net keploy-network --name $ka_container gin-mongo" \
    --containerName "$ka_container" \
    --apiTimeout 60 \
    --delay 20 \
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