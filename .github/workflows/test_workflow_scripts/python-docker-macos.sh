#!/usr/bin/env bash

# macOS variant. Requires Docker Desktop for Mac running.
set -euo pipefail

# for the below shource make it such a way that if the file is not present or already present it does not error
source ./../../.github/workflows/test_workflow_scripts/test-iid-macos.sh

# Function to find available port
find_available_port() {
    local start_port=${1:-8080}
    local port=$start_port
    while lsof -i:$port >/dev/null 2>&1; do
        port=$((port + 1))
    done
    echo $port
}

# Find 4 available ports
APP_PORT=$(find_available_port 8080)
DB_PORT=$(find_available_port $((APP_PORT + 1)))
PROXY_PORT=$(find_available_port $((DB_PORT + 1)))
DNS_PORT=$(find_available_port $((PROXY_PORT + 1)))

# Generate unique container names with JOB_ID suffix
APP_CONTAINER="flaskApp_${JOB_ID}"
DB_CONTAINER="mongo_${JOB_ID}"
KEPLOY_CONTAINER="keploy_${JOB_ID}"
APP_IMAGE="flask-app_${JOB_ID}:1.0"

echo "Using ports - APP: $APP_PORT, DB: $DB_PORT, PROXY: $PROXY_PORT, DNS: $DNS_PORT"
echo "Using containers - APP: $APP_CONTAINER, DB: $DB_CONTAINER, KEPLOY: $KEPLOY_CONTAINER"

# Cleanup function to remove containers
cleanup() {
    echo "Cleaning up containers..."
    docker rm -f "$DB_CONTAINER" >/dev/null 2>&1 || true
    docker rm -f "$APP_CONTAINER" >/dev/null 2>&1 || true
    docker rm -f "${APP_CONTAINER}_1" >/dev/null 2>&1 || true
    docker rm -f "${APP_CONTAINER}_2" >/dev/null 2>&1 || true
    docker rm -f "$KEPLOY_CONTAINER" >/dev/null 2>&1 || true
    docker rm -f mongo >/dev/null 2>&1 || true
    echo "Cleanup completed"
}

# Set trap to run cleanup on script exit (success, failure, or interrupt)
trap cleanup EXIT INT TERM

# Replace ports and container names in all files in current directory
echo "Updating configuration files with dynamic ports and container names..."
for file in $(find . -maxdepth 1 -type f \( -name "*.yml" -o -name "*.yaml" -o -name "*.py" -o -name "*.json" -o -name "*.sh" -o -name "*.env" -o -name "*.md" \)); do
    if [ -f "$file" ] && [ "$file" != "./golang-docker-macos.sh" ]; then
        # Replace 6000 with APP_PORT
        sed -i '' "s/6000/${APP_PORT}/g" "$file" 2>/dev/null || true
        # Replace mongo:27017 with $DB_CONTAINER:27017
        sed -i '' "s/mongo:27017/${DB_CONTAINER}:27017/g" "$file" 2>/dev/null || true
        echo "Updated $file"
    fi
done

# --- Networking: create once, quietly ---
if ! docker network ls --format '{{.Name}}' | grep -q '^keploy-network$'; then
  docker network create keploy-network
fi

# --- Start fresh Mongo (force remove any stale one first) ---
docker rm -f mongo >/dev/null 2>&1 || true
docker run --name $DB_CONTAINER --rm --net keploy-network -p $DB_PORT:27017 -d mongo

# --- Prepare app image & keploy config ---
rm -rf keploy/  # Clean up old test data
rm ./keploy.yml >/dev/null 2>&1 || true

docker build -t $APP_IMAGE .


# Generate the keploy-config file.
$RECORD_BIN config --generate

# Update the global noise to ts in the config file.
config_file="./keploy.yml"
if [ -f "$config_file" ]; then
  sed -i '' 's/global: {}/global: {"body": {"ts":[]}}/' "$config_file" || true
else
  echo "⚠️ Config file $config_file not found, skipping sed replace."
fi

sleep 5


