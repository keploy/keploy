#! /bin/bash

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

# Start the postgres database.
docker network create backend

# Remove old keploy tests and mocks.
rm -rf keploy/

# Generate the keploy-config file.
sudo -E env PATH=$PATH ./../../keployv2 config --generate

# Update the global noise to ignore the Allow header.
config_file="./keploy.yml"
sed -i 's/global: {}/global: {"header": {"Allow":[]}}/' "$config_file"

# Wait for 5 seconds for it to complete.
sleep 5

# Start the django-postgres app in record mode and record testcases and mocks.
docker build -t flask-app:1.0 .

for i in {1..2}; do
sudo -E env PATH=$PATH ./../../keployv2 record -c "docker compose up" --containerName flask-app --buildDelay 40 --generateGithubActions=false  &

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

# wait for 5 seconds for keploy to record.
sleep 5

# Stop the keploy container and the application container.
docker rm -f keploy-v2
docker rm -f flask-app
done

# Start the app in test mode.
sudo -E env PATH=$PATH ./../../keployv2 test -c "docker compose up" --containerName flask-app --buildDelay 40 --apiTimeout 60 --delay 20 --generateGithubActions=false                                     

# Get the test results from the testReport file.
report_file="./keploy/reports/test-run-0/test-set-0-report.yaml"
test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')

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

