#!/bin/bash

source ./../../../.github/workflows/test_workflow_scripts/test-iid.sh

# Checkout a different branch
git fetch origin
git checkout native-linux

# Start postgres instance.
docker run -d -e POSTGRES_USER=petclinic -e POSTGRES_PASSWORD=petclinic -e POSTGRES_DB=petclinic -p 5432:5432 --name mypostgres postgres:15.2

# Update the java version
source ./../../../.github/workflows/test_workflow_scripts/update-java.sh

# Remove any existing test and mocks by keploy.
sudo rm -rf keploy/

# Update the postgres database.
docker cp ./src/main/resources/db/postgresql/initDB.sql mypostgres:/initDB.sql
docker exec mypostgres psql -U petclinic -d petclinic -f /initDB.sql

send_request(){
    sleep 10
    app_started=false
    while [ "$app_started" = false ]; do
        if curl -X GET http://localhost:9966/petclinic/api/pettypes; then
            app_started=true
        fi
        sleep 3 # wait for 3 seconds before checking again.
    done
    echo "App started"
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

    # Wait for 10 seconds for keploy to record the tcs and mocks.
    sleep 10
    pid=$(pgrep keploy)
    echo "$pid Keploy PID" 
    echo "Killing keploy"
    sudo kill -9 $(sudo lsof -ti:8086)
}

for i in {1..2}; do
# Start keploy in record mode.
    mvn clean install -Dmaven.test.skip=true
    app_name="javaApp_${i}"
    sudo ./../../../keployv2 agent &
    sleep 5
    send_request &
    sudo -E env PATH=$PATH ./../../../keployv2 record -c 'java -jar target/spring-petclinic-rest-3.0.2.jar'    &> "${app_name}.txt"
    if grep "ERROR" "${app_name}.txt"; then
        echo "Error found in pipeline..."
        cat "${app_name}.txt"
        # exit 1
    fi
    if grep "WARNING: DATA RACE" "${app_name}.txt"; then
        echo "Race condition detected in recording, stopping pipeline..."
        cat "${app_name}.txt"
        # exit 1
    fi
    sleep 5
    wait
    echo "Recorded test case and mocks for iteration ${i}"
done

sudo ./../../../keployv2 agent &
sleep 5
# Start keploy in test mode.
sudo -E env PATH=$PATH ./../../../keployv2 test -c 'java -jar target/spring-petclinic-rest-3.0.2.jar' --delay 20    &> test_logs.txt
if grep "ERROR" "test_logs.txt"; then
    echo "Error found in pipeline..."
    cat "test_logs.txt"
    # exit 1
fi
if grep "WARNING: DATA RACE" "test_logs.txt"; then
    echo "Race condition detected in test, stopping pipeline..."
    cat "test_logs.txt"
    # exit 1
fi

all_passed=true

# Get the test results from the testReport file.
for i in {0..1}
do
    # Define the report file for each test set
    report_file="./keploy/reports/test-run-0/test-set-$i-report.yaml"

    # Extract the test status
    test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')

    # Print the status for debugging
    echo "Test status for test-set-$i: $test_status"

    # Check if any test set did not pass
    if [ "$test_status" != "PASSED" ]; then
        all_passed=false
        echo "Test-set-$i did not pass."
        break # Exit the loop early as all tests need to pass
    fi
done

# Check the overall test status and exit accordingly
if [ "$all_passed" = true ]; then
    echo "All tests passed"
    exit 0
else
    cat "test_logs.txt"
    exit 1
fi