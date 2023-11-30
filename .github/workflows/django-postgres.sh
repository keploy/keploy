#! /bin/bash

# Start the postgres database.
sudo docker-compose up -d

# Install the dependencies.
pip3 install -r requirements.txt

# Set the environment variable for the app to run correctly.
export PYTHON_PATH=./venv/lib/python3.10/site-packages/django

# Generate the keploy-config file.
./../../../keployv2 generate-config

# Update the global noise to ignore the Allow header.
config_file="./keploy-config.yaml"
sed -i 's/"header": {}/"header":{"Allow":[]}/' "$config_file"


# Make migrations
# python3 manage.py makemigrations
# python3 manage.py migrate

# Wait for 5 seconds for it to complete
sleep 5

# Start the django-postgres app in record mode and record testcases and mocks.
sudo -E env PATH="$PATH" ./../../../keployv2 record -c "python3 manage.py runserver" &

# Wait for the application to start.
app_started=false
while [ "$app_started" = false ]; do
    if curl --location 'http://127.0.0.1:8000/'; then
        echo "we are in the if block right now"
        app_started=true
    fi
    echo $app_started
    sleep 3 # wait for 3 seconds before checking again.
done

# Start the postgres database.
sudo docker-compose up -d

# Check if postgres is running
sudo docker ps

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

