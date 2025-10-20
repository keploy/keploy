#!/bin/bash

source ./../../../.github/workflows/test_workflow_scripts/test-iid.sh

# Creates a collapsible group in the GitHub Actions log
section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

# Checkout to the specified branch
git fetch origin
git checkout native-linux

if [[ "${ENABLE_SSL:-false}" == "false" ]]; then
    echo "Starting Postgres without SSL/TLS"
    docker compose up postgres -d
  else
    echo "Starting postgres with SSL/TLS"
    git checkout enable-ssl-postgres
    docker compose up postgres_ssl -d
    export DB_SSLMODE="require"
    sleep 10
fi

# Install dependencies
pip3 install -r requirements.txt

# Setup environment
export PYTHON_PATH=./venv/lib/python3.10/site-packages/django

# Database migrations
python3 manage.py makemigrations
python3 manage.py migrate

# Configuration and cleanup
sudo $RECORD_BIN config --generate
sudo rm -rf keploy/  # Clean old test data
config_file="./keploy.yml"
sed -i 's/global: {}/global: {"header": {"Allow":[],}}/' "$config_file"
sleep 5  # Allow time for configuration changes

# Waits for an HTTP endpoint to become available
wait_for_http() {
  local host="localhost" # Assuming localhost
  local port="$1"
  echo "Waiting for application on port $port..."
  for i in {1..120}; do
    # Use netcat (nc) to check if the port is open without sending app-level data
    if nc -z "$host" "$port" >/dev/null 2>&1; then
      echo "✅ Application port $port is open."
      endsec
      return 0
    fi
    sleep 1
  done
  echo "::error::Application did not become available on port $port in time."
  return 1
}

send_request(){
    wait_for_http 8000

    # Start making curl calls to record the testcases and mocks.
    curl --location 'http://127.0.0.1:8000/user/' --header 'Content-Type: application/json' --data-raw '{
        "name": "Jane Smith",
        "email": "jane.smith@example.com",
        "password": "smith567",
        "website": "www.janesmith.com"
    }'
    curl --location 'http://127.0.0.1:8000/user/' --header 'Content-Type: application/json' --data-raw '{
        "name": "John Doe",
        "email": "john.doe@example.com",
        "password": "john567",
        "website": "www.johndoe.com"
    }'
    curl --location 'http://127.0.0.1:8000/user/'
}

# Validates the Keploy test report to ensure all test sets passed
check_test_report() {
    echo "Checking test reports..."
    if [ ! -d "./keploy/reports" ]; then
        echo "Test report directory not found!"
        return 1
    fi

    local latest_report_dir
    latest_report_dir=$(ls -td ./keploy/reports/test-run-* | head -n 1)
    if [ -z "$latest_report_dir" ]; then
        echo "No test run directory found in ./keploy/reports/"
        return 1
    fi
    
    local all_passed=true
    # Loop through all generated report files
    for report_file in "$latest_report_dir"/test-set-*-report.yaml; do
        [ -e "$report_file" ] || { echo "No report files found."; all_passed=false; break; }
        
        local test_set_name
        test_set_name=$(basename "$report_file" -report.yaml)
        local test_status
        test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
        
        echo "Status for ${test_set_name}: $test_status"
        if [ "$test_status" != "PASSED" ]; then
            all_passed=false
            echo "Test set ${test_set_name} did not pass."
        fi
    done

    if [ "$all_passed" = false ]; then
        echo "One or more test sets failed."
        return 1
    fi

    echo "All tests passed in reports."
    return 0
}

# Checks a log file for critical errors or data races
check_for_errors() {
  local logfile=$1
  echo "Checking for errors in $logfile..."
  if [ -f "$logfile" ]; then
    # Find critical Keploy errors, but exclude specific non-critical ones.
    if grep "ERROR" "$logfile" | grep "Keploy:" | grep -v "failed to read symbols, skipping coverage calculation"; then
      echo "::error::Critical error found in $logfile. Failing the build."
      # Print the specific errors that caused the failure
      echo "--- Failing Errors ---"
      grep "ERROR" "$logfile" | grep "Keploy:" | grep -v "failed to read symbols, skipping coverage calculation"
      echo "----------------------"
      exit 1
    fi
    if grep -q "WARNING: DATA RACE" "$logfile"; then
      echo "::error::Race condition detected in $logfile"
      exit 1
    fi
  fi
  echo "No critical errors found in $logfile."
}

# Record and Test cycles
for i in {1..2}; do
    section "Start Recording Server"
    app_name="flaskApp_${i}"
    sudo -E env PATH="$PATH" $RECORD_BIN record -c "python3 manage.py runserver" 2>&1 | tee "${app_name}.txt" &
    endsec

    section "Sending Requests for iteration ${i}..."
    send_request
    # allow some time for testcases to be formed
    sleep 10
    endsec

    section "Stop Recording for iteration ${i}..."
    REC_PID="$(pgrep -n -f 'keploy record' || true)"
    echo "$REC_PID Keploy PID"
    echo "Killing keploy"
    sudo kill -INT $REC_PID 2>/dev/null || true
    sleep 10
    check_for_errors "${app_name}.txt"
    echo "Recording stopped."
    endsec
    
    echo "Recorded test case and mocks for iteration ${i}"
done

# Shutdown postgres before test mode - Keploy should use mocks for database interactions
echo "Shutting down postgres before test mode..."
docker compose down -v
echo "Postgres stopped - Keploy should now use mocks for database interactions"

section "Testing phase"
sudo -E env PATH="$PATH" $REPLAY_BIN test -c "python3 manage.py runserver" --delay 10 2>&1 | tee test_logs.txt
check_for_errors "test_logs.txt"
check_test_report
endsec

echo "✅ All tests completed successfully."
exit 0