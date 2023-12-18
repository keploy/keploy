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
sudo -E env PATH=$PATH ./../../keployv2 record -c 'npm start' &

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

# Wait for 5 seconds for keploy to stop.
sleep 5
done

# Start keploy in test mode.
sudo -E env PATH=$PATH ./../../keployv2 test -c 'npm start' --delay 10

sudo -E env PATH=$PATH ./../../keployv2 serve -c "npm test" --delay 5

sudo -E env PATH=$PATH ./../../keployv2 test -c 'npm start' --delay 10 --testsets test-set-0

sed -i "s/mongoDb:27017/localhost:27017/" "$file_path"

# Generate the keploy-config file.
./../../keployv2 generate-config

# Update the global noise to ts.
config_file="./keploy-config.yaml"
sed -i '/tests:/a \        "test-set-0": ["test-1", "test-2", "test-3"]' "keploy-config.yaml"

sudo -E env PATH=$PATH ./../../keployv2 test -c 'npm start' --delay 10

# Get the test results from the testReport file.
report_file="./keploy/testReports/report-1.yaml"
test_status1=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
report_file2="./keploy/testReports/report-2.yaml"
test_status2=$(grep 'status:' "$report_file2" | head -n 1 | awk '{print $2}')
report_file3="./keploy/testReports/report-3.yaml"
test_status3=$(grep 'status:' "$report_file3" | head -n 1 | awk '{print $2}')
report_file4="./keploy/testReports/report-4.yaml"
test_status4=$(grep 'status:' "$report_file4" | head -n 1 | awk '{print $2}')
report_file5="./keploy/testReports/report-5.yaml"
test_status5=$(grep 'status:' "$report_file5" | head -n 1 | awk '{print $2}')
report_file6="./keploy/testReports/report-6.yaml"
test_status6=$(grep 'status:' "$report_file6" | head -n 1 | awk '{print $2}')
report_file7="./keploy/testReports/report-7.yaml"
test_status7=$(grep 'status:' "$report_file7" | head -n 1 | awk '{print $2}')

# Return the exit code according to the status.
if [ "$test_status1" = "PASSED" ] && [ "$test_status2" = "PASSED" ] && [ "$test_status3" = "PASSED" ] && [ "$test_status4" = "PASSED" ] && [ "$test_status5" = "PASSED" ] && [ "$test_status6" = "PASSED" ] && [ "$test_status7" = "PASSED" ] ; then
    exit 0
else
    exit 1
fi
