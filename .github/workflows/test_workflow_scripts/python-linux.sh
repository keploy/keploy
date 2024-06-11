#!/bin/bash

source ./../../../.github/workflows/test_workflow_scripts/test-iid.sh

# Checkout to the specified branch
git fetch origin
git checkout native-linux

# Start the postgres database
docker-compose up -d

# Install dependencies
pip3 install -r requirements.txt

# Setup environment
export PYTHON_PATH=./venv/lib/python3.10/site-packages/django

# Database migrations
python3 manage.py makemigrations
python3 manage.py migrate

# Configuration and cleanup
sudo ./../../../keployv2 config --generate
sudo rm -rf keploy/  # Clean old test data
config_file="./keploy.yml"
sed -i 's/global: {}/global: {"header": {"Allow":[]}}/' "$config_file"
sleep 5  # Allow time for configuration changes

# Function to wait for the application to become responsive
wait_for_app() {
    while ! curl --silent --location 'http://127.0.0.1:8000/'; do
        sleep 3  # Check every 3 seconds
    done
}

# Function to perform API calls
perform_api_calls() {
    curl --location 'http://127.0.0.1:8000/user/' --header 'Content-Type: application/json' --data-raw '{
        "name": "Jane Smith",
        "email": "jane.smith@example.com",
        "password": "smith567",
        "website": "www.janesmith.com"
    }'
    curl --location 'http://127.0.0.1:8000/user/' --header 'Content-Type: application/json' --data-raw '{
        "name": "John Doe",
        "email": "john.doe@example.com",
        "password": "john567",
        "website": "www.johndoe.com"
    }'
    curl --location 'http://127.0.0.1:8000/user/'
}

# Record and Test cycles
for i in {1..2}; do
    sudo -E env PATH="$PATH" ./../../../keployv2 record -c "python3 manage.py runserver" --generateGithubActions=false &> record_logs.txt &
    wait_for_app
    perform_api_calls
    sleep 5  # Wait for recording to complete
    sudo kill $(pgrep keploy)
    sleep 5  # Allow keploy to terminate gracefully
done

# Testing phase
sudo -E env PATH="$PATH" ./../../../keployv2 test -c "python3 manage.py runserver" --delay 10 --generateGithubActions=false &> test_logs.txt

grep -q "race condition detected" test_logs.txt && echo "Race condition detected in testing, stopping tests..." && exit 1
all_passed=true

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
    exit 1
fi