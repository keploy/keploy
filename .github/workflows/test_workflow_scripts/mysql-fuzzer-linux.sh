#!/usr/bin/env bash

# This script orchestrates the testing of the MySQL fuzzer with Keploy.
# It handles setting up a MySQL instance for recording, generating Keploy tests and mocks,
# and then running tests in a mock environment to validate Keploy's functionality.

# --- Script Configuration and Safety ---
set -Eeuo pipefail

# --- Helper Functions for Logging and Error Handling ---

# Creates a collapsible group in the GitHub Actions log
section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

# Error handler: dumps context and logs before exiting
die() {
  local rc=$?
  echo "::error::Pipeline failed (exit code=$rc). Dumping context..."
  section "Docker Status"
  docker ps -a || true
  endsec
  section "MySQL Container Logs"
  docker logs mysql-container || true
  endsec
  section "Workspace Tree"
  find . -maxdepth 3 -print | sort || true
  endsec
  section "Keploy Artifacts"
  find ./keploy -maxdepth 4 -type f -print 2>/dev/null | sort || true
  endsec
  section "Log Files"
  for f in ./*.txt; do [[ -f "$f" ]] && { echo "--- Log: $f ---"; cat "$f"; }; done
  endsec
  exit "$rc"
}
trap die ERR

# Waits for the MySQL container to become ready and accept connections
wait_for_mysql() {
  section "Waiting for MySQL to become ready..."
  for i in {1..90}; do
    if docker exec mysql-container mysql -uroot -ppassword -e "SELECT 1;" >/dev/null 2>&1; then
      echo "✅ MySQL is ready."
      endsec
      return 0
    fi
    echo "Waiting for MySQL... (attempt $i/90)"
    sleep 1
  done
  echo "::error::MySQL did not become ready in the allotted time."
  endsec
  return 1
}

# Waits for an HTTP endpoint to become available
wait_for_http() {
  local url="$1"
  local port="$2"
  section "Waiting for application on port $port..."
  for i in {1..120}; do
    if curl -fsS "$url" >/dev/null 2>&1; then
      echo "✅ Application is available."
      endsec
      return 0
    fi
    sleep 1
  done
  echo "::error::Application did not become available on port $port in time."
  endsec
  return 1
}

# --- Main Execution Logic ---

# Initial setup and environment logging
section "Initializing Environment"
echo "FUZZER_BIN: $FUZZER_BIN"
echo "RECORD_BIN: $RECORD_BIN"
echo "REPLAY_BIN: $REPLAY_BIN"
rm -rf keploy/ keploy.yml golden/ record.txt test.txt
mkdir -p golden/
endsec

# --- Recording Phase ---
section "Recording with MySQL Fuzzer"
# Start a MySQL instance for the recording session
docker run --name mysql-container \
  -e MYSQL_ROOT_PASSWORD=password \
  -p 3306:3306 --rm -d mysql:8.0
wait_for_mysql

# Generate Keploy configuration and add noise parameter
sudo "$RECORD_BIN" config --generate
sed -i 's/global: {}/global: {"body": {"duration_ms":[]}}/' ./keploy.yml
echo "Keploy config generated and updated."

# Start Keploy recording in the background
echo "Starting Keploy in record mode..."
sudo -E env PATH="$PATH" "$RECORD_BIN" record -c "$FUZZER_BIN" > record.txt 2>&1 &
KEPLOY_PID=$!

# Wait for the fuzzer to start and then trigger it
wait_for_http "http://localhost:18080/run" 18080
echo "Triggering the fuzzer to generate traffic..."
curl -sS --request POST 'http://localhost:18080/run' \
  --header 'Content-Type: application/json' \
  --data-raw '{
    "dsn": "root:password@tcp(127.0.0.1:3306)/",
    "db_name": "fuzzdb",
    "drop_db_first": true,
    "seed": 42,
    "total_ops": 1000,
    "timeout_sec": 1000,
    "mode": "record",
    "golden_path": "./golden/mysqlfuzz_golden.json"
  }'

# Wait for Keploy to finish recording and check its exit status
set +e
wait "$KEPLOY_PID"
RECORD_RC=$?
set -e

# Validate recording logs
if grep -q "ERROR" record.txt; then
  echo "::warning::Errors detected in recording log."
  cat record.txt
fi
if grep -q "WARNING: DATA RACE" record.txt; then
  echo "::error::Data race detected during recording!"
  cat record.txt
  exit 1
fi
if [[ $RECORD_RC -ne 0 ]]; then
  echo "::error::Keploy record process exited with non-zero status: $RECORD_RC"
  exit $RECORD_RC
fi
echo "✅ Recording phase completed successfully."
endsec

# --- Teardown before Replay ---
section "Shutting Down MySQL for Replay"
docker stop mysql-container || true
echo "✅ MySQL container stopped. Replay will rely on Keploy mocks."
endsec

# --- Replay Phase ---
section "Replaying Tests with Keploy Mocks"
# Start Keploy in test mode in the background
echo "Starting Keploy in test mode..."
sudo -E env PATH="$PATH" "$REPLAY_BIN" test -c "$FUZZER_BIN" --delay 15 > test.txt 2>&1 &
KEPLOY_PID=$!

# Wait for the fuzzer to start and trigger its test mode
wait_for_http "http://localhost:18080/run" 18080
echo "Triggering the fuzzer in test mode..."
curl -sS --request POST 'http://localhost:18080/run' \
  --header 'Content-Type: application/json' \
  --data-raw '{
    "dsn": "root:password@tcp(127.0.0.1:3306)/",
    "db_name": "fuzzdb",
    "drop_db_first": true,
    "seed": 42,
    "total_ops": 1000,
    "timeout_sec": 1000,
    "mode": "test",
    "golden_path": "./golden/mysqlfuzz_golden.json"
  }'

# Wait for the replay to complete and capture its exit code
set +e
wait "$KEPLOY_PID"
REPLAY_RC=$?
set -e
echo "Replay finished with exit code: $REPLAY_RC"
cat test.txt
endsec

# --- Verification Phase ---
section "Verifying Test Reports"
# Find the latest test run directory
RUN_DIR=$(ls -1dt ./keploy/reports/test-run-* 2>/dev/null | head -n1 || true)
if [[ -z "${RUN_DIR:-}" ]]; then
  echo "::error::No test run directory found in ./keploy/reports."
  exit 1
fi
echo "Analyzing reports in: $RUN_DIR"

# Check the status of each test set report
all_passed=true
for report in "$RUN_DIR"/test-set-*-report.yaml; do
  [[ -f "$report" ]] || continue
  status=$(awk '/^status:/{print $2; exit}' "$report")
  echo "Status for $(basename "$report"): ${status:-<unknown>}"
  if [[ "$status" != "PASSED" ]]; then
    all_passed=false
  fi
done
endsec

# --- Final Result ---
if [[ "$all_passed" == "true" && $REPLAY_RC -eq 0 ]]; then
  echo "✅ All tests passed successfully!"
  exit 0
else
  echo "::error::One or more tests failed or the replay process exited with an error."
  exit 1
fi