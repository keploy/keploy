#!/bin/bash

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

# Checkout a different branch
git fetch origin
git checkout native-linux

# Start mongo before starting keploy.
docker run --rm -d -p27017:27017 --name mongoDb mongo

# Check if there is a keploy-config file, if there is, delete it.
if [ -f "./keploy.yml" ]; then
    rm ./keploy.yml
fi

# Generate the keploy-config file.
sudo ./../../keployv2 config --generate


echo "Keploy config file generated"
echo "listing the files in the current directory"
ls -l

# Update the global noise to ts.
config_file="./keploy.yml"
sed -i 's/global: {}/global: {"body": {"ts":[]}}/' "$config_file"

sed -i 's/ports: 0/ports: 27017/' "$config_file"

# Remove any preexisting keploy tests and mocks.
rm -rf keploy/

# Build the binary.
go build -o ginApp

echo "Binary built successfully and now checking its existence"
ls -l

send_request(){
    sleep 10
    app_started=false
    while [ "$app_started" = false ]; do
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

    # Wait for 10 seconds for keploy to record the tcs and mocks.
    sleep 10
    pid=$(pgrep keploy)
    echo "$pid Keploy PID" 
    echo "Killing keploy"
    sudo kill $pid
}


for i in {1..2}; do
    app_name="javaApp_${i}"
    send_request &
    sudo -E env PATH="$PATH" ./../../keployv2 record -c "./ginApp"   
    # if grep "ERROR" "${app_name}.txt"; then
    #     echo "Error found in pipeline..."
    #     cat "${app_name}.txt"
    #     exit 1
    # fi
    # if grep "WARNING: DATA RACE" "${app_name}.txt"; then
    #   echo "Race condition detected in recording, stopping pipeline..."
    #   cat "${app_name}.txt"
    #   exit 1
    # fi
    sleep 5
    wait
    echo "Recorded test case and mocks for iteration ${i}"
done

# Start the gin-mongo app in test mode.
sudo -E env PATH="$PATH" ./../../keployv2 test -c "./ginApp" --delay 7   

# if grep "ERROR" "test_logs.txt"; then
#     echo "Error found in pipeline..."
#     cat "test_logs.txt"
#     exit 1
# fi

# if grep "WARNING: DATA RACE" "test_logs.txt"; then
#     echo "Race condition detected in test, stopping pipeline..."
#     cat "test_logs.txt"
#     exit 1
# fi

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