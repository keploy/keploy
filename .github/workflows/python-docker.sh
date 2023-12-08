#! /bin/bash

# Start the postgres database.
docker network create django-postgres-network
docker run -p 5432:5432 -d -e POSTGRES_PASSWORD=postgres  --network django-postgres-network --name mypostgres -v $(pwd)/sql:/docker-entrypoint-initdb.d postgres
docker network inspect django-postgres-network

# Remove old keploy tests and mocks.
sudo rm -rf keploy/

# Change the database configuration.
sed -i "s/'HOST': '.*'/'HOST': 'mypostgres'/g" django_postgres/settings.py
sed -i "s/'PORT': '.*'/'PORT': '5432'/g" django_postgres/settings.py

# Generate the keploy-config file.
echo "Generating keploy config"
docker run  --name keploy-v2 -p 16789:16789 --privileged --pid=host -v "$(pwd)":/files -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock --rm keployv2 generate-config

# Update the global noise to ignore the Allow header.
config_file="./keploy-config.yaml"
sed -i 's/"header": {}/"header":{"Allow":[]}/' "$config_file"

# Wait for 5 seconds for it to complete
sleep 5

# Start the django-postgres app in record mode and record testcases and mocks.
docker build -t django-app:1.0 .
docker ps
docker network inspect django-postgres-network
docker run  --name keploy-v2 -p 16789:16789 --privileged --pid=host -v "$(pwd)":/files -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock --rm keployv2 record -c "docker run -p 8000:8000 --name DjangoApp --network django-postgres-network django-app:1.0" &

# Wait for the application to start.
app_started=false
while [ "$app_started" = false ]; do
    if curl --location 'http://127.0.0.1:8000/'; then
        app_started=true
    fi
    sleep 3 # wait for 3 seconds before checking again.
done

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

# Stop the keploy container and the application container.
docker rm -f keploy-v2
docker rm -f DjangoApp

# Start the app in test mode.
docker run  --name keploy-v2 -p 16789:16789 --privileged --pid=host -v "$(pwd)":/files -v /sys/fs/cgroup:/sys/fs/cgroup -v /sys/kernel/debug:/sys/kernel/debug -v /sys/fs/bpf:/sys/fs/bpf -v /var/run/docker.sock:/var/run/docker.sock --rm keployv2 test -c "docker run -p 8000:8000 --name DjangoApp --network django-postgres-network django-app:1.0" --delay 20

# Get the test results from the testReport file.
report_file="./keploy/testReports/report-1.yaml"
test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')

# Return the exit code according to the status.
if [ "$test_status" = "PASSED" ]; then
    exit 0
else
    exit 1
fi

