#! /bin/bash

# Checkout a different branch
git fetch origin
git checkout add_keploy

# Start postgres instance.
 sudo docker run -e POSTGRES_USER=petclinic -e POSTGRES_PASSWORD=petclinic -e POSTGRES_DB=petclinic -p 5432:5432 postgres:15.2

# Update the java version
source ./../.github/workflows/update-java.sh

# Remove any existing test and mocks by keploy.
sudo rm -rf keploy/

# Start keploy in record mode.
sudo -E env PATH=$PATH ./../keployv2 record -c './mvnw spring-boot:run' &

# Wait for the application to start.
app_started=false
while [ "$app_started" = false ]; do
    if curl -X GET http://localhost:9966/petclinic/api/pettypes; then
        app_started=true
    fi
    sleep 3 # wait for 3 seconds before checking again.
done

# Get the pid of the application.
pid=$(pgrep keploy)

# Start making curl calls to record the testcases and mocks.
curl -X GET http://localhost:9966/petclinic/api/pettypes

curl --request POST \
--url http://localhost:9966/petclinic/api/pettypes \
   --header 'content-type: application/json' \
   --data '{
    "name":"John Doe"}'

curl -X GET http://localhost:9966/petclinic/api/pettypes

curl --request POST \
--url http://localhost:9966/petclinic/api/pettypes \
   --header 'content-type: application/json' \
   --data '{
    "name":"Alice Green"}'

curl -X GET http://localhost:9966/petclinic/api/pettypes

 curl --request DELETE \
--url http://localhost:9966/petclinic/api/pettypes/1

curl -X GET http://localhost:9966/petclinic/api/pettypes

# Wait for 5 seconds for keploy to record the tcs and mocks.
sleep 5

# Stop keploy.
sudo kill $pid

# Check all the testcases and mocks recorded by keploy.
cat ./keploy/test-set-0/tests/test-1.yaml ./keploy/test-set-0/tests/test-2.yaml ./keploy/test-set-0/tests/test-3.yaml ./keploy/test-set-0/tests/test-4.yaml ./keploy/test-set-0/tests/test-5.yaml ./keploy/test-set-0/tests/test-6.yaml

# Start keploy in test mode.
sudo -E env PATH=$PATH ./../keployv2 test -c './mvnw spring-boot:run' --delay 20

# Get the test results from the testReport file.
report_file="./keploy/testReports/report-1.yaml"
test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')

# Return the exit code according to the status.
if [ "$test_status" = "PASSED" ]; then
    exit 0
else
    exit 1
fi