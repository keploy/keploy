#!/bin/bash

# macOS variant for gin-mongo (docker). Uses BSD sed.
# Fully isolated for concurrent execution on shared self-hosted runners.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEST_IID_SCRIPT="${SCRIPT_DIR}/../../test-iid-macos.sh"
if [[ ! -f "${TEST_IID_SCRIPT}" ]]; then
  echo "ERROR: Required helper script not found at ${TEST_IID_SCRIPT}."
  exit 1
fi
source "${TEST_IID_SCRIPT}"

# Verify Docker Desktop is running -- never start it from CI.
if ! docker info >/dev/null 2>&1; then
  echo "ERROR: Docker Desktop is not running."
  echo "Ensure Docker Desktop is configured as a Login Item on the self-hosted runner."
  exit 1
fi

# ---------------------------------------------------------------------------
# Dynamic port allocation -- avoids conflicts with concurrent jobs.
# Uses a JOB_ID-derived offset so concurrent jobs on the same machine
# don't race for the same starting port (TOCTOU prevention).
# ---------------------------------------------------------------------------
find_available_port() {
    local start_port=${1:-6000}
    local port=$start_port
    while lsof -i:$port >/dev/null 2>&1; do
        port=$((port + 1))
    done
    echo $port
}

# gin-mongo uses base range 18000-19999, offset by JOB_ID hash
PORT_OFFSET=$(( $(echo "$JOB_ID" | cksum | awk '{print $1}') % 400 ))
BASE_PORT=$(( 18000 + PORT_OFFSET * 5 ))
APP_PORT=$(find_available_port $BASE_PORT)
DB_PORT=$(find_available_port $((APP_PORT + 1)))
PROXY_PORT=$(find_available_port $((DB_PORT + 1)))
DNS_PORT=$(find_available_port $((PROXY_PORT + 1)))

echo "Using ports - APP: $APP_PORT, DB: $DB_PORT, PROXY: $PROXY_PORT, DNS: $DNS_PORT"

# ---------------------------------------------------------------------------
# Unique resource names scoped to this job -- no collisions across runners.
# ---------------------------------------------------------------------------
NETWORK_NAME="keploy-network-${JOB_ID}"
MONGO_CONTAINER="mongoDb_${JOB_ID}"
APP_IMAGE="gin-mongo-${JOB_ID}"
KEPLOY_CONTAINER="keploy_ginmongo_${JOB_ID}"

echo "Using network: $NETWORK_NAME"
echo "Using containers - MONGO: $MONGO_CONTAINER, APP_IMAGE: $APP_IMAGE, KEPLOY: $KEPLOY_CONTAINER"

# ---------------------------------------------------------------------------
# Cleanup -- runs on exit to free resources regardless of outcome.
# ---------------------------------------------------------------------------
cleanup() {
    echo "Cleaning up containers and network for job ${JOB_ID}..."
    for c in "${MONGO_CONTAINER}" "ginApp_${JOB_ID}_1" "ginApp_${JOB_ID}_2" "ginApp_${JOB_ID}_test" "${KEPLOY_CONTAINER}"; do
        docker rm -f "$c" >/dev/null 2>&1 || true
    done
    docker network rm "$NETWORK_NAME" >/dev/null 2>&1 || true
    docker rmi "$APP_IMAGE" >/dev/null 2>&1 || true
    echo "Cleanup completed for job ${JOB_ID}"
}
trap cleanup EXIT INT TERM

# Clean up any stale resources from a previous failed run of this exact JOB_ID (unlikely but safe).
cleanup 2>/dev/null || true

# Create isolated Docker network for this job (idempotent).
docker network rm "$NETWORK_NAME" >/dev/null 2>&1 || true
docker network create "$NETWORK_NAME"

# Start MongoDB with a unique container name but a network alias of "mongoDb"
# so the gin-mongo app (which hardcodes "mongoDb:27017") resolves correctly.
docker run --name "$MONGO_CONTAINER" --rm \
  --net "$NETWORK_NAME" --network-alias mongoDb \
  -p "${DB_PORT}:27017" -d mongo

# Generate the keploy-config file.
$RECORD_BIN config --generate

# Update the global noise to ts.
config_file="./keploy.yml"
sed -i '' 's/global: {}/global: {"body": {"ts":[]}}/' "$config_file"

# Remove any preexisting keploy tests and mocks.
rm -rf keploy/
docker logs "$MONGO_CONTAINER" &

# Replace hardcoded port 8080 in source and Dockerfile so keploy records
# and replays with the same dynamic port (avoids host/container port mismatch).
echo "Updating source files to use APP_PORT=${APP_PORT}..."
sed -i '' "s/port := \"8080\"/port := \"${APP_PORT}\"/" main.go
sed -i '' "s/EXPOSE 8080/EXPOSE ${APP_PORT}/" Dockerfile
for file in $(find . -maxdepth 1 -type f \( -name "*.yml" -o -name "*.yaml" \)); do
    sed -i '' "s/8080/${APP_PORT}/g" "$file" 2>/dev/null || true
