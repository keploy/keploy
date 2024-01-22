#! /bin/bash

source ./../../../.github/workflows/workflow_scripts/fake-iid.sh

# Checkout a different branch
git fetch origin
git checkout native-linux

# Start postgres instance.
docker run -d -e POSTGRES_USER=petclinic -e POSTGRES_PASSWORD=petclinic -e POSTGRES_DB=petclinic -p 5432:5432 postgres:15.2

# Update the java version
source ./../../../.github/workflows/workflow_scripts/update-java.sh

# Remove any existing test and mocks by keploy.
sudo rm -rf keploy/

# Update the postgres database.
docker cp ./src/main/resources/db/postgresql/initDB.sql mypostgres:/initDB.sql
docker exec mypostgres psql -U petclinic -d petclinic -f /initDB.sql

for i in {1..2}; do
# Start keploy in record mode.
mvn clean install -Dmaven.test.skip=true
sudo -E env PATH=$PATH ./../../../keployv2 record -c 'java -jar target/spring-petclinic-rest-3.0.2.jar' &

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

# Wait for 5 seconds for keploy to stop.
sleep 5

done

# Start keploy in test mode.
sudo -E env PATH=$PATH ./../../../keployv2 test -c 'java -jar target/spring-petclinic-rest-3.0.2.jar' --delay 20

# Get the test results from the testReport file.
report_file="./keploy/testReports/test-run-0/report-1.yaml"
test_status1=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
report_file2="./keploy/testReports/test-run-0/report-2.yaml"
test_status2=$(grep 'status:' "$report_file2" | head -n 1 | awk '{print $2}')

# Return the exit code according to the status.
if [ "$test_status1" = "PASSED" ] && [ "$test_status2" = "PASSED" ]; then
    exit 0
else
    exit 1
fi