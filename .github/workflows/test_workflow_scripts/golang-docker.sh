
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
docker logs mongoDb &

# Start keploy in record mode.
docker build -t gin-mongo .
docker rm -f ginApp 2>/dev/null || true

for i in {1..2}; do
    container_name="ginApp_${i}"
    sudo -E env PATH=$PATH ./../../keployv2 record -c "docker run -p8080:8080 --net keploy-network --rm --name ${container_name} gin-mongo" --containerName "${container_name}" --generateGithubActions=false  &

    sleep 5

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
sudo -E env PATH=$PATH ./../../keployv2 test -c 'docker run -p8080:8080 --net keploy-network --name ginApp_test gin-mongo' --containerName "ginApp_test" --apiTimeout 60 --delay 20 --generateGithubActions=false 
docker rm -f ginApp_test

# Get the test results from the testReport file.
report_file="./keploy/reports/test-run-0/test-set-0-report.yaml"
test_status1=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
report_file2="./keploy/reports/test-run-0/test-set-1-report.yaml"
test_status2=$(grep 'status:' "$report_file2" | head -n 1 | awk '{print $2}')

# Return the exit code according to the status.
if [ "$test_status1" = "PASSED" ] && [ "$test_status2" = "PASSED" ]; then
    exit 0
else
    exit 1
fi
