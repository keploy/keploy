#! /bin/bash

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

# Start the docker container.
docker network create keploy-network
docker run --name mongoDb --rm --net keploy-network -p 27017:27017 -d mongo

# Remove any preexisting keploy tests.
sudo rm -rf keploy/

# Build the image of the application.
docker build -t node-app:1.0 .

for i in {1..2}; do
# Start keploy in record mode.
sudo -E env PATH=$PATH ./../../keployv2 record -c "docker run -p 8000:8000 --name nodeMongoApp --network keploy-network node-app:1.0" --containerName nodeMongoApp --generateGithubActions=false &

# Wait for the application to start.
app_started=false
while [ "$app_started" = false ]; do
    if curl -X GET http://localhost:8000/students; then
        app_started=true
    fi
    sleep 3 # wait for 3 seconds before checking again.
done

# Start making curl calls to record the testcases and mocks.
curl --request POST \
--url http://localhost:8000/students \
   --header 'content-type: application/json' \
   --data '{
    "name":"John Do",
    "email":"john@xyiz.com",
    "phone":"0123456799"
    }'

curl --request POST \
--url http://localhost:8000/students \
   --header 'content-type: application/json' \
   --data '{
    "name":"Alice Green",
    "email":"green@alice.com",
    "phone":"3939201584"
    }'

curl -X GET http://localhost:8000/students

# Wait for 5 seconds for keploy to record the tcs and mocks.
sleep 5

# Stop keploy.
docker rm -f keploy-v2
docker rm -f nodeMongoApp
done

# Start keploy in test mode.
sudo -E env PATH=$PATH ./../../keployv2 test -c "docker run -p 8000:8000 --name nodeMongoApp --network keploy-network node-app:1.0" --containerName nodeMongoApp --apiTimeout 30 --delay 30 --generateGithubActions=false 

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
