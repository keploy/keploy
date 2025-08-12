#!/bin/bash

source $GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/test-iid.sh

# Create a shared network for Keploy and the application containers
docker network create keploy-network || true

# Start the database
docker compose up -d

# Install dependencies
pip3 install -r requirements.txt

# Setup environment variables for the application to connect to the Dockerized DB
export DB_HOST=127.0.0.1
export DB_PORT=3306
export DB_USER=demo
export DB_PASSWORD=demopass
export DB_NAME=demo

# Configuration and cleanup
sudo $RECORD_BIN config --generate
sudo rm -rf keploy/  # Clean old test data
config_file="./keploy.yml"

# Idempotently add/update the globalNoise configuration in keploy.yml
# This awk script finds the `test:` block, replaces any existing `globalNoise`
# block within it with the desired configuration. This prevents duplicates.
temp_file=$(mktemp)
sudo awk '
    # state: 0=normal, 1=in_test_block, 2=skipping_old_noise
    BEGIN { state=0 }

    # Rule 1: When we find the "test:" line...
    /^test:/ {
        print                                       # Print "test:"
        print "    globalNoise:"                      # Print our new block
        print "        global:"
        print "            body:"
        print "                access_token: []"
        print "            header:"
        print "                server: []"
        print "        test-sets: {}"
        state=1                                     # Enter state 1 (we are inside the test block)
        next                                        # Skip to the next line of input
    }

    # Rule 2: If we are in state 1 and see the start of an old noise block...
    state==1 && /^\s+globalNoise:/ {
        state=2                                     # Enter state 2 (start skipping)
        next                                        # Skip this line
    }

    # Rule 3: If we are in state 2 (skipping) and the line is still indented...
    state==2 && /^\s{5,}/ {
        next                                        # ...keep skipping.
    }

    # Rule 4: If we are in state 2 and the line is no longer indented enough...
    state==2 && !/^\s{5,}/ {
        state=1                                     # ...the old block is over. Go back to state 1.
    }
    
    # Rule 5: If we are in state 1 and see a non-indented line...
    state==1 && !/^\s+/ {
        state=0                                     # ...we have left the test block. Go back to state 0.
    }

    # Default Action: Print the current line. This runs for any line that was not skipped.
    { print }
' "$config_file" > "$temp_file"
sudo mv "$temp_file" "$config_file"

sleep 5  # Allow time for configuration changes

send_request(){
    # Wait for the application to be fully started
    sleep 10
    app_started=false
    echo "Checking for app readiness on port 5000..."
    while [ "$app_started" = false ]; do
        # App runs on port 5000 as per main.py
        if curl -s --head http://127.0.0.1:5000/health > /dev/null; then
            app_started=true
            echo "App is ready!"
        fi
        sleep 3 # wait for 3 seconds before checking again.
    done
    
    # 1. Login to get the JWT token
    echo "Logging in to get JWT token..."
    TOKEN_RESPONSE=$(curl -s -X POST -H "Content-Type: application/json" \
        -d '{"username": "admin", "password": "admin123"}' \
        "http://127.0.0.1:5000/login")

    TOKEN=$(echo "$TOKEN_RESPONSE" | jq -r .access_token)

    if [ -z "$TOKEN" ] || [ "$TOKEN" == "null" ]; then
        echo "Failed to retrieve JWT token. Response: $TOKEN_RESPONSE"
        pid=$(pgrep keploy)
        sudo kill "$pid"
        exit 1
    fi
    echo "Token received: ${TOKEN:0:20}..."
    
    # 2. Start making curl calls to record the testcases and mocks.
    echo "Sending API requests..."
    
    # Basic data creation (from curls.sh)
    echo "Creating data entry..."
    curl -s -X POST -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" \
        -d '{"message": "hello world"}' \
        'http://127.0.0.1:5000/data'

    # Get all data
    echo "Getting all data..."
    curl -s -H "Authorization: Bearer $TOKEN" 'http://127.0.0.1:5000/data'

    # Complex queries endpoint (from curls.sh)
    echo "Hitting complex queries endpoint..."
    curl -s -H "Authorization: Bearer $TOKEN" 'http://127.0.0.1:5000/generate-complex-queries'

    # System status endpoint
    echo "Getting system status..."
    curl -s -H "Authorization: Bearer $TOKEN" 'http://127.0.0.1:5000/system/status'

    # Migrations endpoint
    echo "Getting migrations..."
    curl -s -H "Authorization: Bearer $TOKEN" 'http://127.0.0.1:5000/system/migrations'

    # Search clients
    echo "Searching clients..."
    curl -s -H "Authorization: Bearer $TOKEN" 'http://127.0.0.1:5000/search/clients?q=Global'

    # Token check
    echo "Checking token..."
    curl -s -H "Authorization: Bearer $TOKEN" 'http://127.0.0.1:5000/auth/check-token/9522d59c56404995af98d4c30bde72b3'

    # Transfer funds (valid transaction)
    echo "Testing fund transfer..."
    curl -s -X POST -H "Content-Type: application/json" -H "Authorization: Bearer $TOKEN" \
        -d '{"from_account_id": 1, "to_account_id": 2, "amount": "100.50"}' \
        'http://127.0.0.1:5000/transactions/transfer'
    
    # Wait for 10 seconds for keploy to record the tcs and mocks.
    sleep 10
    pid=$(pgrep keploy)
    echo "$pid Keploy PID" 
    echo "Killing keploy"
    sudo kill "$pid"
}

