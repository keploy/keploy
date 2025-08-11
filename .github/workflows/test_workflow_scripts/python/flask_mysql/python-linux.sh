#!/bin/bash
set -Eeuo pipefail

# ──────────────────────────────────────────────────────────────────────────────
# Setup
# ──────────────────────────────────────────────────────────────────────────────
source ../../.github/workflows/test_workflow_scripts/test-iid.sh

# Isolate compose resources for easy teardown
export COMPOSE_PROJECT_NAME="keploy_py_mysql_${RANDOM}"

# Keep track of background pids we spawn
declare -a SEND_REQ_PIDS=()

cleanup() {
  local exit_code=$?
  echo "🧹 Cleaning up (exit code: $exit_code)..."
  set +e

  # Stop any background send_request loops we started
  if [[ "${#SEND_REQ_PIDS[@]}" -gt 0 ]]; then
    for pid in "${SEND_REQ_PIDS[@]}"; do
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
    done
  fi

  # Kill leftover keploy or app processes (best-effort)
  pkill -x keploy 2>/dev/null || true
  pkill -f "python3 demo.py" 2>/dev/null || true

  # Bring down the app stack and remove volumes/orphans
  docker compose down -v --remove-orphans 2>/dev/null || true

  # Stop any stragglers on our network, then remove the network
  if docker network inspect keploy-network >/dev/null 2>&1; then
    docker ps --filter network=keploy-network -q \
      | xargs -r docker stop >/dev/null 2>&1
    docker network rm keploy-network >/dev/null 2>&1 || true
  fi

  echo "✅ Cleanup complete."
  exit "$exit_code"
}
trap cleanup EXIT INT TERM

# ──────────────────────────────────────────────────────────────────────────────
# Networking & dependencies
# ──────────────────────────────────────────────────────────────────────────────
docker network create keploy-network 2>/dev/null || true

# Start the DB/app stack (compose file is expected in this directory)
docker compose up -d

# Install dependencies
pip3 install -r requirements.txt

# Env for the app to connect to the Dockerized MySQL
export DB_HOST=127.0.0.1
export DB_PORT=3306
export DB_USER=demo
export DB_PASSWORD=demopass
export DB_NAME=demo

# Keploy config + reset
sudo "$RECORD_BIN" config --generate
sudo rm -rf keploy/
config_file="./keploy.yml"
sed -i 's/global: {}/global: {"header": {"Allow":[]}}/' "$config_file"
sleep 5

send_request(){
  # Wait for the application to be fully started
  sleep 10
  local app_started=false
  echo "Checking for app readiness on port 5001..."
  while [ "$app_started" = false ]; do
    if curl -s --head http://127.0.0.1:5001/ > /dev/null; then
      app_started=true
      echo "App is ready!"
    fi
    sleep 3
  done

  # 1) Get JWT
  echo "Logging in to get JWT token..."
  TOKEN=$(curl -s -X POST -H "Content-Type: application/json" \
    -d '{"username": "admin", "password": "admin123"}' \
    "http://127.0.0.1:5001/login" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')

  if [ -z "$TOKEN" ]; then
    echo "Failed to retrieve JWT token. Aborting."
    pkill -x keploy 2>/dev/null || true
    exit 1
  fi
  echo "Token received."

  # 2) Exercise APIs to record tests/mocks
  echo "Sending API requests..."
  curl -s -X POST -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" \
    -d '{"name": "Keyboard", "quantity": 50, "price": 75.00, "description": "Mechanical keyboard"}' \
    'http://127.0.0.1:5001/robust-test/create' >/dev/null

  curl -s -X POST -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" \
    -d '{"name": "Webcam", "quantity": 30}' \
    'http://127.0.0.1:5001/robust-test/create-with-null' >/dev/null

  curl -s -H "Authorization: Bearer $TOKEN" \
    'http://127.0.0.1:5001/robust-test/get-all' >/dev/null

  # Give keploy time to write tcs/mocks
  sleep 10
  echo "Killing keploy (recording session) if still running..."
  pkill -x keploy 2>/dev/null || true
}

# ──────────────────────────────────────────────────────────────────────────────
# Record
# ──────────────────────────────────────────────────────────────────────────────
for i in {1..2}; do
  app_name="flask-mysql-app-native-${i}"
  send_request & SEND_REQ_PIDS+=("$!")

  # Run recording with env passed through
  if ! sudo -E env PATH="$PATH" \
      DB_HOST=$DB_HOST DB_PORT=$DB_PORT DB_USER=$DB_USER DB_PASSWORD=$DB_PASSWORD DB_NAME=$DB_NAME \
      "$RECORD_BIN" record -c "python3 demo.py" &> "${app_name}.txt"; then
    echo "record command failed for iteration ${i}"
    cat "${app_name}.txt" || true
    exit 1
  fi

  if grep -q "ERROR" "${app_name}.txt"; then
    echo "Error found in pipeline..."
    cat "${app_name}.txt"
    exit 1
  fi
  if grep -q "WARNING: DATA RACE" "${app_name}.txt"; then
    echo "Race condition detected in recording, stopping pipeline..."
    cat "${app_name}.txt"
    exit 1
  fi

  # Wait for the specific send_request we launched
  wait "${SEND_REQ_PIDS[-1]}"
  echo "Recorded test case and mocks for iteration ${i}"
done

# ──────────────────────────────────────────────────────────────────────────────
# Reset DB before testing
# ──────────────────────────────────────────────────────────────────────────────
echo "Resetting database state for a clean test environment..."
docker compose down
docker compose up -d

# Wait for DB to be ready
echo "Waiting for DB on 127.0.0.1:${DB_PORT}..."
for _ in {1..30}; do
  if nc -z 127.0.0.1 "${DB_PORT}" 2>/dev/null; then
    echo "DB port is open."
    break
  fi
  sleep 2
done
sleep 10

# Ensure tests recorded
if [ ! -d "./keploy/tests" ]; then
  echo "No recorded tests found in ./keploy/tests. Did recording succeed?"
  ls -la ./keploy || true
fi

# ──────────────────────────────────────────────────────────────────────────────
# Test (replay)
# ──────────────────────────────────────────────────────────────────────────────
echo "Starting testing phase..."
set +e
sudo -E env PATH="$PATH" \
  DB_HOST=$DB_HOST DB_PORT=$DB_PORT DB_USER=$DB_USER DB_PASSWORD=$DB_PASSWORD DB_NAME=$DB_NAME \
  "$REPLAY_BIN" test -c "python3 demo.py" --delay 20 &> test_logs.txt
TEST_EXIT=$?
set -e

echo "Keploy test exited with code: $TEST_EXIT"
echo "----- keploy test logs (head) -----"
sed -n '1,200p' test_logs.txt || true
echo "-----------------------------------"

if [ $TEST_EXIT -ne 0 ]; then
  echo "keploy test returned non-zero exit."
fi

if grep -q "ERROR" "test_logs.txt"; then
  echo "Error found in pipeline..."
  cat "test_logs.txt"
  exit 1
fi
if grep -q "WARNING: DATA RACE" "test_logs.txt"; then
  echo "Race condition detected in test, stopping pipeline..."
  cat "test_logs.txt"
  exit 1
fi
