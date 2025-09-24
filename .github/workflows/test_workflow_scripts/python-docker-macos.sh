#!/usr/bin/env bash
# macOS variant. Requires Docker Desktop running.
# BSD sed note: use `-i ''` for in-place edits.

set -euo pipefail

# Isolate HOME per run to avoid cross-job collisions on a self-hosted runner
export KEPLOY_HOME_ROOT="${TMPDIR:-/tmp}/keploy-run-${GITHUB_RUN_ID:-$$}-${GITHUB_JOB:-python-docker}-$(date +%s)"
export HOME="$KEPLOY_HOME_ROOT/home"
mkdir -p "$HOME"

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

# Start mongo before starting keploy (auto-removed on stop because of --rm)
docker network create keploy-network >/dev/null 2>&1 || true
docker run --name mongo --rm --net keploy-network -p 27017:27017 -d mongo

# Clean old test data & (re)build app image
rm -rf keploy/
docker build -t flask-app:1.0 .

# Configure keploy (ignore if keploy.yml missing)
sed -i '' 's/global: {}/global: {"header": {"Allow":[]}}/' "./keploy.yml" || true
sleep 3

# Kill a specific PID (not every "keploy" process)
kill_if_running() {
  local pid="${1:-}"
  if [ -n "${pid}" ] && ps -p "${pid}" >/dev/null 2>&1; then
    echo "Killing keploy (pid=${pid})"
    kill "${pid}" || true
    wait "${pid}" || true
  fi
}

send_request(){
  # 1) wait until app is up
  local tries=0
  until curl --silent --fail http://localhost:6000/students >/dev/null 2>&1; do
    tries=$((tries+1))
    if [ $tries -gt 60 ]; then
      echo "App did not become ready on :6000 within timeout"
      return 1
    fi
    sleep 1
  done

  # 2) exercise endpoints to create TCs/mocks
  curl -sS -X POST -H "Content-Type: application/json" \
    -d '{"student_id":"12345","name":"John Doe","age":20}' http://localhost:6000/students >/dev/null
  curl -sS -X POST -H "Content-Type: application/json" \
    -d '{"student_id":"12346","name":"Alice Green","age":22}' http://localhost:6000/students >/dev/null
  curl -sS http://localhost:6000/students >/dev/null
  curl -sS -X PUT -H "Content-Type: application/json" \
    -d '{"name":"Jane Smith","age":21}' http://localhost:6000/students/12345 >/dev/null
  curl -sS http://localhost:6000/students >/dev/null
  curl -sS -X DELETE http://localhost:6000/students/12345 >/dev/null

  # give keploy a moment to flush files
  sleep 5
}

# Record sessions
for i in 1 2; do
  container_name="flaskApp_${i}"

  # Start traffic generator in background
  send_request &
  req_pid=$!

  # Start keploy record (NO sudo to avoid macOS chmod issues)
  "$RECORD_BIN" record \
    -c "docker run --rm -p6000:6000 --net keploy-network --name $container_name flask-app:1.0" \
    --container-name "$container_name" \
    &> "${container_name}.txt" &
  kpid=$!

  # Wait for request generator to complete, then stop keploy record cleanly
  wait "$req_pid" || true
  kill_if_running "$kpid"

  # Basic log checks
  if grep -q "ERROR" "${container_name}.txt"; then
    echo "Error found during record for ${container_name}"
    sed -n '1,200p' "${container_name}.txt"
    exit 1
  fi
  if grep -q "WARNING: DATA RACE" "${container_name}.txt"; then
    echo "Race condition detected during record for ${container_name}"
    sed -n '1,200p' "${container_name}.txt"
    exit 1
  fi

  # Ensure test assets exist after each iteration
  if [ ! -d "./keploy/test-sets" ]; then
    echo "No test-sets created after recording ${container_name}"
    sed -n '1,200p' "${container_name}.txt" || true
    find . -maxdepth 3 -type d -name 'keploy' -print || true
    exit 1
  fi

  echo "Recorded test case and mocks for iteration ${i}"
done

# Shutdown mongo before test mode – keploy should replay using mocks
echo "Shutting down mongo before test mode..."
docker stop mongo >/dev/null 2>&1 || true
# no docker rm needed; --rm already removes it

# Testing phase
test_container="flaskApp_test"
echo "Starting test mode..."

# IMPORTANT: use the correct flag name (--container-name) and avoid sudo
"$REPLAY_BIN" test \
  -c "docker run --rm -p6000:6000 --net keploy-network --name $test_container flask-app:1.0" \
  --container-name "$test_container" \
  --apiTimeout 60 \
  --delay 20 \
  --generate-github-actions=false \
  &> "${test_container}.txt" || true

# Quick sanity check on logs
if grep -q "ERROR" "${test_container}.txt"; then
  echo "Error found in test logs"
  sed -n '1,200p' "${test_container}.txt"
  exit 1
fi
if grep -q "WARNING: DATA RACE" "${test_container}.txt"; then
  echo "Race condition detected in test logs"
  sed -n '1,200p' "${test_container}.txt"
  exit 1
fi

# Find the latest test-run directory dynamically
latest_run="$(ls -1dt ./keploy/reports/test-run-* 2>/dev/null | head -n1 || true)"
if [ -z "${latest_run}" ]; then
  echo "No reports generated (./keploy/reports/test-run-*/ not found)"
  echo "--- keploy test log (tail) ---"
  tail -n 200 "${test_container}.txt" || true
  echo "--- docker ps (recent) ---"
  docker ps -a | head -n 20 || true
  exit 1
fi
echo "Using report directory: ${latest_run}"

all_passed=true
for i in 0 1; do
  report_file="${latest_run}/test-set-${i}-report.yaml"
  if [ ! -f "${report_file}" ]; then
    echo "Report not found: ${report_file}"
    all_passed=false
    break
  fi
  status="$(awk '/^status:/{print $2; exit}' "${report_file}")"
  echo "Test status for test-set-${i}: ${status}"
  if [ "${status}" != "PASSED" ]; then
    all_passed=false
    break
  fi
done

if [ "${all_passed}" = true ]; then
  echo "All tests passed ✅"
  exit 0
else
  echo "Some tests failed ❌"
  echo "--- keploy test log (tail) ---"
  tail -n 200 "${test_container}.txt" || true
  echo "--- keploy tree ---"
  find ./keploy -maxdepth 5 -type f -print || true
  echo "--- docker ps (recent) ---"
  docker ps -a | head -n 20 || true
  echo "--- container logs (${test_container}) ---"
  docker logs "${test_container}" 2>&1 | tail -n 200 || true
  exit 1
fi
