
#!/bin/bash

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

# Start mongo before starting keploy.
docker network create keploy-network
docker run --name mongoDb --rm --net keploy-network -p 27017:27017 -d mongo

# Generate the keploy-config file.
docker run  --name keploy-v2 -p 16789:16789 --privileged --pid=host -v $(pwd):$(pwd) -w $(pwd) -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock --rm keployv2 generate-config

# Update the global noise to ts.
config_file="./keploy-config.yaml"
sed -i 's/body: {}/body: {"ts":[]}/' "$config_file"

# Remove any preexisting keploy tests and mocks.
rm -rf keploy/
docker logs mongoDb &

# Start keploy in record mode.
docker build -t gin-mongo .
for i in {1..2}; do
docker run  --name keploy-v2 -p 16789:16789 --privileged --pid=host -v $(pwd):$(pwd) -w $(pwd) -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock --rm keployv2 record -c 'docker run -p8080:8080 --net keploy-network --rm --name ginApp gin-mongo' &

# Wait for the application to start.
app_started=false
while [ "$app_started" = false ]; do
    if curl -X GET http://localhost:8080/CJBKJd92; then
        app_started=true
    fi
    sleep 3 # wait for 3 seconds before checking again.
done

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

# Stop keploy.
docker stop keploy-v2
docker stop ginApp
done

# Start the keploy in test mode.
docker run  --name keploy-v2 -p 16789:16789 --privileged --pid=host -v $(pwd):$(pwd) -w $(pwd) -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock --rm keployv2 test -c 'docker run -p8080:8080 --net keploy-network --name ginApp gin-mongo' --apiTimeout 60 --delay 10 --generateTestReport

# Get the test results from the testReport file.
report_file="./keploy/testReports/test-run-1/report-1.yaml"
test_status1=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
report_file2="./keploy/testReports/test-run-1/report-2.yaml"
test_status2=$(grep 'status:' "$report_file2" | head -n 1 | awk '{print $2}')

# Return the exit code according to the status.
if [ "$test_status1" = "PASSED" ] && [ "$test_status2" = "PASSED" ]; then
    exit 0
else
    exit 1
fi