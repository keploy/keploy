#!/usr/bin/env bash

# macOS variant. Requires Docker Desktop for Mac running.
set -euo pipefail


# for the below shource make it such a way that if the file is not present or already present it does not error
source ./../../.github/workflows/test_workflow_scripts/test-iid-macos.sh

# --- Networking: create once, quietly ---
if ! docker network ls --format '{{.Name}}' | grep -q '^keploy-network$'; then
  docker network create keploy-network
fi

# --- Start fresh Mongo (force remove any stale one first) ---
docker rm -f mongo >/dev/null 2>&1 || true
docker run --name mongo --rm --net keploy-network -p 27017:27017 -d mongo

# --- Prepare app image & keploy config ---
rm -rf keploy/  # Clean up old test data

rm ./keploy.yml >/dev/null 2>&1 || true

docker build -t flask-app:1.0 .


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
    if curl --silent --fail http://localhost:6000/students >/dev/null 2>&1; then
      echo "Application is up. Sending requests..."
      break
    fi
    echo "Waiting for application to start..."
    sleep 3
  done

  # Exercise endpoints to produce testcases & mocks
  curl -sS -X POST -H "Content-Type: application/json" \
    -d '{"student_id":"12345","name":"John Doe","age":20}' http://localhost:6000/students >/dev/null
  curl -sS -X POST -H "Content-Type: application/json" \
    -d '{"student_id":"12346","name":"Alice Green","age":22}' http://localhost:6000/students >/dev/null
  curl -sS http://localhost:6000/students >/dev/null
  curl -sS -X PUT -H "Content-Type: application/json" \
    -d '{"name":"Jane Smith","age":21}' http://localhost:6000/students/12345 >/dev/null
  curl -sS http://localhost:6000/students >/dev/null
  curl -sS -X DELETE http://localhost:6000/students/12345 >/dev/null


}

# --- Record sessions ---
for i in 1 2; do
  container_name="flaskApp_${i}"
  
  # Run the request and shutdown sequence in the background
  send_request_and_shutdown "$container_name" &
  
  # FIX #1: Added --generate-github-actions=false to prevent the read-only filesystem error.
  "$RECORD_BIN" record \
    -c "docker run -p6000:6000 --net keploy-network --rm --name $container_name flask-app:1.0" \
    --container-name "$container_name" \
    --generate-github-actions=false \
    --record-timer=10s 2>&1 | tee "${container_name}.txt"
  
    cat "${container_name}.txt"  # For visibility in logs
  # The Keploy command will now exit naturally when the container stops. We don't need `|| true`.
  # If it fails, the script should fail.

  if grep -q "ERROR" "${container_name}.txt"; then
    echo "Error found in pipeline during record (${container_name})"
    cat "${container_name}.txt"
    exit 1
  fi
  if grep -q "WARNING: DATA RACE" "${container_name}.txt"; then
    echo "Race condition detected during record (${container_name})"
    cat "${container_name}.txt"
    exit 1
  fi

  echo "Successfully recorded test case and mocks for iteration ${i}"
done

# --- Stop Mongo before test ---
echo "Shutting down mongo before test mode..."
docker stop mongo >/dev/null 2>&1 || true

# --- Test phase ---
test_container="flaskApp_test"
echo "Starting test mode..."
"$REPLAY_BIN" test \
  -c "docker run -p6000:6000 --net keploy-network --name $test_container flask-app:1.0" \
  --container-name "$test_container" \
  --apiTimeout 60 \
  --delay 12 \
  --generate-github-actions=false 2>&1 | tee "${test_container}.txt"


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
  exit 0
else
  exit 1
fi