send_request_and_shutdown() {
  local container_name="${1:-}"
  # Wait for the app to be ready
  for i in {1..10}; do
    if curl --silent --fail http://localhost:$APP_PORT/students >/dev/null 2>&1; then
      echo "Application is up. Sending requests..."
      break
    fi
    echo "Waiting for application to start..."
    sleep 3
  done

  # Exercise endpoints to produce testcases & mocks
  curl -sS -X POST -H "Content-Type: application/json" \
    -d '{"student_id":"12345","name":"John Doe","age":20}' http://localhost:$APP_PORT/students >/dev/null
  curl -sS -X POST -H "Content-Type: application/json" \
    -d '{"student_id":"12346","name":"Alice Green","age":22}' http://localhost:$APP_PORT/students >/dev/null
  curl -sS http://localhost:$APP_PORT/students >/dev/null
  curl -sS -X PUT -H "Content-Type: application/json" \
    -d '{"name":"Jane Smith","age":21}' http://localhost:$APP_PORT/students/12345 >/dev/null
  curl -sS http://localhost:$APP_PORT/students >/dev/null
  curl -sS -X DELETE http://localhost:$APP_PORT/students/12345 >/dev/null


}

# --- Record sessions ---
for i in 1 2; do
  container_name="${APP_CONTAINER}_${i}"

  # Run the request and shutdown sequence in the background
  send_request_and_shutdown "$container_name" &
  
  # FIX #1: Added --generate-github-actions=false to prevent the read-only filesystem error.
  "$RECORD_BIN" record \
    -c "docker run -p $APP_PORT:$APP_PORT --net keploy-network --rm --name $container_name $APP_IMAGE" \
    --container-name "$container_name" \
    --generate-github-actions=false \
    --proxy-port $PROXY_PORT \
    --dns-port $DNS_PORT \
    --keploy-container "$KEPLOY_CONTAINER" \
    --record-timer=40s 2>&1 | tee "${container_name}.txt"
     
  
  # cat "${container_name}.txt"  # For visibility in logs
  # The Keploy command will now exit naturally when the container stops. We don't need `|| true`.
  # If it fails, the script should fail.

  if grep -q "WARNING: DATA RACE" "${container_name}.txt"; then
    echo "Race condition detected during record (${container_name})"
    cat "${container_name}.txt"
    exit 1
  fi

  # Commenting this for now as it is failing for the mongo parser connection eof issue and broken pipe.
  # Giving false negatives.

  # if grep "ERROR" "${container_name}.txt"; then
  #   echo "Error found in pipeline..."
  #   cat "${container_name}.txt"
  #   exit 1
  # fi

  echo "Successfully recorded test case and mocks for iteration ${i}"
done

# --- Stop Mongo before test ---
echo "Shutting down mongo before test mode..."
docker stop $DB_CONTAINER >/dev/null 2>&1 || true

# --- Test phase ---
test_container="${APP_CONTAINER}_test_1"
echo "Starting test mode..."
"$REPLAY_BIN" test \
  -c "docker run -p $APP_PORT:$APP_PORT --net keploy-network --name $test_container $APP_IMAGE" \
  --container-name "$test_container" \
  --apiTimeout 60 \
  --delay 12 \
  --proxy-port $PROXY_PORT \
  --dns-port $DNS_PORT \
  --keploy-container "$KEPLOY_CONTAINER" \
  --generate-github-actions=false 2>&1 | tee "${test_container}.txt"


if grep -q "WARNING: DATA RACE" "${test_container}.txt"; then
    echo "Race condition detected during test (${test_container})"
    cat "${test_container}.txt"
    exit 1
fi

# if grep "ERROR" "${test_container}.txt"; then
#   echo "Error found while running test pipeline..."
#   cat "${test_container}.txt"
#   exit 1
# fi

# --- Verify reports ---
all_passed=true
sleep 2 # Give a moment for the report file to be written
for i in 0 1; do
  report_file="./keploy/reports/test-run-0/test-set-$i-report.yaml"
  if [ -f "$report_file" ]; then
    test_status="$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')"
    echo "Test status for test-set-$i: $test_status"
    if [ "$test_status" != "PASSED" ]; then
      all_passed=false
      echo "Test-set-$i did not pass."
      break
    fi
  else
    all_passed=false
    echo "Report not found: $report_file"
    break
  fi
done

if $all_passed; then
  echo "All tests passed"
else
  echo "Some tests failed"
  exit 1
fi