#!/bin/bash

# Start Redis
docker compose up -d

# Remove any preexisting keploy tests and mocks.
rm -rf keploy/

# Build the binary.
go build -cover -coverpkg=./... -o sseApp .

for i in {1..2}; do
    app_name="sseApp_${i}"
    "$RECORD_BIN" record -c "./sseApp" 2>&1 | tee "${app_name}.txt" &
    
    KEPLOY_PID=$!

    # Drive traffic via the included script (it blocks until /health is ready)
    chmod +x ./request.sh || true
    bash ./request.sh

    # Wait for 5 seconds for keploy to finish recording the tcs and mocks.
    sleep 5
    REC_PID="$(pgrep -n -f 'keploy record' || true)"
    echo "$REC_PID Keploy PID"
    echo "Killing keploy"
    if [ -n "$REC_PID" ]; then
        sudo kill -INT "$REC_PID" 2>/dev/null || true
    else
        echo "No keploy process found to kill."
    fi

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

# Shutdown Redis before test mode - Keploy should use mocks for database interactions
echo "Shutting down redis before test mode..."
docker compose down || true
echo "Redis stopped - Keploy should now use mocks for database interactions"

# Start the sse_redis app in test mode.
"$REPLAY_BIN" test -c "./sseApp" --delay 7 2>&1 | tee test_logs.txt

cat test_logs.txt || true

# Extract and validate coverage percentage from log
coverage_line=$(grep -Eo "Total Coverage Percentage:[[:space:]]+[0-9]+(\.[0-9]+)?%" "test_logs.txt" | tail -n1 || true)

if [[ -z "$coverage_line" ]]; then
  echo "::error::No coverage percentage found in test_logs.txt"
  cat test_logs.txt
  exit 1
fi

coverage_percent=$(echo "$coverage_line" | grep -Eo "[0-9]+(\.[0-9]+)?" || echo "0")
echo "📊 Extracted coverage: ${coverage_percent}%"

# Compare coverage with threshold (50%)
if (( $(echo "$coverage_percent < 50" | bc -l) )); then
  echo "::error::Coverage below threshold (50%). Found: ${coverage_percent}%"
  cat test_logs.txt
  exit 1
else
  echo "✅ Coverage meets threshold (>= 50%)"
fi

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

# Get the test results from the testReport file.
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

# Check the overall test status and exit accordingly
if [ "$all_passed" = true ]; then
    echo "All tests passed"
    exit 0
else
    cat "test_logs.txt"
    exit 1
fi
