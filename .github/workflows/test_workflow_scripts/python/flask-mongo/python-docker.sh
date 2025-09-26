#!/bin/bash
set -Eeuo pipefail

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

# Read the DEBIAN_CODENAME from the environment, with a default value for local testing
DEBIAN_CODENAME=${DEBIAN_CODENAME:-bookworm}
IMAGE_TAG="flask-app:${DEBIAN_CODENAME}"

# Start mongo before starting keploy.
docker network inspect keploy-network >/dev/null 2>&1 || docker network create keploy-network
docker run --name mongo --rm --net keploy-network -p 27017:27017 -d mongo

# Clean up old test data
rm -rf keploy/

# Build app image with dynamic Debian codename
docker build --build-arg DEBIAN_VERSION="${DEBIAN_CODENAME}" -t "${IMAGE_TAG}" .

# Configure keploy (only if config exists)
if [ -f "./keploy.yml" ]; then
  sed -i 's/global: {}/global: {"header": {"Allow":[]}}/' "./keploy.yml" || true
else
  echo "⚠️  keploy.yml not found; skipping sed replace."
fi
sleep 5

container_kill() {
  if pid=$(pgrep -n keploy 2>/dev/null); then
    echo "Killing keploy (pid=${pid})"
    sudo kill "$pid" || true
  fi
}

send_request() {
  local container_name=$1
  sleep 10
  local app_started=false
  while [ "$app_started" = false ]; do
    if curl --silent http://localhost:6000/students >/dev/null; then
      app_started=true
    else
      sleep 3
    fi
  done

  # Record traffic
  curl -sS -X POST -H "Content-Type: application/json" \
    -d '{"student_id": "12345", "name": "John Doe", "age": 20}' http://localhost:6000/students >/dev/null
  curl -sS -X POST -H "Content-Type: application/json" \
    -d '{"student_id": "12346", "name": "Alice Green", "age": 22}' http://localhost:6000/students >/dev/null
  curl -sS http://localhost:6000/students >/dev/null
  curl -sS -X PUT -H "Content-Type: application/json" \
    -d '{"name": "Jane Smith", "age": 21}' http://localhost:6000/students/12345 >/dev/null
  curl -sS http://localhost:6000/students >/dev/null
  curl -sS -X DELETE http://localhost:6000/students/12345 >/dev/null

  sleep 5
  container_kill
  wait || true
}

# Record sessions
for i in {1..2}; do
  container_name="flaskApp_${i}"
  send_request "$container_name" &

  log_file="${container_name}.txt"
  # Stream + save logs; exit code of keploy is preserved by pipefail
  sudo -E env PATH=$PATH \
    "$RECORD_BIN" record \
      -c "docker run -p6000:6000 --net keploy-network --rm --name $container_name ${IMAGE_TAG}" \
      --container-name "$container_name" \
    2>&1 | tee "$log_file"

  # Post-run checks (no extra cat needed because tee already printed logs)
  if grep -q "ERROR" "$log_file"; then
    echo "❌ Error found in recording logs"; exit 1
  fi
  if grep -q "WARNING: DATA RACE" "$log_file"; then
    echo "❌ Data race detected during recording"; exit 1
  fi

  sleep 5
  echo "✅ Recorded test case and mocks for iteration ${i}"
done

echo "Shutting down mongo before test mode..."
docker stop mongo || true
docker rm mongo || true
echo "MongoDB stopped - Keploy should now use mocks for database interactions"

# Testing phase
test_container="flaskApp_test"   # (fixed typo: was 'flashApp_test')
test_log="${test_container}.txt"

sudo -E env PATH=$PATH \
  "$REPLAY_BIN" test \
    -c "docker run -p8080:8080 --net keploy-network --name $test_container ${IMAGE_TAG}" \
    --containerName "$test_container" \
    --apiTimeout 60 \
    --delay 20 \
    --generate-github-actions=false \
  2>&1 | tee "$test_log"

if grep -q "ERROR" "$test_log"; then
  echo "❌ Error found in test logs"; exit 1
fi
if grep -q "WARNING: DATA RACE" "$test_log"; then
  echo "❌ Data race detected during test"; exit 1
fi

all_passed=true
for i in {0..1}; do
  report_file="./keploy/reports/test-run-0/test-set-$i-report.yaml"
  if [ ! -f "$report_file" ]; then
    echo "❌ Missing report: $report_file"
    all_passed=false; break
  fi
  test_status=$(grep 'status:' "$report_file" | head -n1 | awk '{print $2}')
  echo "Test status for test-set-$i: $test_status"
  if [ "$test_status" != "PASSED" ]; then
    all_passed=false; echo "❌ Test-set-$i did not pass."; break
  fi
done

$all_passed && { echo "✅ All tests passed"; exit 0; } || exit 1