done

# Build the app image with a unique tag.
docker build -t "$APP_IMAGE" .

send_request(){
    echo "Sending requests to the application..."
    sleep 10
    app_started=false
    for attempt in $(seq 1 30); do
        if curl --fail --silent -X GET http://localhost:${APP_PORT}/CJBKJd92 >/dev/null 2>&1; then
            app_started=true
            break
        fi
        sleep 3
    done
    if [ "$app_started" = false ]; then
        echo "ERROR: App failed to start after 30 attempts"
        return 1
    fi
    echo "App started"

    curl --request POST \
      --url http://localhost:${APP_PORT}/url \
      --header 'content-type: application/json' \
      --data '{
      "url": "https://google.com"
    }'

    curl --request POST \
      --url http://localhost:${APP_PORT}/url \
      --header 'content-type: application/json' \
      --data '{
      "url": "https://facebook.com"
    }'

    curl -X GET http://localhost:${APP_PORT}/CJBKJd92

    curl --request GET \
      --url "http://localhost:${APP_PORT}/verify-email?email=test@gmail.com" \
      --header 'Accept: application/json'

    curl --request GET \
      --url "http://localhost:${APP_PORT}/verify-email?email=admin@yahoo.com" \
      --header 'Accept: application/json'

    sleep 3
    echo "Requests sent successfully."
}

do_record_iteration() {
    local i="$1"
    local extra_flags="${2:-}"
    local label="${extra_flags:+_json}"
    local container_name="ginApp_${JOB_ID}_${i}${label}"
    send_request &
    # shellcheck disable=SC2086
    $RECORD_BIN record $extra_flags \
      -c "docker run -p ${APP_PORT}:${APP_PORT} --net ${NETWORK_NAME} --rm --name $container_name ${APP_IMAGE}" \
      --container-name "$container_name" \
      --generate-github-actions=false \
      --proxy-port=$PROXY_PORT \
      --dns-port=$DNS_PORT \
      --keploy-container "$KEPLOY_CONTAINER" \
      --record-timer "40s" \
      2>&1 | tee "${container_name}.txt"

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
    echo "Recorded test case and mocks for iteration ${i}${label:+ (json)}"
}

for i in {1..2}; do
    do_record_iteration "$i"
done

# shellcheck disable=SC1091
source "${GITHUB_WORKSPACE}/.github/workflows/test_workflow_scripts/json-pass-helpers.sh"

if json_pass_supported; then
    for i in {1..2}; do
        do_record_iteration "$i" "--storage-format json"
    done
fi

# Start the keploy in test mode.
test_container="ginApp_${JOB_ID}_test"
$REPLAY_BIN test \
  -c "docker run --rm -p ${APP_PORT}:${APP_PORT} --net ${NETWORK_NAME} --name $test_container ${APP_IMAGE}" \
  --containerName "$test_container" \
  --apiTimeout 60 \
  --delay 20 \
  --generate-github-actions=false \
  --proxy-port=$PROXY_PORT \
  --dns-port=$DNS_PORT \
  --keploy-container "$KEPLOY_CONTAINER" \
  2>&1 | tee "${test_container}.txt"

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
    report_file="./keploy/reports/test-run-0/test-set-$i-report.yaml"
    test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
    echo "Test status for test-set-$i: $test_status"

    if [ "$test_status" != "PASSED" ]; then
        all_passed=false
        echo "Test-set-$i did not pass."
        break
    fi
done

if [ "$all_passed" != true ]; then
    cat "${test_container}.txt"
    exit 1
fi

if json_pass_supported; then
    test_container_json="${test_container}_json"
    $REPLAY_BIN test --storage-format json \
      -c "docker run --rm -p ${APP_PORT}:${APP_PORT} --net ${NETWORK_NAME} --name $test_container_json ${APP_IMAGE}" \
      --containerName "$test_container_json" \
      --apiTimeout 60 \
      --delay 20 \
      --generate-github-actions=false \
      --proxy-port=$PROXY_PORT \
      --dns-port=$DNS_PORT \
      --keploy-container "$KEPLOY_CONTAINER" \
      2>&1 | tee "${test_container_json}.txt"
    if grep "ERROR" "${test_container_json}.txt"; then
        cat "${test_container_json}.txt"
        exit 1
    fi
    if grep "WARNING: DATA RACE" "${test_container_json}.txt"; then
        cat "${test_container_json}.txt"
        exit 1
    fi
    if ! json_scan_reports; then
        cat "${test_container_json}.txt"
        exit 1
    fi
    echo "All tests passed (yaml + json)"
else
    echo "All tests passed (yaml only — json pass skipped for compat-matrix cell)"
fi
exit 0
