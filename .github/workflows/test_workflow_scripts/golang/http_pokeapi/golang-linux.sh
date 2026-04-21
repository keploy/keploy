#!/bin/bash

echo "$RECORD_BIN"
echo "$REPLAY_BIN"

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh
echo "iid.sh executed"

# Checkout a different branch
git fetch origin
#git checkout native-linux

# Check if there is a keploy-config file, if there is, delete it.
if [ -f "./keploy.yml" ]; then
    rm ./keploy.yml
fi

rm -rf keploy/

# Build go binary.
#
# proxy.golang.org intermittently returns a TLS handshake timeout
# when the WSL/linux runner first reaches it — seen on
# keploy/keploy#4077 run 24631193918/job/72018929505 fetching
# github.com/go-chi/chi@v1.5.5. `go build` has no built-in retry
# for module download, so a single transient DNS/TLS hiccup kills
# the whole job. Wrap with a bounded retry + GOPROXY fallback so
# the flake no longer blocks PRs unrelated to this sample.
build_go_app() {
  local attempt=1
  local max_attempts=4
  local sleep_sec=5
  while [ "$attempt" -le "$max_attempts" ]; do
    if GOPROXY="proxy.golang.org,direct" go build -o http-pokeapi; then
      return 0
    fi
    if [ "$attempt" -ge "$max_attempts" ]; then
      echo "::error::go build for http-pokeapi failed after ${max_attempts} attempts"
      return 1
    fi
    echo "go build attempt ${attempt} failed; retrying in ${sleep_sec}s (attempt $((attempt+1))/${max_attempts})…"
    sleep "$sleep_sec"
    sleep_sec=$((sleep_sec * 2))
    attempt=$((attempt + 1))
  done
}
build_go_app
echo "go binary built"

# Generate the keploy-config file.
sudo "$RECORD_BIN" config --generate

# Update the global noise to updated_at.
config_file="./keploy.yml"
sed -i 's/global: {}/global: {"body": {"updated_at":[]}}/' "$config_file"

send_request() {
    local index=$1  

    sleep 6
    app_started=false
    while [ "$app_started" = false ]; do
        if curl -X GET http://localhost:8080/api/locations; then
            app_started=true
        fi
        sleep 3
    done
    
    echo "App started"
    
    response=$(curl -s -X GET http://localhost:8080/api/locations)

    # Extract any location from the reponse
    location=$(echo "$response" | jq -r ".location[$index]")
    
    response=$(curl -s -X GET http://localhost:8080/api/locations/$location)

    # Extract any pokemon from the response
    pokemon=$(echo "$response" | jq -r ".[$index]")
    
    curl -s -X GET http://localhost:8080/api/greet

    curl -s -X GET http://localhost:8080/api/greet?format=html

    curl -s -X GET http://localhost:8080/api/greet?format=xml

    # Wait for 7 seconds for Keploy to record the tcs and mocks.
    sleep 7
    pid=$(pgrep keploy)
    echo "$pid Keploy PID"
    echo "Killing Keploy"
    sudo kill $pid
}

for i in {1..2}; do
    app_name="http-pokeapi_${i}"
    send_request $i &
    "$RECORD_BIN" record -c "./http-pokeapi" --generateGithubActions=false 2>&1 | tee "${app_name}.txt"
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

# Start the go-http app in test mode.
"$REPLAY_BIN" test -c "./http-pokeapi" --delay 7 --debug --generateGithubActions=false 2>&1 | tee test_logs.txt


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