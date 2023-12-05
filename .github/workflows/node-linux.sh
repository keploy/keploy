#! /bin/bash

# Start the docker container.
sudo docker run --name mongoDb --rm -p 27017:27017 -d mongo

# Install the required node dependencies.
npm install

# Edit the connection.js file to connect to local mongodb.
file_path="src/db/connection.js"
sed -i "s/mongoDb:27017/localhost:27017/" "$file_path"

# Remove any preexisting keploy tests.
sudo rm -rf keploy/

for i in {1..2}; do
# Start keploy in record mode.
sudo -E env PATH=$PATH ./../../keployv2 record -c 'node src/app.js' &

# Wait for the application to start.
app_started=false
while [ "$app_started" = false ]; do
    if curl -X GET http://localhost:8000/students; then
        app_started=true
    fi
    sleep 3 # wait for 3 seconds before checking again.
done

# Get the pid of the application.
pid=$(pgrep keploy)

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
sudo kill $pid
done

# Start keploy in test mode.
sudo -E env PATH=$PATH ./../../keployv2 test -c 'node src/app.js' --delay 10

# Get the test results from the testReport file.
report_file="./keploy/testReports/report-1.yaml"
test_status1=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
report_file2="./keploy/testReports/report-2.yaml"
test_status2=$(grep 'status:' "$report_file2" | head -n 1 | awk '{print $2}')

# Return the exit code according to the status.
if [ "$test_status1" = "PASSED"] && ["$test_status2" = "PASSED"]; then
    exit 0
else
    exit 1
fi
