#! /bin/bash

# Start the postgres database.
sudo docker-compose up -d

# Start a virtual env for the app.
pip3 install virtualenv
python3 -m virtualenv venv
source venv/bin/activate

# Install the dependencies.
pip3 install -r requirements.txt

# Set the environment variable for the app to run correctly.
export PYTHON_PATH=./venv/lib/python3.10/site-packages/django

# Generate the keploy-config file.
./../../../keployv2 generate-config

# Update the global noise to ignore the Allow header.
config_file="./keploy-config.yaml"
sed -i 's/"header": {}/"header":{"Allow":[]}/' "$config_file"

# Start the django-postgres app in record mode and record testcases and mocks.
sudo -E env PATH="$PATH" ./../../../testing/keploy/keploy record -c "python3 manage.py runserver" &

# Wait for the application to start.
app_started=false
while [ "$app_started" = false ]; do
    if curl --location 'http://127.0.0.1:8000/user/'; then
        app_started=true
    fi
    sleep 3 # wait for 3 seconds before checking again.
done

# Get the pid of the application.
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
        "name": "John Doe",
        "email": "john.doe@example.com",
        "password": "john567",
        "website": "www.johndoe.com"
    }'

# Wait for 5 seconds for keploy to record the tcs and mocks.
sleep 5

# Stop the gin-mongo app.
sudo kill $pid