# Record cycles
for i in {1..2}; do
    app_name="flask-mysql-app-native-${i}"
    send_request &
    # Pass necessary environment variables to the recording session
    sudo -E env PATH="$PATH" DB_HOST=$DB_HOST DB_PORT=$DB_PORT DB_USER=$DB_USER DB_PASSWORD=$DB_PASSWORD DB_NAME=$DB_NAME $RECORD_BIN record -c "python3 main.py" &> "${app_name}.txt" || true
    if grep "ERROR" "${app_name}.txt"; then
        echo "Error found in recording..."
        cat "${app_name}.txt"
        exit 1
    fi
    sleep 5
    wait # Wait for send_request to finish
    echo "Recorded test case and mocks for iteration ${i}"
done

# Sanity: ensure we actually have recorded tests by checking for test-set-* directories
if [ -z "$(ls -d ./keploy/test-set-* 2>/dev/null)" ]; then
  echo "No recorded test sets (e.g., test-set-0) found in ./keploy/. Did recording succeed?"
  echo "Contents of ./keploy/ directory:"
  ls -la ./keploy || echo "./keploy directory not found."
  exit 1
fi

echo "✅ Sanity check passed. Found recorded test sets."

echo "Starting testing phase with up to 3 attempts..."

for attempt in {1..3}; do
    echo "--- Test Attempt ${attempt}/3 ---"

    # Reset database state for a clean test environment before each attempt
    echo "Resetting database state for attempt ${attempt}..."
    docker compose down
    docker compose up -d

    # Wait for MySQL to be ready
    echo "Waiting for DB on 127.0.0.1:${DB_PORT}..."
    db_ready=false
    for i in {1..30}; do
        if nc -z 127.0.0.1 "${DB_PORT}" 2>/dev/null; then
            echo "DB port is open."
            db_ready=true
            break
        fi
        sleep 2
    done

    if [ "$db_ready" = false ]; then
        echo "DB failed to become ready for attempt ${attempt}. Retrying..."
        continue # Skip to the next attempt
    fi

    sleep 5 # Extra wait time for DB initialization

    # Run the test for the current attempt
    log_file="test_logs_attempt_${attempt}.txt"
    echo "Running Keploy test for attempt ${attempt}, logging to ${log_file}"

    set +e
    sudo -E env PATH="$PATH" \
      DB_HOST=$DB_HOST DB_PORT=$DB_PORT DB_USER=$DB_USER DB_PASSWORD=$DB_PASSWORD DB_NAME=$DB_NAME \
      "$REPLAY_BIN" test -c "python3 main.py" --delay 20 &> "${log_file}"
    TEST_EXIT_CODE=$?
    set -e

    echo "Keploy test (attempt ${attempt}) exited with code: $TEST_EXIT_CODE"
    echo "----- Keploy test logs (attempt ${attempt}) -----"
    cat "${log_file}"
    echo "-------------------------------------------"

    # Check for generic errors in logs first
    if grep -q "ERROR" "${log_file}"; then
        echo "❌ Test Attempt ${attempt} Failed. Found ERROR in logs."
        if [ "$attempt" -lt 5 ]; then
            echo "Retrying..."
            sleep 5
            continue
        else
            break
        fi
    fi
    
    # Check individual test reports for PASSED status
    all_passed_in_attempt=true
    # The recording loop runs twice {1..2}, so we expect test-set-0 and test-set-1
    for i in {0..1}; do
        report_file="./keploy/reports/test-run-0/test-set-$i-report.yaml"

        if [ ! -f "$report_file" ]; then
            echo "Report file not found for test-set-$i. Marking attempt as failed."
            all_passed_in_attempt=false
            break
        fi

        test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
        echo "Test status for test-set-$i: $test_status"

        if [ "$test_status" != "PASSED" ]; then
            all_passed_in_attempt=false
            echo "Test-set-$i did not pass."
            break
        fi
    done

    if [ "$all_passed_in_attempt" = true ]; then
        echo "✅ All tests passed on attempt ${attempt}!"
        docker compose down
        exit 0 # Successful exit from the script
    fi

    # If we reach here, the attempt failed.
    echo "❌ Test Attempt ${attempt} Failed. Not all reports were PASSED."
    if [ "$attempt" -lt 5 ]; then
        echo "Retrying..."
        sleep 5
    fi
done

# If the loop completes, all attempts have failed.
echo "🔴 All 5 test attempts failed. Exiting with failure."
docker compose down
exit 1