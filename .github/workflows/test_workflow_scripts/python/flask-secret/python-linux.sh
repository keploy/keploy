#!/bin/bash

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
        else
            echo "All keploy processes cleaned up successfully"
        fi
    else
        echo "No keploy processes found to cleanup"
    fi
}

# Set trap to cleanup on script exit
trap cleanup_keploy EXIT

# Add coverage to requirements.txt
echo "coverage" >> requirements.txt

# Install dependencies
pip3 install -r requirements.txt

rm -rf keploy.yml

# Database migrations
$RECORD_BIN config --generate
rm -rf keploy/  # Clean old test data
sleep 5  # Allow time for configuration changes

send_request(){
    mode="$1"

    # wait for app to be ready
    sleep 10
    until curl -fsS http://127.0.0.1:8000/health >/dev/null; do
        sleep 3
    done
    echo "App started for mode: $mode"

    if [ "$mode" = "astro" ]; then
        curl -sS http://127.0.0.1:8000/astro >/dev/null
    elif [ "$mode" = "secrets" ]; then
        for ep in secret1 secret2 secret3; do
            echo "GET /$ep"
            curl -sS "http://127.0.0.1:8000/$ep" >/dev/null
        done
    else # This handles the "misc" case
        for ep in jwtlab curlmix cdn; do
            echo "GET /$ep"
            curl -sS "http://127.0.0.1:8000/$ep" >/dev/null
        done
    fi

    # Wait for keploy to flush recordings, then stop it
    sleep 10
    pid=$(pgrep keploy | head -n 1)
    if [ -n "$pid" ]; then
        echo "$pid Keploy PID"
        echo "Killing keploy"
        
        # Try graceful shutdown first
        if ! kill "$pid" 2>/dev/null; then
            echo "Graceful kill failed, trying with sudo..."
            if ! sudo kill "$pid" 2>/dev/null; then
                echo "Sudo kill failed, trying SIGKILL..."
                if ! sudo kill -9 "$pid" 2>/dev/null; then
                    echo "Warning: Could not kill keploy process $pid"
                else
                    echo "Successfully killed keploy with SIGKILL"
                fi
            else
                echo "Successfully killed keploy with sudo"
            fi
        else
            echo "Successfully killed keploy gracefully"
        fi
        
        # Wait a bit and verify the process is gone
        sleep 2
        if pgrep keploy >/dev/null; then
            echo "Warning: Keploy process may still be running"
            pgrep keploy | xargs -r sudo kill -9 2>/dev/null || true
        fi
    else
        echo "No keploy process found to kill"
    fi
}

# --- Record cycles for secret endpoints (2 sets, unchanged behavior) ---
for i in 1 2; do
    app_name="flaskSecret_${i}"
    send_request "secrets" &
    sudo -E env PATH="$PATH" $RECORD_BIN record -c "python3 main.py" --metadata "suite=secrets,run=$i" 2>&1 | tee ${app_name}.txt
    if grep "ERROR" "${app_name}.txt"; then exit 1; fi
    if grep "WARNING: DATA RACE" "${app_name}.txt"; then exit 1; fi
    sleep 5
    wait
    echo "Recorded test case and mocks for iteration ${i}"
done

# Sanitize the testcases
$RECORD_BIN sanitize 2>&1 | tee sanitize_logs.txt
if grep "ERROR" "sanitize_logs.txt"; then exit 1; fi
sleep 5

# --- Record cycle for the new /astro endpoint (its own test set) ---
app_name="flaskAstro"
send_request "astro" &
sudo -E env PATH="$PATH" $RECORD_BIN record -c "python3 main.py" --metadata "suite=astro,endpoint=/astro" 2>&1 | tee ${app_name}.txt
if grep "ERROR" "${app_name}.txt"; then exit 1; fi
if grep "WARNING: DATA RACE" "${app_name}.txt"; then exit 1; fi
sleep 5
wait
echo "Recorded astro test case and mocks"

echo "Shutting down flask before test mode..."

# --- Create secret.yaml in the newly created astro test-set ---
latest_set=$(ls -d ./keploy/test-set-* 2>/dev/null | sort -V | tail -n 1)
if [ -n "$latest_set" ]; then
    echo "Creating secret.yaml in ${latest_set}"
    cat > "${latest_set}/secret.yaml" <<'EOF'
