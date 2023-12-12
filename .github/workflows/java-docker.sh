#! /bin/bash

# Checkout the add-petclinic branch.
git fetch origin
git checkout add-petclinic

# Update the java version
source ./../.github/workflows/update-java.sh
mvn --version
cd ./spring-petclinic/spring-petclinic-rest

# Remove any existing test and mocks by keploy.
sudo rm -rf keploy/

# Start keploy in record mode.
docker network create keploy-network
sudo mvn --version
sudo java --version
mvn clean install -Dmaven.test.skip=true
docker run  --name keploy-v2 -p 16789:16789 --privileged --pid=host -v "$(pwd)":/files -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock --rm keployv2 record -c 'docker compose up' --containerName javaApp
sleep 3
docker cp ./src/main/resources/db/postgresql/initDB.sql mypostgres:/initDB.sql
docker exec mypostgres psql -U petclinic -d petclinic -f /initDB.sql

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

# Start keploy in test mode.
sudo docker run  --name keploy-v2 -p 16789:16789 --privileged --pid=host -v "$(pwd)":/files -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock --rm keployv2 test -c './mvnw spring-boot:run' --containerName javaApp --delay 20

# Get the test results from the testReport file.
report_file="./keploy/testReports/report-1.yaml"
test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')

# Return the exit code according to the status.
if [ "$test_status" = "PASSED" ]; then
    exit 0
else
    exit 1
fi