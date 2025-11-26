#!/bin/bash

# This script tests the gRPC proto to JSON conversion feature in Keploy.
# It runs Keploy in test mode on the multi-proto sample application and validates
# that all GRPC_DATA expected values are properly converted to JSON format.

set -Eeuo pipefail

section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

# --- Helper Functions ---

# Checks a log file for critical errors or data races
check_for_errors() {
  local logfile=$1
  echo "Checking for errors in $logfile..."
  if [ -f "$logfile" ]; then
    # Find lines with "ERROR" but exclude non-critical errors if needed
    if grep -q "ERROR" "$logfile"; then
        echo "Error found in $logfile"
        cat "$logfile"
        exit 1
    fi
    if grep -q "WARNING: DATA RACE" "$logfile"; then
      echo "Race condition detected in $logfile"
      cat "$logfile"
      exit 1
    fi
  fi
  echo "No critical errors found in $logfile."
}

# --- Sanity Checks ---
[ -x "${REPLAY_BIN:-}" ] || { echo "REPLAY_BIN not set or not executable"; exit 1; }
command -v go >/dev/null 2>&1 || { echo "go not found"; exit 1; }
command -v python3 >/dev/null 2>&1 || { echo "python3 not found"; exit 1; }

section "🛠️ Setting up environment..."

echo "root ALL=(ALL:ALL) ALL" | sudo tee -a /etc/sudoers

# Navigate to the server directory
cd samples-go/grpc-apps/multi-proto/server

endsec

section "▶️ Running Keploy test mode..."

sudo -E env PATH="$PATH" "$REPLAY_BIN" test -c "go run main.go" --generateGithubActions=false --disableMockUpload 2>&1 | tee test_logs.txt || true

echo "== Test logs =="
cat test_logs.txt

check_for_errors test_logs.txt

endsec

section "🔍 Validating gRPC proto to JSON conversion..."

# Run the validation script and capture both output and exit code
set +e
python3 validate_grpc_data.py 2>&1 | tee validation_output.txt
VALIDATION_EXIT_CODE=$?
set -e

echo "== Validation output =="
cat validation_output.txt

# Check if validation was successful
if [ $VALIDATION_EXIT_CODE -ne 0 ]; then
    echo "❌ Validation script failed with exit code $VALIDATION_EXIT_CODE"
    exit 1
fi

# Verify the success message is present
if ! grep -q "✓ Found valid JSON, feature is working" validation_output.txt; then
    echo "❌ Validation failed: Not all GRPC_DATA values are valid JSON"
    cat validation_output.txt
    exit 1
fi

echo "✅ All GRPC_DATA values are valid JSON - feature is working correctly!"

endsec

echo "✅ gRPC Proto to JSON test passed successfully."
