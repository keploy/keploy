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
# Arguments: $1: endpoint
record_traffic() {
    local endpoint="$1"
    local url="http://127.0.0.1:3000/${endpoint}"

    echo "⏳ Waiting for application to start..."
    # Wait for the app to be ready by checking for a 200 OK response
    while ! curl -s -o /dev/null -w "%{http_code}" ${url} | grep -E "200|405" > /dev/null; do
        sleep 3
    done
    echo "✅ Application is ready. Sending requests to ${url}"

    # Conditionally send requests based on the endpoint
    if [ "$endpoint" == "large-payload" ]; then
        echo "📦 Generating 500KB payload for POST request..."
        # Create a temporary file with a large JSON body
        local temp_file="large_payload.json"
        echo '{"data":"' > $temp_file
        # Generate ~500KB of characters
        head -c 511980 /dev/zero | tr '\0' 'a' >> $temp_file
        echo '"}' >> $temp_file

        echo "🚀 Sending POST request with 500KB payload..."
        curl -s -o /dev/null -X POST -H "Content-Type: application/json" --data @"$temp_file" ${url}
        
        # Clean up the temporary file
        rm $temp_file
    else
        # For small-payload, send a simple GET request
        echo "🚀 Sending GET request..."
        curl -s -o /dev/null ${url}
    fi
    
    sleep 5

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


# --- Function to run tests and verify results ---
# Arguments: $1: test_log_file
run_and_verify_tests() {
    local test_log_file="$1"

    echo "🚀 Running tests..."
    # Use 'keploy' directly since it's in the system PATH
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
        cat "${test_log_file}"
        exit 1
    fi

    echo "✔️ Tests passed successfully!"
}

# =================================================================
#              MAIN EXECUTION
# =================================================================

# --- STEP 1: Test the Small Payload Endpoint ---
echo "--- 🧪 Starting Test for /small-payload ---"

sudo rm -rf keploy/ reports/
sudo keploy config --generate

echo "🎥 Starting recorder for small payload..."
sudo -E env PATH="$PATH" keploy record -c "node server.js" &> "record_small.txt" &

record_traffic "small-payload"
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

record_traffic "large-payload"
run_and_verify_tests "test_large.txt"

echo "--- ✅ /large-payload Test Completed Successfully ---"
echo ""

# --- FINAL STEP: Conclusion ---
echo "🎉 All tests for all endpoints passed successfully!"
exit 0
