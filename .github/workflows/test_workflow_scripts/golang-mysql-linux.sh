#!/bin/bash

set -e  # Exit immediately if a command exits with a non-zero status.

# Create Docker network
docker network create keploy_network || true

# Start MySQL container
docker run --network keploy_network --ip 172.18.0.22 --rm --name mysql -e MYSQL_ROOT_PASSWORD=my-secret-pw -d mysql:latest

# Wait for MySQL to initialize
until docker exec mysql mysqladmin ping -h "127.0.0.1" -uroot -pmy-secret-pw --silent; do
    echo "Waiting for MySQL to initialize..."
    sleep 2
done

# Grant privileges to root user from any host
docker exec mysql mysql -uroot -pmy-secret-pw -e "
    GRANT ALL PRIVILEGES ON *.* TO 'root'@'%' IDENTIFIED BY 'my-secret-pw';
    FLUSH PRIVILEGES;
"
echo "MySQL is ready and privileges are set."

# Export connection string
export ConnectionString="root:my-secret-pw@tcp(172.18.0.22:3306)/mysql"

# Build application
go build -o urlShort

# Start application container
docker run --network keploy_network --rm --name url_shortener -d url_shortener

# Wait for application to start
until curl -s http://localhost:9090/healthcheck; do
    echo "Waiting for application to start..."
    sleep 2
done

echo "Application started successfully."

# Run Keploy recording
keploy record -c "docker exec url_shortener ./urlShort" --generateGithubActions=false &

# Send requests
sleep 5  # Ensure Keploy is ready
curl -X POST http://localhost:9090/shorten -H "Content-Type: application/json" -d '{"url": "https://github.com"}'
curl -X GET http://localhost:9090/resolve/4KepjkTT

# Stop recording
sleep 5
pkill keploy

# Run Keploy tests
keploy test -c "docker exec url_shortener ./urlShort" --delay 7 --generateGithubActions=false

# Check test results
all_passed=true
for report_file in ./keploy/reports/test-run-0/*-report.yaml; do
    test_status=$(grep 'status:' "$report_file" | awk '{print $2}')
    echo "Test status in $(basename $report_file): $test_status"
    if [ "$test_status" != "PASSED" ]; then
        all_passed=false
        echo "Test failed in $(basename $report_file)."
    fi
done

if [ "$all_passed" = true ]; then
    echo "All tests passed successfully."
    exit 0
else
    echo "Some tests failed. Check the reports for details."
    exit 1
fi
