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

# Build go binary
go build -o http-pokeapi
echo "go binary built"

# Generate the keploy-config file.
sudo "$RECORD_BIN" config --generate

# Update the global noise to updated_at.
config_file="./keploy.yml"
sed -i 's/global: {}/global: {"body": {"updated_at":[]}}/' "$config_file"

# Wait for a minimum number of test cases to be recorded
wait_for_tests() {
    local min_tests=$1
    local max_wait=${2:-60}
    local waited=0
    
    echo "Waiting for at least $min_tests test(s) to be recorded..."
    
    while [ $waited -lt $max_wait ]; do
        local test_count=0
        if [ -d "./keploy" ]; then
            test_count=$(find ./keploy -name "test-*.yaml" -path "*/tests/*" 2>/dev/null | wc -l | tr -d ' ')
        fi
        
        if [ "$test_count" -ge "$min_tests" ]; then
            echo "Found $test_count test(s) recorded."
            return 0
        fi
        
        echo "Currently $test_count test(s), waiting... ($waited/$max_wait sec)"
        sleep 5
        waited=$((waited + 5))
    done
    
    echo "Timeout waiting for tests. Only found $test_count test(s)."
    return 1
}

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
    
    curl -s -X GET http://localhost:8080/api/pokemon/$pokemon

    curl -s -X GET http://localhost:8080/api/greet

    curl -s -X GET http://localhost:8080/api/greet?format=html

    curl -s -X GET http://localhost:8080/api/greet?format=xml

    # Wait for at least 6 tests to be recorded
    wait_for_tests 6 60
    
    pid=$(pgrep keploy)
    echo "$pid Keploy PID"
    echo "Killing Keploy"
    sudo kill $pid
}

for i in {1..2}; do
    app_name="http-pokeapi_${i}"
    send_request $i &
    sudo -E env PATH="$PATH" "$RECORD_BIN" record -c "./http-pokeapi" --generateGithubActions=false &> "${app_name}.txt"
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
sudo -E env PATH="$PATH" "$REPLAY_BIN" test -c "./http-pokeapi" --delay 7 --generateGithubActions=false &> test_logs.txt


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