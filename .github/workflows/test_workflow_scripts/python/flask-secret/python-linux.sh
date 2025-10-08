#!/bin/bash

source $GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/test-iid.sh

# Install dependencies
pip3 install -r requirements.txt

sudo rm -rf keploy.yml

# Database migrations
sudo $RECORD_BIN config --generate
sudo rm -rf keploy/  # Clean old test data
sleep 5  # Allow time for configuration changes

send_request(){
    mode="$1"  # "secrets" or "astro"
    sleep 10
    app_started=false
    while [ "$app_started" = false ]; do
        if curl -X GET http://127.0.0.1:8000/; then
            app_started=true
        fi
        sleep 3
    done
    echo "App started"

    if [ "$mode" = "astro" ]; then
        curl -s http://localhost:8000/astro
    else
        curl -s http://localhost:8000/secret1
        curl -s http://localhost:8000/secret2
        curl -s http://localhost:8000/secret3
    fi

    # Wait for keploy to record
    sleep 10
    pid=$(pgrep keploy | head -n 1)
    echo "$pid Keploy PID"
    echo "Killing keploy"
    sudo kill $pid
}

# --- Record cycles for secret endpoints (2 sets, unchanged behavior) ---
for i in 1 2; do
    app_name="flaskSecret_${i}"
    send_request "secrets" &
    sudo -E env PATH="$PATH" $RECORD_BIN record -c "python3 main.py" --metadata "suite=secrets,run=$i" 2>&1 | tee ${app_name}.txt
    if grep "ERROR" "${app_name}.txt"; then
        echo "Error found in pipeline..."
        cat "${app_name}.txt"
        exit 1
    fi
    if grep "WARNING: DATA RACE" "${app_name}.txt"; then
        echo "Race condition detected in recording, stopping pipeline..."
        cat "${app_name}.txt"
        exit 1
    fi
    sleep 5
    wait
    echo "Recorded test case and mocks for iteration ${i}"
done

# Sanitize the testcases
sudo -E env PATH="$PATH" $RECORD_BIN sanitize
sleep 5

# --- Record cycle for the new /astro endpoint (its own test set) ---
app_name="flaskAstro"
send_request "astro" &
sudo -E env PATH="$PATH" $RECORD_BIN record -c "python3 main.py" --metadata "suite=astro,endpoint=/astro" 2>&1 | tee ${app_name}.txt
if grep "ERROR" "${app_name}.txt"; then
    echo "Error found in pipeline..."
    cat "${app_name}.txt"
    exit 1
fi
if grep "WARNING: DATA RACE" "${app_name}.txt"; then
    echo "Race condition detected in recording, stopping pipeline..."
    cat "${app_name}.txt"
    exit 1
fi
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

if grep "ERROR" "test_logs.txt"; then
    echo "Error found in pipeline..."
    cat "test_logs.txt"
    exit 1
fi
if grep "WARNING: DATA RACE" "test_logs.txt"; then
    echo "Race condition detected in test, stopping pipeline..."
    cat "test_logs.txt"
    exit 1
fi

all_passed=true

# We now expect three test sets: test-set-0, test-set-1, test-set-2 (astro)
for i in {0..2}
do
    # Define the report file for each test set
    report_file="./keploy/reports/test-run-0/test-set-$i-report.yaml"
    if [ ! -f "$report_file" ]; then
        echo "Report missing for test-set-$i: $report_file"
        all_passed=false
        break
    fi

    # Extract the test status
    test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')

    # Print the status for debugging
    echo "Test status for test-set-$i: $test_status"

    # Check if any test set did not pass
    if [ "$test_status" != "PASSED" ]; then
        all_passed=false
        echo "Test-set-$i did not pass."
        break # Exit the loop early as all tests need to pass
    fi
done

# Check the overall test status and exit accordingly
if [ "$all_passed" = true ]; then
    echo "All tests passed"
else
    cat "test_logs.txt"
    exit 1
fi

echo "removing main.py and changing temp_main.py to main.py"
rm main.py
mv temp_main.py main.py

echo "remove test-set-2"
rm keploy/test-set-2

echo "running the test again, this will fail as expected and generate the report file"
# run the test again, this will fail as expected and generate the report file
sudo -E env PATH="$PATH" $REPLAY_BIN test -c "python3 main.py" --delay 10 2>&1 | tee test_logs.txt

# run the normalize command 
# now the tests are fixed and we have secrets with updated values
echo "running the normalize command"
sudo -E env PATH="$PATH" $REPLAY_BIN normalize

echo "running the test again, this time it will pass"
# run the test again, this time it will pass
sudo -E env PATH="$PATH" $REPLAY_BIN test -c "python3 main.py" --delay 10 2>&1 | tee test_logs.txt

if grep "ERROR" "test_logs.txt"; then
    echo "Error found in pipeline..."
    cat "test_logs.txt"
    exit 1
fi

if grep "WARNING: DATA RACE" "test_logs.txt"; then
    echo "Race condition detected in test, stopping pipeline..."
    cat "test_logs.txt"
    exit 1
fi

echo "checking the last report file"
report_file="./keploy/reports/test-run-3/test-set-1-report.yaml"
if [ ! -f "$report_file" ]; then
    echo "Report missing for test-set-1: $report_file"
    exit 1
fi

test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
echo "Test status for test-set-1: $test_status"

if [ "$test_status" != "PASSED" ]; then
    echo "Test-set-1 did not pass."
    exit 1
fi

echo "all tests passed"
exit 0