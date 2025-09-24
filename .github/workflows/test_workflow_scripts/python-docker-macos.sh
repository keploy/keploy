#!/usr/bin/env bash

# macOS variant. Requires Docker Desktop for Mac running.
# Note: BSD sed needs an empty string after -i for in-place edits.

set -euo pipefail

# Isolate keploy home per run to avoid cross-job collisions on a single self-hosted runner
export KEPLOY_HOME_ROOT="${TMPDIR:-/tmp}/keploy-run-${GITHUB_RUN_ID:-$$}-${GITHUB_JOB:-python-docker}-$(date +%s)"
export HOME="$KEPLOY_HOME_ROOT/home"
mkdir -p "$HOME"

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

# --- Networking: create once, quietly ---
if ! docker network ls --format '{{.Name}}' | grep -q '^keploy-network$'; then
  docker network create keploy-network
fi

# --- Start fresh Mongo (force remove any stale one first) ---
docker rm -f mongo >/dev/null 2>&1 || true
docker run --name mongo --rm --net keploy-network -p 27017:27017 -d mongo

# --- Prepare app image & keploy config ---
rm -rf keploy/  # Clean up old test data
docker build -t flask-app:1.0 .

# Safe even if keploy.yml doesn't exist
sed -i '' 's/global: {}/global: {"header": {"Allow":[]}}/' "./keploy.yml" || true
sleep 5

# --- Helpers ---
container_kill() {
  pid="$(pgrep -n keploy || true)"
  if [ -n "${pid}" ]; then
    echo "Killing keploy (pid=${pid})"
    kill "${pid}" || true
    sleep 3
    if kill -0 "${pid}" 2>/dev/null; then
      echo "Forcing kill (SIGKILL) for keploy (pid=${pid})"
      kill -9 "${pid}" || true
    fi
  fi
}

send_request() {
  local container_name="${1:-}"
  sleep 10
  local app_started=false
  while [ "$app_started" = false ]; do
    if curl --silent http://localhost:6000/students >/dev/null 2>&1; then
      app_started=true
    else
      sleep 3
    fi
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

  sleep 5
  container_kill
  wait || true
}

# --- Record sessions ---
for i in 1 2; do
  container_name="flaskApp_${i}"
  send_request "$container_name" &
  "$RECORD_BIN" record \
    -c "docker run -p6000:6000 --net keploy-network --rm --name $container_name flask-app:1.0" \
    --container-name "$container_name" \
    &> "${container_name}.txt" || true

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

  sleep 5
  echo "Recorded test case and mocks for iteration ${i}"
done

# --- Stop Mongo before test (uses --rm so removal is automatic) ---
echo "Shutting down mongo before test mode..."
docker stop mongo >/dev/null 2>&1 || true
echo "MongoDB stopped - Keploy should now use mocks for database interactions"

# --- Test phase ---
test_container="flaskApp_test"
echo "Starting test mode..."
"$REPLAY_BIN" test \
  -c "docker run -p6000:6000 --net keploy-network --name $test_container flask-app:1.0" \
  --container-name "$test_container" \
  --apiTimeout 60 \
  --delay 20 \
  --generate-github-actions=false \
  &> "${test_container}.txt" || true

if grep -q "ERROR" "${test_container}.txt"; then
  echo "Error found in pipeline during test"
  cat "${test_container}.txt"
  exit 1
fi
if grep -q "WARNING: DATA RACE" "${test_container}.txt"; then
  echo "Race condition detected in test"
  cat "${test_container}.txt"
  exit 1
fi

# --- Verify reports (still checking test-run-0 per your original logic) ---
all_passed=true
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

# --- Outcome ---
if [ "$all_passed" = true ]; then
  echo "All tests passed"
  exit 0
else
  cat "${test_container}.txt"
  echo "--- Diagnostics: keploy directory tree (if any) ---"
  if [ -d keploy ]; then
    find keploy -maxdepth 5 -type f -print
  else
    echo "keploy directory not found"
  fi
  echo "--- Diagnostics: docker ps (recent) ---"
  docker ps -a | head -n 20 || true
  echo "--- Diagnostics: container logs ($test_container) ---"
  docker logs "$test_container" 2>&1 | tail -n 200 || true
  exit 1
fi
