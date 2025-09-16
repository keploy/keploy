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

send_request() {
    # Run 50 test cases per set
    for index in {0..49}; do
        sleep 10
        app_started=false
        while [ "$app_started" = false ]; do
            if curl -X GET http://localhost:8080/api/locations; then
                app_started=true
            fi
            sleep 3
        done
        
        echo "App started for test case $index"
        
        response=$(curl -s -X GET http://localhost:8080/api/locations)

        # Extract any location from the response
        location=$(echo "$response" | jq -r ".location[$index]")
        
        response=$(curl -s -X GET http://localhost:8080/api/locations/$location)

        # Extract any pokemon from the response
        pokemon=$(echo "$response" | jq -r ".[$index]")
        
        curl -s -X GET http://localhost:8080/api/pokemon/$pokemon

        curl -s -X GET http://localhost:8080/api/greet
        curl -s -X GET http://localhost:8080/api/greet?format=html
        curl -s -X GET http://localhost:8080/api/greet?format=xml
    done

    # Wait for Keploy to record the tcs and mocks.
    sleep 10
    pid=$(pgrep keploy)
    echo "$pid Keploy PID"
    echo "Killing Keploy"
    sudo kill $pid
}

# Run 100 test sets
for i in {1..100}; do
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
for i in {0..99}
do
    report_file="./keploy/reports/test-run-0/test-set-$i-report.yaml"

    test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
    echo "Test status for test-set-$i: $test_status"

    if [ "$test_status" != "PASSED" ]; then
        all_passed=false
        echo "Test-set-$i did not pass."
        break
    fi
done

if [ "$all_passed" = true ]; then
    echo "All tests passed"
    exit 0
else
    cat "test_logs.txt"
    exit 1
fi