AWS_KEY: xyz
EOF
else
    echo "Could not locate the newly created test-set directory for astro."
    exit 1
fi

# Testing phase
sudo -E env PATH="$PATH" $REPLAY_BIN test -c "python3 main.py" --delay 10 2>&1 | tee test_logs.txt
if grep "ERROR" "test_logs.txt"; then exit 1; fi
if grep "WARNING: DATA RACE" "test_logs.txt"; then exit 1; fi

all_passed=true
# We now expect three test sets: test-set-0, test-set-1, test-set-2 (astro)
for i in {0..2}; do
    report_file="./keploy/reports/test-run-0/test-set-$i-report.yaml"
    if [ ! -f "$report_file" ] || [ "$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')" != "PASSED" ]; then
        all_passed=false
        echo "Test-set-$i did not pass."
        break
    fi
done

if [ "$all_passed" = true ]; then
    echo "All initial tests passed"
else
    cat "test_logs.txt"
    exit 1
fi

# --- CONFIG PATH TEST ---
echo "Testing config path functionality..."

# Create a test config directory
CONFIG_TEST_DIR="./config-test-dir"
mkdir -p "$CONFIG_TEST_DIR"

# Move keploy.yml to the test directory
if [ -f "keploy.yml" ]; then
    mv keploy.yml "$CONFIG_TEST_DIR/"
    echo "Moved keploy.yml to $CONFIG_TEST_DIR"
else
    echo "keploy.yml not found in current directory, cannot test config path functionality"
    exit 1
fi

# Run test with config path pointing to the new location
echo "Running test with --config-path $CONFIG_TEST_DIR"
sudo -E env PATH="$PATH" $REPLAY_BIN test -c "python3 main.py" --delay 10 --config-path "$CONFIG_TEST_DIR" 2>&1 | tee config_path_test_logs.txt

# Check if keploy.yml was created in the original location (should NOT happen)
if [ -f "keploy.yml" ]; then
    echo "ERROR: keploy.yml was incorrectly created in the original location!"
    ls -la keploy.yml
    exit 1
else
    echo "✓ keploy.yml was NOT created in the original location"
fi

# Check if keploy.yml still exists in the moved location
if [ -f "$CONFIG_TEST_DIR/keploy.yml" ]; then
    echo "✓ keploy.yml still exists in the moved location: $CONFIG_TEST_DIR/keploy.yml"
else
    echo "ERROR: keploy.yml is missing from the moved location: $CONFIG_TEST_DIR/keploy.yml"
    exit 1
fi

# Check if the test with config path was successful
if grep "ERROR" "config_path_test_logs.txt"; then
    echo "ERROR: Test with config path failed"
    cat "config_path_test_logs.txt"
    exit 1
fi

echo "Config path test passed successfully!"

