#!/bin/bash

# Exit immediately if a command exits with a non-zero status.
set -e

# --- Define the working directory for the API ---
API_DIR="samples-typescript/node-bigpayload"

# --- Change to the API directory ---
echo "📂 Changing directory to ${API_DIR}"
cd "${API_DIR}"

# --- Install dependencies for the Node.js app ---
echo "📦 Installing Node.js dependencies..."
npm install

# Arguments: $1: endpoint, $2: number of requests
record_traffic() {
    local endpoint="$1"
    local num_requests="$2"
    local url="http://127.0.0.1:3000/${endpoint}"

    echo "⏳ Waiting for application to start..."
    sleep 30
    echo "✅ Application is ready. Sending ${num_requests} requests to ${url}"

    # Conditionally prepare for requests based on the endpoint
    if [ "$endpoint" == "large-payload" ]; then
        echo "📦 Generating 1MB payload for POST requests..."
        local temp_file="large_payload.json"
        echo '{"data":"' > $temp_file
        head -c 111980 /dev/zero | tr '\0' 'a' >> $temp_file
        echo '"}' >> $temp_file
    fi

    # Loop to send the specified number of requests
    for (( i=1; i<=num_requests; i++ )); do
        echo "🚀 Sending request ${i}/${num_requests}..."
        if [ "$endpoint" == "large-payload" ]; then
            # Send verbose POST requests in the loop
            curl -v -i -X POST -H "Content-Type: application/json" --data @"$temp_file" ${url}
        else
            curl -i ${url}
        fi
        # Wait for 100ms between requests
        sleep 0.2
    done

    # Clean up the temp file if it was created
    if [ -f "$temp_file" ]; then
        rm $temp_file
    fi
    
    # Allow extra time for the last few requests to be processed and recorded
    echo "Waiting for final recordings to complete..."
    sleep 10

    # Gracefully stop the recording process
    pid=$(pgrep keploy)
    if [ -n "$pid" ]; then
        echo "🛑 Stopping Keploy recorder (PID: $pid)..."
        sudo kill $pid
        echo "Recording stopped."
    else
        echo "⚠️ Keploy recorder process not found."
    fi
}

# --- Function to verify the number of recorded test cases ---
# Arguments: $1: expected number of tests
verify_test_count() {
    local expected_count="$1"
    local test_dir="./keploy/test-set-0/tests"

    echo "🔎 Verifying number of recorded test cases..."
    
    if [ ! -d "$test_dir" ]; then
        echo "🚨 Test directory ${test_dir} not found! Recording may have failed."
        exit 1
    fi

    # Count the number of .yaml files in the test directory
    local actual_count=$(ls -1 ${test_dir}/test-*.yaml | wc -l)

    echo "Found ${actual_count} recorded test cases. Expected ${expected_count}."

    if [ "$actual_count" -ne "$expected_count" ]; then
        echo "❌ Test case count mismatch! Some tests may have been skipped during recording."
        exit 1
    fi

    echo "✔️ Correct number of test cases recorded."
}


# --- Function to run tests and verify results ---
# Arguments: $1: test_log_file
run_and_verify_tests() {
    local test_log_file="$1"

    echo "🚀 Running tests..."
    sudo -E env PATH="$PATH" keploy test -c "node server.js" --delay 10 &> "${test_log_file}" || true

    echo "🔍 Checking for errors in test logs..."
    if grep -E "ERROR|WARNING: DATA RACE" "${test_log_file}"; then
        echo "🚨 Error or Data Race detected during testing!"
        cat "${test_log_file}"
        exit 1
    fi

    echo "📊 Verifying test report..."
    local report_file="./keploy/reports/test-run-0/test-set-0-report.yaml"
    if [ ! -f "$report_file" ]; then
        echo "🚨 Test report file not found at ${report_file}!"
        cat "${test_log_file}"
        exit 1
    fi

    local test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
    echo "Test status found: ${test_status}"

    if [ "$test_status" != "PASSED" ]; then
        echo "❌ Tests did not pass. Status: ${test_status}"
        echo "--- Displaying Test Logs (${test_log_file}) ---"
        cat "${test_log_file}"
        echo "--- Displaying Report File (${report_file}) ---"
        cat "${report_file}"
        exit 1
    fi

    echo "✔️ Tests passed successfully!"
}

# --- STEP 1: Test the Small Payload Endpoint ---
echo "--- 🧪 Starting Test for /small-payload ---"
sudo rm -rf keploy/ reports/
sudo keploy config --generate
echo "🎥 Starting recorder for small payload..."
sudo -E env PATH="$PATH" keploy record -c "node server.js" &> "record_small.txt" &
record_traffic "small-payload" 100
verify_test_count 100
run_and_verify_tests "test_small.txt"
echo "--- ✅ /small-payload Test Completed Successfully ---"
echo ""

# --- STEP 2: Test the Large Payload Endpoint ---
echo "--- 🧪 Starting Test for /large-payload ---"
echo "🧹 Cleaning up for the next test run..."
sudo rm -rf keploy/ reports/
sudo keploy config --generate
echo "🎥 Starting recorder for large payload..."
sudo -E env PATH="$PATH" keploy record -c "node server.js" --bigPayload &> "record_large.txt" &
record_traffic "large-payload" 100
verify_test_count 100
run_and_verify_tests "test_large.txt"
echo "--- ✅ /large-payload Test Completed Successfully ---"
echo ""

# --- FINAL STEP: Conclusion ---
echo "🎉 All tests for all endpoints passed successfully!"
exit 0
