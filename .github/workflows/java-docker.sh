#! /bin/bash

source ./../.github/workflows/fake-iid.sh

# Checkout the add-petclinic branch.
git fetch origin
git checkout employeem-docker



# Update the java version
source ./../.github/workflows/update-java.sh
mvn --version
cd ./employee-manager

# Start postgres instance.
docker network create keploy-network
docker compose up -d
docker logs mypostgres --follow &

# Remove any existing test and mocks by keploy.
sudo rm -rf keploy/

# Start keploy in record mode.
mvn clean install -Dmaven.test.skip=true
sudo docker build -t java-app:1.0 .
for i in {1..2}; do
docker run  --name keploy-v2 -p 16789:16789 --privileged --pid=host -v "$(pwd)":/files -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock --rm keployv2 record -c 'docker run -p8080:8080 --name javaApp --net keploy-network java-app:1.0'   &

# Wait for the application to start.
app_started=false
while [ "$app_started" = false ]; do
    if curl --location --request GET 'http://localhost:8080/api/employees'; then
        app_started=true
    fi
    sleep 3 # wait for 3 seconds before checking again.
done

# Get the pid of the application.
pid=$(pgrep keploy)

# Start making curl calls to record the testcases and mocks.
curl --location --request POST 'http://localhost:8080/api/employees' \
--header 'Content-Type: application/json' \
--data-raw '{
    "firstName": "Myke",
    "lastName": "Tyson",
    "email": "mt@gmail.com",
    "timestamp":1
}'

curl --location --request GET 'http://localhost:8080/api/employees/1'

curl --location --request POST 'http://localhost:8080/api/employees' \
--header 'Content-Type: application/json' \
--data-raw '{
    "firstName": "John",
    "lastName": "Doe",
    "email": "ok@gmail.com",
    "timestamp":2
}'

curl --location --request GET 'http://localhost:8080/api/employees'

curl -X GET http://localhost:8080/curl --location --request POST 'http://localhost:8080/api/employees' \
--header 'Content-Type: application/json' \
--data-raw '{
    "firstName": "Alice",
    "lastName": "Green",
    "email": "ag@gmail.com",
    "timestamp":3
}'
curl --location --request GET 'http://localhost:8080/api/employees'

# Wait for 5 seconds for keploy to record the tcs and mocks.
sleep 5

# Stop keploy.
sudo docker rm -f keploy-v2
sudo docker rm -f javaApp
done

# Start keploy in test mode.
docker run  --name keploy-v2 -p 16789:16789 --privileged --pid=host -v "$(pwd)":/files -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock --rm keployv2 test -c 'docker run -p 9966:9966 --name javaApp --network keploy-network java-app:1.0' --apiTimeout 30 --delay 30

# Get the test results from the testReport file.
report_file="./keploy/testReports/report-1.yaml"
test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')

# Return the exit code according to the status.
if [ "$test_status" = "PASSED" ]; then
    exit 0
else
    exit 1
fi