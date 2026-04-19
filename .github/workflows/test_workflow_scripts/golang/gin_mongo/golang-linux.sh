#!/bin/bash

# Checkout a different branch
git fetch origin
git checkout native-linux

echo "root ALL=(ALL:ALL) ALL" | sudo tee -a /etc/sudoers

# Start mongo before starting keploy.
docker run --rm -d -p27017:27017 --name mongoDb mongo

# Check if there is a keploy-config file, if there is, delete it.
if [ -f "./keploy.yml" ]; then
    rm ./keploy.yml
fi

# Generate the keploy-config file.
sudo "$RECORD_BIN" config --generate

# Update the global noise to ts.
config_file="./keploy.yml"
sed -i 's/global: {}/global: {"body": {"ts":[]}}/' "$config_file"

sed -i 's/ports: 0/ports: 27017/' "$config_file"

# Remove any preexisting keploy tests and mocks.
rm -rf keploy/

# Build the binary.
go build -cover -coverpkg=./... -o ginApp


send_request(){
    local kp_pid="$1"

    # Bound the readiness loop so a never-starting app (keploy
    # stuck, mongo stuck, docker stuck, anything) fails the
    # iteration in 5 minutes instead of hanging the whole job
    # for hours — see run 24631193918 where this loop was the
    # suspected entry point into a 140-minute gin-mongo hang.
    local deadline=$(( $(date +%s) + 300 ))
    app_started=false
    while [ "$app_started" = false ]; do
        if [ "$(date +%s)" -gt "$deadline" ]; then
            echo "::error::gin-mongo app did not respond on http://localhost:8080 within 5 minutes; aborting iteration"
            return 1
        fi
        if curl --request POST \
      --url http://localhost:8080/url \
      --header 'content-type: application/json' \
      --data '{
      "url": "https://facebook.com"
    }'; then
            app_started=true
        fi
        sleep 3 # wait for 3 seconds before checking again.
    done
    echo "App started"
    # Start making curl calls to record the testcases and mocks.
    curl --request POST \
      --url http://localhost:8080/url \
      --header 'content-type: application/json' \
      --data '{
      "url": "https://google.com"
    }'

    curl --request POST \
      --url http://localhost:8080/url \
      --header 'content-type: application/json' \
      --data '{
      "url": "https://facebook.com"
    }'

    curl -X GET http://localhost:8080/CJBKJd92

    # Test email verification endpoint
    curl --request GET \
      --url 'http://localhost:8080/verify-email?email=test@gmail.com' \
      --header 'Accept: application/json'

    curl --request GET \
      --url 'http://localhost:8080/verify-email?email=admin@yahoo.com' \
      --header 'Accept: application/json'

    # Wait for keploy to record the tcs and mocks.
    sleep 10
    REC_PID="$(pgrep -n -f 'keploy record' || true)"
    echo "$REC_PID Keploy PID"
    echo "Killing keploy"
    if [ -n "$REC_PID" ]; then
        sudo kill -INT "$REC_PID" 2>/dev/null || true
        # Wait for keploy to flush and exit (up to 30s).
        for i in {1..30}; do
            kill -0 "$REC_PID" 2>/dev/null || break
            sleep 1
        done
        # SIGINT-then-SIGKILL escalation. On keploy/keploy#4077 run
        # 24631193918 a single gin-mongo record iteration hung the
        # entire job for 140+ minutes because keploy didn't respond
        # to SIGINT and `wait` at the bottom of the loop blocked
        # forever on its tee'd stdout. Dumping goroutines before
        # SIGKILL gives us the actual RCA on the next hang instead
        # of a blank 6-hour timeout. SIGQUIT is Go-runtime-friendly
        # and prints all goroutine stacks to stderr, which the
        # tee above captures into ${app_name}.txt.
        if kill -0 "$REC_PID" 2>/dev/null; then
            echo "::warning::keploy did not exit within 30s of SIGINT (pid $REC_PID). Dumping goroutines via SIGQUIT, then escalating to SIGKILL."
            sudo kill -QUIT "$REC_PID" 2>/dev/null || true
            sleep 3
            sudo kill -9 "$REC_PID" 2>/dev/null || true
            # Brief pause so the tee pipe has a chance to drain
            # before the outer `wait` returns and the loop moves on.
            sleep 2
        fi
    else
        echo "No keploy process found to kill."
    fi
}


for i in {1..2}; do
    app_name="javaApp_${i}"
    "$RECORD_BIN" record -c "./ginApp" 2>&1 | tee "${app_name}.txt" &
    
    KEPLOY_PID=$!

    # Drive traffic and stop keploy (will fail the pipeline if health never comes up)
    send_request "$KEPLOY_PID"

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

# Keep MongoDB running during test replay. Keploy will serve mocks for
# matched requests; unmatched requests fall through to the real database
# which returns the same data recorded earlier, preventing flaky failures
# caused by non-deterministic mock matching across test sets.

# Start the gin-mongo app in test mode.
"$REPLAY_BIN" test -c "./ginApp" --delay 7   2>&1 | tee test_logs.txt

cat test_logs.txt || true

# ✅ Extract and validate coverage percentage from log
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