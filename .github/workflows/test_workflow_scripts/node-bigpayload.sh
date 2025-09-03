#!/bin/bash

set -e

API_DIR="samples-typescript/node-bigpayload"

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

    if [ "$endpoint" == "large-payload" ]; then
        echo "📦 Generating 1MB payload for POST requests..."
        local temp_file="large_payload.json"
        echo '{"data":"' > $temp_file
        head -c 10 /dev/zero | tr '\0' 'a' >> $temp_file
        echo '"}' >> $temp_file
    fi

    for (( i=1; i<=num_requests; i++ )); do
        echo "🚀 Sending request ${i}/${num_requests}..."
        if [ "$endpoint" == "large-payload" ]; then
            curl -v -i -X POST -H "Content-Type: application/json" --data @"$temp_file" ${url}
        else
            curl -i ${url}
        fi
        sleep 2
    done

    if [ -f "$temp_file" ]; then
        rm $temp_file
    fi
    
    echo "Waiting for final recordings to complete..."
    sleep 10

    pid=$(pgrep keploy)
    if [ -n "$pid" ]; then
        echo "🛑 Stopping Keploy recorder (PID: $pid)..."
        sudo kill $pid
        echo "Recording stopped."
    else
        echo "⚠️ Keploy recorder process not found."
    fi
}

# Arguments: $1: expected number of tests
verify_test_count() {
    local expected_count="$1"
    local test_dir="./keploy/test-set-0/tests"

    echo "🔎 Verifying number of recorded test cases..."
    
    if [ ! -d "$test_dir" ]; then
        echo "🚨 Test directory ${test_dir} not found! Recording may have failed."
        exit 1
    fi

    local actual_count=$(ls -1 ${test_dir}/test-*.yaml | wc -l)

    echo "Found ${actual_count} recorded test cases. Expected ${expected_count}."

    if [ "$actual_count" -ne "$expected_count" ]; then
        echo "❌ Test case count mismatch! Some tests may have been skipped during recording."
        exit 1
    fi

    echo "✔️ Correct number of test cases recorded."
}


# Arguments: $1: test_log_file
run_and_verify_tests() {
    local test_log_file="$1"

    echo "🚀 Running tests..."
    sudo -E env PATH="$PATH" "$REPLAY_BIN" test -c "node server.js" --delay 10 &> "${test_log_file}" || true

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
sudo "$RECORD_BIN" config --generate
echo "🎥 Starting recorder for small payload..."
sudo -E env PATH="$PATH" "$RECORD_BIN" record -c "node server.js" &> "record_small.txt" &
record_traffic "small-payload" 5
verify_test_count 5
run_and_verify_tests "test_small.txt"
echo "--- ✅ /small-payload Test Completed Successfully ---"
echo ""

# --- STEP 2: Test the Large Payload Endpoint ---
echo "--- 🧪 Starting Test for /large-payload ---"
echo "🧹 Cleaning up for the next test run..."
sudo rm -rf keploy/ reports/
sudo "$RECORD_BIN" config --generate
echo "🎥 Starting recorder for large payload..."
sudo -E env PATH="$PATH" "$RECORD_BIN" record -c "node server.js" --bigPayload &> "record_large.txt" &
record_traffic "large-payload" 5
verify_test_count 5
run_and_verify_tests "test_large.txt"
echo "--- ✅ /large-payload Test Completed Successfully ---"
echo ""

echo "🎉 All tests for all endpoints passed successfully!"
exit 0
