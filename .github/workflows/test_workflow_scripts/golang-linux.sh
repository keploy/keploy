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

# Update the global noise to ts.
config_file="./keploy.yml"
sed -i 's/global: {}/global: {"body": {"ts":[]}}/' "$config_file"

sed -i 's/ports: 0/ports: 27017/' "$config_file"

# Remove any preexisting keploy tests and mocks.
rm -rf keploy/

# Build the binary.
go build -o ginApp

for i in {1..2}; do
# Start the gin-mongo app in record mode and record testcases and mocks.
sudo -E env PATH="$PATH" ./../../keployv2 record -c "./ginApp" --generateGithubActions=false &

# Wait for the application to start.
app_started=false

sleep 5

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

# Get the pid of the application.
pid=$(pgrep keploy)

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

# Stop the gin-mongo app.
sudo kill $pid

# Wait for 5 seconds for keploy to stop.
sleep 5
done

# Start the gin-mongo app in test mode.
sudo -E env PATH="$PATH" ./../../keployv2 test -c "./ginApp" --delay 7 --generateGithubActions=false 

# # move keployv2 to /usr/local/bin/keploy
# mv ./../../keployv2 /usr/local/bin/keploy

# sed -i 's/<path for storing stubs>/\/home\/runner\/work\/keploy\/keploy\/samples-go\/gin-mongo/' main_test.go

# # run in mockrecord mode
# go test

# sed -i 's/MOCK_RECORD/MOCK_TEST/' main_test.go
# # run in mocktest mode
# go test

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