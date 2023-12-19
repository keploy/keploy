#! /bin/bash

source ./../../../.github/workflows/workflow_scripts/fake-iid.sh

# Checkout a different branch
git fetch origin
git checkout native-linux

# Start the postgres database.
docker-compose up -d

# Install the dependencies.
pip3 install -r requirements.txt

# Set the environment variable for the app to run correctly.
export PYTHON_PATH=./venv/lib/python3.10/site-packages/django

# Make the required migrations.
python3 manage.py makemigrations
python3 manage.py migrate

# Generate the keploy-config file.
./../../../keployv2 generate-config

#Clean any keploy folders.
sudo rm -rf keploy/

# Update the global noise to ignore the Allow header.
config_file="./keploy-config.yaml"
sed -i 's/header: {}/header: {"Allow":[]}/' "$config_file"

# Wait for 5 seconds for it to complete
sleep 5

for i in {1..2}; do
# Start the django-postgres app in record mode and record testcases and mocks.
sudo -E env PATH="$PATH" ./../../../keployv2 record -c "python3 manage.py runserver" &

# Wait for the application to start.
app_started=false
while [ "$app_started" = false ]; do
    if curl --location 'http://127.0.0.1:8000/user'; then
        app_started=true
    fi
    sleep 3 # wait for 3 seconds before checking again.
done

# Get the pid of keploy.
pid=$(pgrep keploy)

# Start making curl calls to record the testcases and mocks.
curl --location 'http://127.0.0.1:8000/user/' \
--header 'Content-Type: application/json' \
--data-raw '    {
        "name": "Jane Smith",
        "email": "jane.smith@example.com",
        "password": "smith567",
        "website": "www.janesmith.com"
    }'

curl --location 'http://127.0.0.1:8000/user/' \
--header 'Content-Type: application/json' \
--data-raw '    {
        "name": "John Doe",
        "email": "john.doe@example.com",
        "password": "john567",
        "website": "www.johndoe.com"
    }'

curl --location 'http://127.0.0.1:8000/user/'

curl --location 'http://127.0.0.1:8000/user/' \
--header 'Content-Type: application/json' \
--data-raw '    {
        "name": "Alice green",
        "email": "alice.green@example.com",
        "password": "alice789",
        "website": "www.alicegreen.com"
    }'

# Wait for 5 seconds for keploy to record the tcs and mocks.
sleep 5

# Stop the gin-mongo app.
sudo kill $pid

# Wait for 5 seconds for keploy to stop.
sleep 5
done

# Start the app in test mode.
sudo -E env PATH="$PATH" ./../../../keployv2 test -c "python3 manage.py runserver" --delay 20

# Get the test results from the testReport file.
report_file="./keploy/testReports/report-1.yaml"
test_status1=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
report_file2="./keploy/testReports/report-2.yaml"
test_status2=$(grep 'status:' "$report_file2" | head -n 1 | awk '{print $2}')

# Return the exit code according to the status.
if [ "$test_status1" = "PASSED" ] && [ "$test_status2" = "PASSED" ]; then
    exit 0
else
    exit 1
fi

