#!/bin/bash

# Checkout a different branch
git fetch origin
git checkout fix-gosdk-version

# Start mongo before starting keploy.
sudo docker run --name mongoDb --rm  -p 27017:27017 -d mongo

# Start the gin-mongo app in record mode and record testcases and mocks.
sudo -E env PATH="$PATH" ./../../keployv2 record -c "go run main.go handler.go" &

# Wait for the application to start.
app_started=false
while [ "$app_started" = false ]; do
    if curl -X GET http://localhost:8080/CJBKJd92; then
        app_started=true
    fi
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

# Start the gin-mongo app in test omde.
sudo -E env PATH="$PATH" ./../../keployv2 test -c "go run main.go handler.go" --delay 7

# Wait for 7 seconds for the app to start.
sleep 7

# Wait for around 20 seconds for the test to complete.
sleep 20

# Get the test results from the testReport file.
status = $(cat ./keploy/testReports/report-1.yaml | grep "status" | awk '{print $1}')

# Return the exit code according to the status.
if [ "$status" == "status: PASSED" ]; then
    exit 0
else
    exit 1
fi