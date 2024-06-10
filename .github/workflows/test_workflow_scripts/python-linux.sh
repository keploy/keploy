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
    sudo -E env PATH="$PATH" ./../../../keployv2 record -c "python3 manage.py runserver" --generateGithubActions=false &
    wait_for_app
    perform_api_calls
    sleep 5  # Wait for recording to complete
    sudo kill $(pgrep keploy)
    sleep 5  # Allow keploy to terminate gracefully
done

# Testing phase
sudo -E env PATH="$PATH" ./../../../keployv2 test -c "python3 manage.py runserver" --delay 10 --generateGithubActions=false

# Collect and evaluate test results
report_file="./keploy/reports/test-run-0/test-set-0-report.yaml"
test_status1=$(awk '/status:/ {print $2}' "$report_file")
report_file2="./keploy/reports/test-run-0/test-set-1-report.yaml"
test_status2=$(awk '/status:/ {print $2}' "$report_file2")

# Exit based on test results
if [ "$test_status1" = "PASSED" ] && [ "$test_status2" = "PASSED" ]; then
    exit 0
else
    exit 1
fi
