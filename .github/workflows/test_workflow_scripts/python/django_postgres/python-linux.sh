#!/bin/bash

source ./../../../.github/workflows/test_workflow_scripts/test-iid.sh

echo "root ALL=(ALL:ALL) ALL" | sudo tee -a /etc/sudoers

# Start the postgres database
docker compose up -d

# Install dependencies
python -m pip install -r requirements.txt

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

APP_HOST="127.0.0.1"
APP_PORT="8000"
APP_URL="http://${APP_HOST}:${APP_PORT}"

stop_recording() {
    REC_PID="$(pgrep -n -f "$(basename "${RECORD_BIN:-keploy}") record" || true)"
    if [ -n "$REC_PID" ]; then
        echo "$REC_PID Keploy PID"
        echo "Killing keploy"
        sudo kill -INT "$REC_PID" 2>/dev/null || true
    else
        echo "No keploy recorder process found"
    fi
}

dump_and_force_kill_if_stuck() {
    local record_pid="$1"
    (
        sleep 15
        if [ -n "$record_pid" ] && kill -0 "$record_pid" 2>/dev/null; then
            # Find the keploy agent subprocess (child of the CLI)
            AGENT_PID="$(pgrep -P "$record_pid" -f 'keploy' 2>/dev/null || true)"

            if [ -n "$AGENT_PID" ]; then
                echo "===== KEPLOY AGENT SUBPROCESS HUNG (PID=$AGENT_PID) - DUMPING GOROUTINE STACKS ====="
                sudo kill -QUIT "$AGENT_PID" 2>/dev/null || true
                sleep 3
            fi

            echo "===== KEPLOY CLI HUNG (PID=$record_pid) - DUMPING GOROUTINE STACKS ====="
            sudo kill -QUIT "$record_pid" 2>/dev/null || true
            sleep 3

            # Force kill both if still alive
            if [ -n "$AGENT_PID" ] && kill -0 "$AGENT_PID" 2>/dev/null; then
                sudo kill -9 "$AGENT_PID" 2>/dev/null || true
            fi
            if kill -0 "$record_pid" 2>/dev/null; then
                echo "===== FORCE KILLING KEPLOY ====="
                sudo kill -9 "$record_pid" 2>/dev/null || true
            fi
        fi
    ) &
}

wait_for_app() {
    for attempt in {1..40}; do
        if (exec 3<>"/dev/tcp/${APP_HOST}/${APP_PORT}") 2>/dev/null; then
            echo "App proxy is accepting connections on ${APP_HOST}:${APP_PORT}"
            return 0
        fi
        echo "Waiting for app proxy on ${APP_HOST}:${APP_PORT} (attempt $attempt/40)"
        sleep 3
    done

    echo "::error::App proxy did not become ready on ${APP_HOST}:${APP_PORT} within 120 seconds"
    stop_recording
    dump_and_force_kill_if_stuck "$REC_PID"
    return 1
}

send_request(){
    sleep 10
    wait_for_app || return 1
    # Start making curl calls to record the testcases and mocks.
    curl -fsS --max-time 10 --location "${APP_URL}/user/" --header 'Content-Type: application/json' --data-raw '{
        "name": "Jane Smith",
        "email": "jane.smith@example.com",
        "password": "smith567",
        "website": "www.janesmith.com"
    }' || return 1
    curl -fsS --max-time 10 --location "${APP_URL}/user/" --header 'Content-Type: application/json' --data-raw '{
        "name": "John Doe",
        "email": "john.doe@example.com",
        "password": "john567",
        "website": "www.johndoe.com"
    }' || return 1
    curl -fsS --max-time 10 --location "${APP_URL}/user/" || return 1
    # Wait for 10 seconds for keploy to record the tcs and mocks.
    sleep 10
    stop_recording
    dump_and_force_kill_if_stuck "$REC_PID"
}

do_record_iteration() {
    local i="$1"
    local extra_flags="${2:-}"
    local label="${extra_flags:+_json}"
    local app_name="flaskApp_${i}${label}"
    send_request &
    local request_pid=$!
    # shellcheck disable=SC2086
    $RECORD_BIN record $extra_flags -c "python3 manage.py runserver"   2>&1 | tee "${app_name}.txt"
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
    if ! wait "$request_pid"; then
        echo "::error::Request driver failed while recording iteration ${i}${label:+ json}"
        exit 1
    fi
    echo "Recorded test case and mocks for iteration ${i}${label:+ (json)}"
}

# Record and Test cycles
for i in {1..2}; do
    do_record_iteration "$i"
done

# shellcheck disable=SC1091
source "${GITHUB_WORKSPACE:-${PWD%/samples-*}}/.github/workflows/test_workflow_scripts/json-pass-helpers.sh"

if json_pass_supported; then
    for i in {1..2}; do
        do_record_iteration "$i" "--storage-format json"
    done
fi

# Shutdown postgres before test mode - Keploy should use mocks for database interactions
echo "Shutting down postgres before test mode..."
docker compose down
echo "Postgres stopped - Keploy should now use mocks for database interactions"

# Testing phase
# Django can take longer to boot after the database is stopped and Keploy is
# replaying the recorded Postgres mocks, especially on shared CI runners.
$REPLAY_BIN test -c "python3 manage.py runserver" --delay 20    2>&1 | tee test_logs.txt

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

for i in {0..1}
do
    # Define the report file for each test set
    report_file="./keploy/reports/test-run-0/test-set-$i-report.yaml"

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

if [ "$all_passed" != true ]; then
    cat "test_logs.txt"
    exit 1
fi

if json_pass_supported; then
    $REPLAY_BIN test --storage-format json -c "python3 manage.py runserver" --delay 20    2>&1 | tee test_logs_json.txt
    if grep "ERROR" "test_logs_json.txt"; then
        echo "Error found in json replay..."
        cat "test_logs_json.txt"
        exit 1
    fi
    if grep "WARNING: DATA RACE" "test_logs_json.txt"; then
        echo "Race condition detected in json test..."
        cat "test_logs_json.txt"
        exit 1
    fi
    if ! json_scan_reports; then
        cat test_logs_json.txt
        exit 1
    fi
    echo "All tests passed (yaml + json)"
else
    echo "All tests passed (yaml only — json pass skipped for compat-matrix cell)"
fi
exit 0
