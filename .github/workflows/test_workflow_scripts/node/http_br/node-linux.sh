#!/bin/bash

# Load test scripts and start MongoDB container
source ./../.github/workflows/test_workflow_scripts/test-iid.sh

# Prepare environment
npm install
rm -rf keploy/

# Check if there is a keploy-config file, if there is, delete it.
if [ -f "./keploy.yml" ]; then
    rm ./keploy.yml
fi

echo "Record Bin: $RECORD_BIN"
echo "Record Version:"
sudo $RECORD_BIN --version
echo "Replay Bin: $REPLAY_BIN"
echo "Replay Version:"
sudo $REPLAY_BIN --version

# Generate the keploy-config file.
sudo $RECORD_BIN config --generate

# Update the global noise to ts.
config_file="./keploy.yml"
sed -i 's/global: {}/global: {"header": {"Etag":""}}/' "$config_file"

send_request(){
    node server.js &
    node_pid=$!
    sleep 10
    app_started=false
    while [ "$app_started" = false ]; do
        if curl http://localhost:3000/; then
            app_started=true
        fi
        sleep 3 # wait for 3 seconds before checking again.
    done
    echo "App started"
    # Start making curl calls to record the testcases and mocks.
    curl -v -H "Accept-Encoding: br" -i http://localhost:3000/ --output -
    curl -v -H "Accept-Encoding: gzip" -i http://localhost:3000/ --output -
    curl -v -H "Accept-Encoding: br" -i http://localhost:3000/proxy --output -
    curl -v -H "Accept-Encoding: gzip" -i http://localhost:3000/proxy --output -
    node request.js &
    request_pid=$!
    echo "Node started with pid=$request_pid"
    if [ -z "$request_pid" ]; then
        echo "Failed to capture PID"
        exit 1
    fi
    # Wait for 10 seconds for keploy to record the tcs and mocks.
    sleep 10
    echo "Stopping request.js (pgid: ${request_pid})"
    # if still alive, try graceful TERM on the whole group
    if kill -0 "-${request_pid}" 2>/dev/null; then
         kill -TERM "-${request_pid}" 2>/dev/null || true

        # wait up to 5s for clean exit
        if ! timeout 5s bash -c "while kill -0 -${request_pid} 2>/dev/null; do sleep 0.2; done"; then
            echo "Force killing request.js (pgid: ${request_pid})"
            kill -KILL "-${request_pid}" 2>/dev/null || true
        fi
    fi

    echo "Killing node server.js"
    kill $node_pid
    wait $node_pid 2>/dev/null
    pid=$(pgrep keploy)
    echo "$pid Keploy PID" 
    echo "Killing keploy"
    sudo kill $pid
}

# Record and test sessions in a loop
for i in {1..2}; do
    app_name="nodeApp_${i}"
    send_request &
    sudo -E env PATH=$PATH $RECORD_BIN record -c 'node app.js'    &> "${app_name}.txt"
    cat "${app_name}.txt"
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

# Test modes and result checking
sudo -E env PATH=$PATH $REPLAY_BIN test -c 'node app.js' --delay 10    &> test_logs1.txt
cat test_logs1.txt
if grep "ERROR" "test_logs1.txt"; then
    echo "Error found in pipeline..."
    cat "test_logs1.txt"
    exit 1
fi
if grep "WARNING: DATA RACE" "test_logs1.txt"; then
    echo "Race condition detected in test, stopping pipeline..."
    cat "test_logs1.txt"
    exit 1
fi

sudo -E env PATH=$PATH $REPLAY_BIN test -c 'node app.js' --delay 10 --testsets test-set-0    &> test_logs2.txt
cat test_logs2.txt
if grep "ERROR" "test_logs2.txt"; then
    echo "Error found in pipeline..."
    cat "test_logs2.txt"
    exit 1
fi
if grep "WARNING: DATA RACE" "test_logs2.txt"; then
    echo "Race condition detected in test, stopping pipeline..."
    cat "test_logs2.txt"
    exit 1
fi

sudo -E env PATH=$PATH $REPLAY_BIN test -c 'node app.js' --apiTimeout 30 --delay 10    &> test_logs3.txt
cat test_logs3.txt
if grep "ERROR" "test_logs3.txt"; then
    echo "Error found in pipeline..."
    cat "test_logs3.txt"
    exit 1
fi
if grep "WARNING: DATA RACE" "test_logs3.txt"; then
    echo "Race condition detected in test, stopping pipeline..."
    cat "test_logs3.txt"
    exit 1
fi

all_passed=true

for i in {0..2}
do
    report_file="./keploy/reports/test-run-$i/test-set-0-report.yaml"
    # Extract the test status
    test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')

    # Print the status for debugging
    echo "Test status for test-set-$i: $test_status"

    # Check if any test set did not pass
    if [ "$test_status" != "PASSED" ]; then
        all_passed=false
        echo "Test-set-$i did not pass."
        cat "test_logs${i+1}.txt"
        break # Exit the loop early as all tests need to pass
    fi
done


# Check the overall test status and exit accordingly
if [ "$all_passed" = true ]; then
    report_file="./keploy/reports/test-run-0/test-set-1-report.yaml"
    test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')

    # Print the status for debugging
    echo "Test status for test-set-0: $test_status"

    # Check if any test set did not pass
    if [ "$test_status" != "PASSED" ]; then
        all_passed=false
        echo "Test-set-1 did not pass."
        cat "test_logs1.txt"
        exit 1
    fi
    echo "All tests passed"
    exit 0
else
    exit 1
fi