# move the config-test-dir to the root of the project
mv $CONFIG_TEST_DIR/* .
rm -rf $CONFIG_TEST_DIR

# --- NORMALIZE WORKFLOW ---
echo "Swapping main.py with temp_main.py for normalize test"
# Save original main.py instead of deleting it
mv main.py original_main.py
mv temp_main.py main.py

echo "Removing astro test-set-2 to focus normalize on secret sets"
rm -rf keploy/test-set-2

echo "Running test again, this will fail as expected"
sudo -E env PATH="$PATH" $REPLAY_BIN test -c "python3 main.py" --delay 10 2>&1 | tee test_logs_fail.txt

echo "Running the normalize command"
sudo -E env PATH="$PATH" $REPLAY_BIN normalize 2>&1 | tee normalize_logs.txt
if grep "ERROR" "normalize_logs.txt"; then exit 1; fi

echo "Running test again, this time it will pass"
sudo -E env PATH="$PATH" $REPLAY_BIN test -c "python3 main.py" --delay 10 2>&1 | tee test_logs_pass.txt
if grep "ERROR" "test_logs_pass.txt"; then exit 1; fi

all_passed=true
for i in {0..1}; do
    report_file="./keploy/reports/test-run-3/test-set-$i-report.yaml"
    if [ ! -f "$report_file" ] || [ "$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')" != "PASSED" ]; then
        all_passed=false
        echo "Test-set-$i did not pass after normalize."
        break
    fi
done

if [ "$all_passed" = true ]; then
    echo "Normalize workflow completed successfully"
else
    cat "test_logs_pass.txt"
    exit 1
fi

# --- FINAL RECORDING AND TESTING (FRESH START) ---

# Restore the original main.py to record new tests against the original application code.
echo "Restoring original application code for new recording session..."
mv main.py temp_main.py      # Move the modified version back to its original filename
mv original_main.py main.py  # Restore the original main.py

# Remove all previous test data to start fresh
echo "Removing all previous test data..."
rm -rf keploy

# Record the "misc" endpoints as a new test suite
app_name="flaskMisc"
send_request "misc" &
sudo -E env PATH="$PATH" $RECORD_BIN record -c "python3 main.py" --metadata "suite=misc" 2>&1 | tee ${app_name}.txt
if grep "ERROR" "${app_name}.txt"; then
    echo "Error found in misc recording..."
    cat "${app_name}.txt"
    exit 1
fi
if grep "WARNING: DATA RACE" "${app_name}.txt"; then
    echo "Race condition detected in misc recording..."
    cat "${app_name}.txt"
    exit 1
fi
sleep 5
wait
echo "Recorded misc test case and mocks"

# Sanitize the newly created test cases
sudo -E env PATH="$PATH" $RECORD_BIN sanitize 2>&1 | tee sanitize_logs_misc.txt
if grep "ERROR" "sanitize_logs_misc.txt"; then
    echo "Error found in misc sanitize..."
    cat "sanitize_logs_misc.txt"
    exit 1
fi

# Final testing phase
echo "Running test on the new 'misc' test suite..."
sudo -E env PATH="$PATH" $REPLAY_BIN test -c "python3 main.py" --delay 10 2>&1 | tee final_test_logs.txt
if grep "ERROR" "final_test_logs.txt"; then
    echo "Error found in final test run..."
    cat "final_test_logs.txt"
    exit 1
fi
if grep "WARNING: DATA RACE" "final_test_logs.txt"; then
    echo "Race condition detected in final test run..."
    cat "final_test_logs.txt"
    exit 1
fi

# Find the most recent test-run dir (don’t assume test-run-0)
RUN_DIR=$(ls -1dt ./keploy/reports/test-run-* 2>/dev/null | head -n1 || true)
if [[ -z "${RUN_DIR:-}" ]]; then
  echo "::error::No test-run directory found under ./keploy/reports"
  [[ $REPLAY_RC -ne 0 ]] && exit "$REPLAY_RC" || exit 1
fi

coverage_file="$RUN_DIR/coverage.yaml"
if [[ -f "$coverage_file" ]]; then
  echo "✅ Coverage file found: $coverage_file"
else
  echo "::error::Coverage file not found in $RUN_DIR"
  return 1
fi

# ✅ Extract and validate coverage percentage from log
coverage_line=$(grep -Eo "Total Coverage Percentage:[[:space:]]+[0-9]+(\.[0-9]+)?%" "test_logs.txt" | tail -n1 || true)

if [[ -z "$coverage_line" ]]; then
  echo "::error::No coverage percentage found in test_logs.txt"
  return 1
fi

coverage_percent=$(echo "$coverage_line" | grep -Eo "[0-9]+(\.[0-9]+)?" || echo "0")
echo "📊 Extracted coverage: ${coverage_percent}%"

# Compare coverage with threshold (50%)
if (( $(echo "$coverage_percent < 50" | bc -l) )); then
  echo "::error::Coverage below threshold (50%). Found: ${coverage_percent}%"
  return 1
else
  echo "✅ Coverage meets threshold (>= 50%)"
fi

# Validate the final test run report. Since we started fresh, there is only one test set.
final_test_passed=true
report_file="./keploy/reports/test-run-0/test-set-0-report.yaml"
echo "Checking final report: $report_file"

if [ ! -f "$report_file" ]; then
    echo "Final report missing: $report_file"
    final_test_passed=false
else
    test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
    echo "Final test status for test-set-0: $test_status"
    if [ "$test_status" != "PASSED" ]; then
        final_test_passed=false
    fi
fi

# Check the overall test status and exit accordingly
if [ "$final_test_passed" = true ]; then
    echo "All final tests passed successfully!"
    exit 0
else
    echo "The final test run failed."
    cat "final_test_logs.txt"
    exit 1
fi
