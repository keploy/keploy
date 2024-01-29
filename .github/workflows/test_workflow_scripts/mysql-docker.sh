#!/bin/bash

# Source the test-iid.sh script
source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

# Create a Docker network for Keploy
docker network create keploy-network

# Start MySQL in Docker
docker run -p 3306:3306 --rm --name mysql -e MYSQL_ROOT_PASSWORD=my-secret-pw -d mysql:latest

# Generate the Keploy configuration file using Docker
docker run --name keploy-v2 -p 16789:16789 --privileged --pid=host -v $(pwd):$(pwd) -w $(pwd) -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock --rm keployv2 generate-config

# Update the global noise to 'ts' in Keploy config
config_file="./keploy-config.yaml"
sed -i 's/body: {}/body: {"ts":[]}/' "$config_file"

# Remove any preexisting Keploy tests and mocks
rm -rf keploy/

# Build the URL shortener Docker image
docker build -t url-short .

# Start Keploy in record mode and capture test cases and mocks
for i in {1..2}; do
    docker run --name keploy-v2 -p 16789:16789 --privileged --pid=host -v $(pwd):$(pwd) -w $(pwd) -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock --rm keployv2 record -c "docker run -p 8080:8080 --name urlshort --rm --network keploy-network url-short:latest" &

    # Wait for the application to start
    app_started=false
    while [ "$app_started" = false ]; do
        if curl localhost:8080/all; then
            app_started=true
        fi
        sleep 3
    done

    # Making curl calls to record test cases and mocks
    curl -X POST localhost:8080/create -d '{"link":"https://google.com"}'
    curl -X POST localhost:8080/create -d '{"link":"https://facebook.com"}'
    curl localhost:8080/all

    # Wait for Keploy to record the test cases and mocks
    sleep 5

    # Stop Keploy
    docker stop keploy-v2
done

# Start Keploy in test mode
docker run --name keploy-v2 -p 16789:16789 --privileged --pid=host -v $(pwd):$(pwd) -w $(pwd) -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock --rm keployv2 test -c "docker run -p 8080:8080 --name urlshort --rm --network keploy-network url-short:latest" --apiTimeout 60 --delay 10

# Get test results from Keploy test reports
report_file="./keploy/testReports/test-run-1/report-1.yaml"
test_status1=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
report_file2="./keploy/testReports/test-run-1/report-2.yaml"
test_status2=$(grep 'status:' "$report_file2" | head -n 1 | awk '{print $2}')

# Return exit code based on test status
if [ "$test_status1" = "PASSED" ] && [ "$test_status2" = "PASSED" ]; then
    exit 0
else
    exit 1
fi
