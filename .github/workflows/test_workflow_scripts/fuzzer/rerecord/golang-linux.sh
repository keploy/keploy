#!/bin/bash

# --- Script Configuration and Safety ---
set -Eeuo pipefail

# --- Helper Functions for Logging ---
section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

# Error handler for logging context on failure
die() {
  local rc=$?
  echo "::error::Pipeline failed (exit code=$rc). Dumping logs..."
  section "Record Log"
  cat record.log 2>/dev/null || echo "Record log not found."
  endsec
  section "Rerecord Log"
  cat rerecord.log 2>/dev/null || echo "Rerecord log not found."
  endsec
  exit "$rc"
}
trap die ERR

# Waits for an HTTP endpoint to become available
wait_for_http() {
  local host="localhost"
  local port="$1"
  section "Waiting for application on port $port..."
  for i in {1..30}; do
    if nc -z "$host" "$port" >/dev/null 2>&1; then
      echo "✅ Application port $port is open."
      endsec
      return 0
    fi
    echo "Waiting for app... (attempt $i/30)"
    sleep 1
  done
  echo "::error::Application did not become available on port $port in time."
  endsec
  return 1
}

# --- Main Execution Logic ---

export ASSERT_CHAINS_WITH=$(realpath ./fuzzer_chains.yaml)
echo "ASSERT_CHAINS_WITH is set to: $ASSERT_CHAINS_WITH"

if [ -f "./keploy.yml" ]; then
    rm ./keploy.yml
fi

# Generate the keploy-config file using the RECORD binary.
sudo $RECORD_KEPLOY_BIN config --generate

# --- 1. Record Phase ---
section "Start Recording Server"
# Start keploy record in the background using the RECORD binary
sudo -E env PATH="$PATH" $RECORD_KEPLOY_BIN record -c $RERECORD_SERVER_BIN > record.log 2>&1 | tee record.log &
KEPLOY_PID=$!
echo "Keploy record process started with PID: $KEPLOY_PID"
endsec

# --- 2. Generate Traffic Phase ---
section "Generate Fuzzer Traffic"
wait_for_http 8080
# Run fuzzer with reduced calls/time for CI environment
$RERECORD_CLIENT_BIN -url http://localhost:8080 -calls 50 -chaining true -time 1m -output chains.json -nesting 3 -output-yaml fuzzer_chains.yaml
echo "Fuzzer client finished generating traffic."
endsec

sleep 10

# --- 3. Stop Recording ---
section "Stop Recording"
REC_PID="$(pgrep -n -f 'keploy record' || true)"
echo "$REC_PID Keploy PID"
echo "Killing keploy"
if [ -n "$REC_PID" ]; then
    sudo kill -INT "$REC_PID" 2>/dev/null || true
else
    echo "No keploy process found to kill."
fi

# Check recording logs for errors
if grep -i "ERROR" record.log; then
    echo "::error::Error found in recording log."
    cat record.log
    exit 1
fi
endsec

# --- 4. Rerecord Phase ---
section "Start Rerecord"

# Run rerecord non-interactively using the RERECORD binary
printf 'y\ny\n' | sudo -E env PATH="$PATH" ASSERT_CHAINS_WITH="$ASSERT_CHAINS_WITH" \
$RERECORD_KEPLOY_BIN rerecord -c $RERECORD_SERVER_BIN -t "test-set-0" --show-diff --disableMockUpload \
  > rerecord.log 2>&1
RERECORD_RC=$?
cat rerecord.log

# Check rerecord exit code and logs
if [[ $RERECORD_RC -ne 0 ]]; then
  echo "::error::Keploy rerecord process exited with non-zero status: $RERECORD_RC"
  exit $RERECORD_RC
fi

if grep -i "ERROR" rerecord.log; then
    echo "::error::Error found in rerecord log."
    grep -i "ERROR" rerecord.log
    exit 1
fi

# Assert that the chain comparison passed successfully
if ! grep -q "✅ PASSED: Keploy's detected chains match the fuzzer's baseline." rerecord.log; then
  echo "::error::Chain assertion check failed! The fuzzer's baseline did not match Keploy's detected chains."
  exit 1
fi
echo "✅ Chain assertion check passed successfully."
endsec

# --- Final Result ---
echo "✅ Rerecord Fuzzer workflow completed successfully!"
exit 0
