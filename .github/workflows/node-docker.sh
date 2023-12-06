#! /bin/bash

# Start the docker container.
sudo docker run --name mongoDb --rm -p 27017:27017 -d mongo

# Remove any preexisting keploy tests.
sudo rm -rf keploy/

# Edit the connection.js file to connect to local mongodb.
file_path="src/db/connection.js"
sed -i "s/localhost:27017/mongoDb:27017/" "$file_path"

# Build the docker image.
docker build -t node-app:1.0 .

# Remove any existing keploy tests and mocks.
sudo rm -rf keploy/

# Start keploy in record mode.
docker network create keploy-network
docker run  --name keploy-v2 -p 16789:16789 --privileged --pid=host -v "$(pwd)":/files -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock --rm keployv2 record -c "docker run -p 8000:8000 --name nodeMongoApp --network keploy-network node-app:1.0" &

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

# Start keploy in test mode.
docker logs nodeMongoApp
docker ps
docker run  --name keploy-v2 -p 16789:16789 --privileged --pid=host -v "$(pwd)":/files -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock --rm keployv2 test -c "docker run -p 8000:8000 --name nodeMongoApp --network keploy-network node-app:1.0" --delay 30

# Get the test results from the testReport file.
report_file="./keploy/testReports/report-1.yaml"
test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')

# Return the exit code according to the status.
if [ "$test_status" = "PASSED" ]; then
    exit 0
else
    exit 1
fi
