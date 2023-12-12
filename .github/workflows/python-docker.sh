#! /bin/bash

# Start the postgres database.
docker network create backend

# Remove old keploy tests and mocks.
sudo rm -rf keploy/

# Generate the keploy-config file.
docker run --name keploy-v2 -p 16789:16789 --privileged --pid=host  -v "$(pwd)":/files -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock --rm keployv2 generate-config

# Update the global noise to ignore the Allow header.
config_file="./keploy-config.yaml"
sed -i 's/"header": {}/"header":{"Allow":[]}/' "$config_file"

# Wait for 5 seconds for it to complete.
sleep 5

# Start the django-postgres app in record mode and record testcases and mocks.
docker build -t flask-app:1.0 .

for i in {1..2}; do
docker run --name keploy-v2 -p 16789:16789 --privileged --pid=host  -v "$(pwd)":/files -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock --rm keployv2 record -c "docker compose up" --buildDelay 40s  &

# Wait for the application to start.
app_started=false
while [ "$app_started" = false ]; do
    if curl http://localhost:6000/students; then
        app_started=true
    fi
    sleep 3 # wait for 3 seconds before checking again.
done

# Start making curl calls to record the testcases and mocks.
curl -X POST -H "Content-Type: application/json" -d '{"student_id": "12345", "name": "John Doe", "age": 20}' http://localhost:6000/students
curl -X POST -H "Content-Type: application/json" -d '{"student_id": "12346", "name": "Alice Green", "age": 22}' http://localhost:6000/students
curl http://localhost:6000/students
curl -X PUT -H "Content-Type: application/json" -d '{"name": "Jane Smith", "age": 21}' http://localhost:6000/students/12345
curl http://localhost:6000/students
curl -X DELETE http://localhost:6000/students/12345

# Stop the keploy container and the application container.
docker rm -f keploy-v2
docker rm -f flask-app
done

# Start the app in test mode.
docker run --name keploy-v2 -p 16789:16789 --privileged --pid=host -v "$(pwd)":/files -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock --rm keployv2 test -c "docker compose up" --buildDelay 40s --delay 15

# Get the test results from the testReport file.
report_file="./keploy/testReports/report-1.yaml"
test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')

# Return the exit code according to the status.
if [ "$test_status" = "PASSED" ]; then
    exit 0
else
    exit 1
fi

