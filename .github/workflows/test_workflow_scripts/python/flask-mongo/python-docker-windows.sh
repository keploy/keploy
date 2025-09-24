#!/usr/bin/env bash
set -euo pipefail

SUDO=""
if command -v sudo >/dev/null 2>&1; then SUDO="sudo"; fi

# wrapper to preserve env with/without sudo
run_env() {
  if [ -n "${SUDO}" ]; then
    sudo -E env PATH="$PATH" "$@"
  else
    env PATH="$PATH" "$@"
  fi
}

source "${GITHUB_WORKSPACE}/.github/workflows/test_workflow_scripts/test-iid.sh"

# --- Detect Docker engine ---
ENGINE_OS="$(docker info --format '{{.OSType}}' || echo '')"
echo "Docker engine OSType: ${ENGINE_OS}"
if [[ "${ENGINE_OS}" != "windows" ]]; then
  echo "::error ::This script expects Windows containers. (For Linux engine, use your linux script.)"
  exit 1
fi

echo "Building Windows image with Dockerfile.windows..."
docker build -t flask-app:1.0 -f Dockerfile.windows .

container_kill() {
  pid=$(pgrep -n keploy || true)
  if [ -n "${pid:-}" ]; then
    echo "Killing keploy PID ${pid}"
    # no bare -E here anymore
    ${SUDO:+sudo} kill "${pid}" || true
  fi
}

send_request() {
  local container_name="$1"
  echo "Waiting for ${container_name} on :6000"
  for i in {1..40}; do
    if curl --silent --fail http://localhost:6000/students > /dev/null 2>&1; then
      break
    fi
    sleep 2
  done

  curl -s -X POST -H "Content-Type: application/json" \
    -d '{"student_id":"12345","name":"John Doe","age":20}' http://localhost:6000/students >/dev/null
  curl -s -X POST -H "Content-Type: application/json" \
    -d '{"student_id":"12346","name":"Alice Green","age":22}' http://localhost:6000/students >/dev/null
  curl -s http://localhost:6000/students >/dev/null
  curl -s -X PUT -H "Content-Type: application/json" \
    -d '{"name":"Jane Smith","age":21}' http://localhost:6000/students/12345 >/dev/null
  curl -s http://localhost:6000/students >/dev/null
  curl -s -X DELETE http://localhost:6000/students/12345 >/dev/null

  sleep 5
  container_kill
  wait || true
}

rm -rf keploy/ || true

if [ -f "./keploy.yml" ]; then
  sed -i 's/global: {}/global: {"header":{"Allow":[]}}/' "./keploy.yml" || true
fi

# --- Record twice ---
for i in 1 2; do
  container_name="flaskApp_${i}"
  send_request "${container_name}" &
  # NOTE: no -E at start; goes through run_env
  run_env "$RECORD_BIN" record \
    -c "docker run -p 6000:6000 --rm --name ${container_name} flask-app:1.0" \
    --container-name "${container_name}" &> "${container_name}.txt" || true

  cat "${container_name}.txt"

  if grep -q "ERROR" "${container_name}.txt"; then
    echo "::error ::Error found during recording ${container_name}"
    cat "${container_name}.txt"
    exit 1
  fi
  if grep -q "WARNING: DATA RACE" "${container_name}.txt"; then
    echo "::error ::Race condition during recording ${container_name}"
    cat "${container_name}.txt"
    exit 1
  fi
  echo "Recorded iteration ${i}"
  sleep 3
done

# --- Test (replay) ---
test_container="flaskApp_test"
# map the correct port (6000), not 8080
run_env "$REPLAY_BIN" test \
  -c "docker run -p 6000:6000 --rm --name ${test_container} flask-app:1.0" \
  --containerName "${test_container}" --apiTimeout 60 --delay 20 \
  --generate-github-actions=false &> "${test_container}.txt" || true

cat "${test_container}.txt"

if grep -q "ERROR" "${test_container}.txt"; then
  echo "::error ::Error during test"
  cat "${test_container}.txt"
  exit 1
fi
if grep -q "WARNING: DATA RACE" "${test_container}.txt"; then
  echo "::error ::Race condition during test"
  cat "${test_container}.txt"
  exit 1
fi

all_passed=true
for i in 0 1; do
  report="./keploy/reports/test-run-0/test-set-${i}-report.yaml"
  if [ -f "$report" ]; then
    status=$(grep '^status:' "$report" | awk '{print $2}')
    echo "test-set-$i status: ${status}"
    if [ "$status" != "PASSED" ]; then all_passed=false; break; fi
  fi
done

if $all_passed; then
  echo "All tests passed"
  exit 0
else
  cat "${test_container}.txt" || true
  exit 1
fi
