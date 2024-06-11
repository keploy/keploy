#!/bin/bash

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

# Start mongo before starting keploy.
docker network create keploy-network
docker run --name mongoDb --rm --net keploy-network -p 27017:27017 -d mongo

# Generate the keploy-config file.
sudo -E env PATH=$PATH ./../../keployv2 config --generate

# Update the global noise to ts.
config_file="./keploy.yml"
sed -i 's/global: {}/global: {"body": {"ts":[]}}/' "$config_file"

# Remove any preexisting keploy tests and mocks.
sudo rm -rf keploy/
docker logs mongoDb &> mongo_logs.txt &

# Start keploy in record mode.
docker build -t gin-mongo .
docker rm -f ginApp 2>/dev/null || true

for i in {1..2}; do
    container_name="ginApp_${i}"
    sudo -E env PATH=$PATH ./../../keployv2 record -c "docker run -p8080:8080 --net keploy-network --rm --name ${container_name} gin-mongo" --containerName "${container_name}" --generateGithubActions=false &

    sleep 5

    # Monitor Docker logs in the background and check for race conditions.
    docker logs ${container_name} &> ${container_name}_logs.txt &
    grep -q "race condition detected" ${container_name}_logs.txt && echo "Race condition detected in recording, stopping tests..." && exit 1

    # Wait for the application to start.
    app_started=false
    while [ "$app_started" = false ]; do
        if curl -X GET http://localhost:8080/CJBKJd92; then
            app_started=true
        fi
        sleep 3 # wait for 3 seconds before checking again.
    done

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

    # Wait for 5 seconds for keploy to record the tcs and mocks.
    sleep 5

    # Stop keploy.
    docker rm -f keploy-v2
    docker rm -f "${container_name}"
done

# Start the keploy in test mode.
test_container="ginApp_test"
sudo -E env PATH=$PATH ./../../keployv2 test -c 'docker run -p8080:8080 --net keploy-network --name ginApp_test gin-mongo' --containerName "$test_container" --apiTimeout 60 --delay 20 --generateGithubActions=false &> test_logs.txt

grep -q "race condition detected" test_logs.txt && echo "Race condition detected in testing, stopping tests..." && exit 1

docker rm -f ginApp_test

echo $(ls -a)
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
    exit 1
fi