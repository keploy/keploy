#!/bin/bash

# Standard Keploy test script pattern
source $GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/test-iid.sh

# Function to cleanup any remaining keploy processes
cleanup_keploy() {
    echo "Cleaning up any remaining keploy processes..."
    local pids=$(pgrep keploy)
    if [ -n "$pids" ]; then
        echo "Found keploy processes: $pids"
        echo "$pids" | xargs -r sudo kill -9 2>/dev/null || true
        sleep 1
        if pgrep keploy >/dev/null; then
            echo "Warning: Some keploy processes may still be running"
            pgrep keploy | xargs -r sudo kill -9 2>/dev/null || true
        else
            echo "All keploy processes cleaned up successfully"
        fi
    else
        echo "No keploy processes found to cleanup"
    fi
}

# Set trap to cleanup on script exit
trap cleanup_keploy EXIT

# Install dependencies if not already present
pip3 install flask

# remove old keploy folder and reports to ensure fresh start
rm -rf keploy/ reports/

# Record Test Cases
echo "Starting Recording with $RECORD_BIN..."
sudo -E env "PATH=$PATH" $RECORD_BIN record -c "python3 app.py" &
KEPLOY_PID=$!

# Wait for app to be ready
sleep 10
until curl -fsS http://127.0.0.1:5000/user/profile >/dev/null; do
    echo "Waiting for app to start..."
    sleep 3
done

# Generate Traffic
echo "Generating Traffic using check-endpoints.py..."
python3 check-endpoints.py
sleep 5 # Allow time for recordings to flush

# Stop Recording gracefully
echo "Stopping record session (PID: $KEPLOY_PID)..."
sudo kill $KEPLOY_PID || true
sleep 5

echo "=== Recorded Data Structure ==="
ls -R keploy/

# Run Schema Match Test
echo "Starting Schema Verification Test with $REPLAY_BIN..."
set +e
OUTPUT=$(sudo -E env "PATH=$PATH" $REPLAY_BIN test -c "python3 app-test.py" --schema-match --delay 10 2>&1)
EXIT_CODE=$?
set -e

echo "=== Test Output ==="
echo "$OUTPUT"

# Verification Logic (7 standard passes + 5 intentional fails)
# Failed test cases:
# - test-4: missing feature_flags
# - test-5: matrix type mismatch
# - test-6: mixed_array hierarchy mismatch
# - test-11: complex/user (id type + missing phone)
# - test-12: complex/product (specs type + dimensions type)
PASS_COUNT=$(echo "$OUTPUT" | grep -c "Testrun passed" || true)
FAIL_COUNT=$(echo "$OUTPUT" | grep -c "Testrun failed" || true)

echo "------------------------------------------------"
echo "Results Summary: $PASS_COUNT PASSED, $FAIL_COUNT FAILED"
echo "Expected Results: 8 PASSED, 5 FAILED"
echo "------------------------------------------------"

if [ "$PASS_COUNT" -eq 8 ] && [ "$FAIL_COUNT" -eq 5 ]; then
    echo "✅ SUCCESS: Schema match logic and side-by-side visualization verified."
    exit 0
else
    echo "❌ FAILURE: Unexpected test results."
    echo "Expected 8 PASSED and 5 FAILED, but got $PASS_COUNT PASSED and $FAIL_COUNT FAILED."
    exit 1
fi
