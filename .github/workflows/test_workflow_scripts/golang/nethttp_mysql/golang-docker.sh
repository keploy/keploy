#!/bin/bash
#
# End-to-end pipeline for the samples-go/nethttp-mysql sample.
#
# Regression guard: during replay the app's db.Ping() issues real MySQL
# commands against the Keploy MITM proxy. If the proxy's command-phase
# read deadline ever regresses to <= 0 (zero SQLDelay over the wire, or
# an int64 overflow from a seconds-valued Duration multiplied by
# time.Second), db.Ping() blocks, the HTTP listener never binds :8080,
# and every replayed request fails with "connection reset by peer".
# That failure is caught by the "All tests passed" gate below.

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

set -x

docker network inspect keploy-network >/dev/null 2>&1 || docker network create keploy-network

# Remove any preexisting keploy tests.
sudo rm -rf keploy/

# Fresh MySQL for recording.
docker rm -f mysql 2>/dev/null || true
docker run --name mysql --rm --net keploy-network \
    -e MYSQL_ROOT_PASSWORD=password -e MYSQL_DATABASE=testdb \
    -p 3306:3306 -d mysql:8.0

echo "Waiting for MySQL..."
for i in $(seq 1 60); do
    if docker exec mysql mysqladmin ping -h localhost -u root -ppassword --silent 2>/dev/null; then
        echo "MySQL is ready"
        break
    fi
    sleep 2
done

# Build the sample.
docker build -t nethttp-mysql:1.0 .

container_kill() {
    REC_PID="$(pgrep -n -f "$(basename "${RECORD_BIN:-keploy}") record" || true)"
    echo "$REC_PID Keploy PID"
    echo "Killing keploy"
    sudo kill -INT "$REC_PID" 2>/dev/null || true
}

send_request() {
    sleep 30
    app_started=false
    while [ "$app_started" = false ]; do
        if curl -sf http://localhost:8080/health >/dev/null; then
            app_started=true
        fi
        sleep 3
    done
    echo "App started"

    curl -sS http://localhost:8080/users
    curl -sS "http://localhost:8080/users/add?name=charlie&email=charlie@test.com"
    curl -sS "http://localhost:8080/users/add?name=dave&email=dave@test.com"
    curl -sS http://localhost:8080/users

    # Give keploy a beat to persist mocks before we SIGINT.
    sleep 5
    container_kill
    wait
}

for i in {1..2}; do
    container_name="nethttpMysqlApp_${i}"
    send_request &
    $RECORD_BIN record \
        -c "docker run -p 8080:8080 --name ${container_name} --network keploy-network \
            -e MYSQL_HOST=mysql -e MYSQL_PORT=3306 -e MYSQL_USER=root \
            -e MYSQL_PASSWORD=password -e MYSQL_DATABASE=testdb \
            nethttp-mysql:1.0" \
        --container-name "${container_name}" \
        --buildDelay 60 \
        &> "${container_name}.txt"

    if grep "WARNING: DATA RACE" "${container_name}.txt"; then
        echo "Race condition detected in recording, stopping pipeline..."
        cat "${container_name}.txt"
        exit 1
    fi
    if grep "ERROR" "${container_name}.txt"; then
        echo "Error found in pipeline..."
        cat "${container_name}.txt"
        exit 1
    fi
    sleep 5
    echo "Recorded test case and mocks for iteration ${i}"
done

# Critical for the regression guard: replay must run against mocks, not
# a live MySQL. If MySQL were still up, a bugged proxy would be masked
# by direct-to-db traffic.
echo "Shutting down MySQL before test mode..."
docker stop mysql || true
docker rm mysql || true

test_container="nethttpMysqlApp_test"
$REPLAY_BIN test \
    -c "docker run -p 8080:8080 --rm --name ${test_container} --network keploy-network \
        -e MYSQL_HOST=mysql -e MYSQL_PORT=3306 -e MYSQL_USER=root \
        -e MYSQL_PASSWORD=password -e MYSQL_DATABASE=testdb \
        nethttp-mysql:1.0" \
    --containerName "${test_container}" \
    --apiTimeout 60 \
    --delay 30 \
    --generate-github-actions=false \
    &> "${test_container}.txt"

if grep "ERROR" "${test_container}.txt"; then
    echo "Error found in pipeline..."
    cat "${test_container}.txt"
    exit 1
fi
if grep "WARNING: DATA RACE" "${test_container}.txt"; then
    echo "Race condition detected in test, stopping pipeline..."
    cat "${test_container}.txt"
    exit 1
fi

all_passed=true
for i in {0..1}; do
    report_file="./keploy/reports/test-run-0/test-set-${i}-report.yaml"
    test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
    echo "Test status for test-set-${i}: $test_status"
    if [ "$test_status" != "PASSED" ]; then
        all_passed=false
        echo "Test-set-${i} did not pass."
        break
    fi
done

if [ "$all_passed" = true ]; then
    echo "All tests passed"
    exit 0
fi
cat "${test_container}.txt"
exit 1
