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

if [ "$PASS_COUNT" -ne 8 ] || [ "$FAIL_COUNT" -ne 5 ]; then
    echo "❌ FAILURE: Unexpected test results."
    echo "Expected 8 PASSED and 5 FAILED, but got $PASS_COUNT PASSED and $FAIL_COUNT FAILED."
    exit 1
fi
echo "✅ SUCCESS: Schema match logic and side-by-side visualization verified."

# Supplemental json-format pass: re-record + re-run schema-match in a
# scratch directory so the existing 8/5 assertion above is unaffected.
# Gated on both binaries supporting --storage-format json.
# shellcheck disable=SC1091
source "${GITHUB_WORKSPACE:-${PWD%/samples-*}}/.github/workflows/test_workflow_scripts/json-pass-helpers.sh"

if json_pass_supported; then
    echo "Running json-format schema-match pass (scratch path)..."
    rm -rf keploy_json/
    sudo -E env "PATH=$PATH" $RECORD_BIN record --storage-format json --path ./keploy_json -c "python3 app.py" &
    KEPLOY_PID=$!
    sleep 10
    until curl -fsS http://127.0.0.1:5000/user/profile >/dev/null; do
        echo "Waiting for app to start..."
        sleep 3
    done
    python3 check-endpoints.py
    sleep 5
    sudo kill $KEPLOY_PID || true
    sleep 5

    set +e
    OUTPUT_JSON=$(sudo -E env "PATH=$PATH" $REPLAY_BIN test --storage-format json --path ./keploy_json -c "python3 app-test.py" --schema-match --delay 10 2>&1)
    set -e
    PASS_COUNT_JSON=$(echo "$OUTPUT_JSON" | grep -c "Testrun passed" || true)
    FAIL_COUNT_JSON=$(echo "$OUTPUT_JSON" | grep -c "Testrun failed" || true)
    echo "JSON results: $PASS_COUNT_JSON PASSED, $FAIL_COUNT_JSON FAILED (expected 8/5)"
    if [ "$PASS_COUNT_JSON" -ne 8 ] || [ "$FAIL_COUNT_JSON" -ne 5 ]; then
        echo "❌ FAILURE: json-format schema-match counts don't match yaml ($PASS_COUNT_JSON/$FAIL_COUNT_JSON vs 8/5)"
        echo "$OUTPUT_JSON"
        exit 1
    fi
    echo "✅ json-format schema-match counts match yaml."
fi
exit 0
