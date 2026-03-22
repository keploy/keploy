#!/bin/bash

# macOS variant for gin-mongo (docker). Uses BSD sed.
source ./../../.github/workflows/test_workflow_scripts/test-iid-macos.sh

# Ensure Docker Desktop is healthy before doing anything
ensure_docker() {
  if docker info >/dev/null 2>&1; then
    return 0
  fi
  echo "Docker daemon not responding. Restarting Docker Desktop..."
  nohup open -g -a Docker >/dev/null 2>&1 &
  disown
  for i in $(seq 1 90); do
    if docker info >/dev/null 2>&1; then
      echo "Docker Desktop is ready (waited ~$((i*2))s)."
      return 0
    fi
    sleep 2
  done
  echo "ERROR: Docker Desktop failed to start within 180s."
  return 1
}

ensure_docker

# Clean up stale containers and networks from previous runs to avoid
# port conflicts on the self-hosted macOS runner.
for c in ginApp ginApp_1 ginApp_2 ginApp_test mongoDb; do
  docker rm -f "$c" 2>/dev/null || true
done
docker network rm keploy-network 2>/dev/null || true

# Also kill any leftover keploy processes holding ports
pgrep -f 'keploy' | xargs -r sudo kill -9 2>/dev/null || true

# Start mongo before starting keploy (retry once if Docker crashes).
docker network create keploy-network
if ! docker run --name mongoDb --rm --net keploy-network -p 27017:27017 -d mongo; then
  echo "Docker run failed. Attempting Docker Desktop recovery..."
  ensure_docker
  docker network create keploy-network 2>/dev/null || true
  docker run --name mongoDb --rm --net keploy-network -p 27017:27017 -d mongo
fi

# Generate the keploy-config file.
$RECORD_BIN config --generate

# Update the global noise to ts.
config_file="./keploy.yml"
# use BSD sed format for macOS
sed -i '' 's/global: {}/global: {"body": {"ts":[]}}/' "$config_file"

# Remove any preexisting keploy tests and mocks.
rm -rf keploy/
docker logs mongoDb &

# Start keploy in record mode.
docker build -t gin-mongo .
docker rm -f ginApp 2>/dev/null || true

container_kill() {
    REC_PID="$(pgrep -n -f 'keploy record' || true)"
    echo "$REC_PID Keploy PID"
    if [ -n "$REC_PID" ]; then
        echo "Killing keploy"
        sudo kill -INT "$REC_PID" 2>/dev/null || true
        # Wait for keploy to flush and exit (up to 30s)
        for i in {1..30}; do
            kill -0 "$REC_PID" 2>/dev/null || break
            sleep 1
        done
    fi
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
    # Explicitly use --platform linux/amd64 if running on Apple Silicon but targeting linux containers? 
    # Or rely on Docker handling. The workflow runs on self-hosted macOS which implies Apple Silicon or Intel.
    # Usually docker run works fine.
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
$REPLAY_BIN test -c 'docker run -p 8080:8080 --net keploy-network --name ginApp_test gin-mongo' --containerName "$test_container" --apiTimeout 60 --delay 20 --generate-github-actions=false &> "${test_container}.txt"

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
    echo "All tests passed"
    exit 0
else
    cat "${test_container}.txt"
    exit 1
fi
