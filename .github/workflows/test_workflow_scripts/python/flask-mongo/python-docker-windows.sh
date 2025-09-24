#!/usr/bin/env bash

# Windows (GitHub Actions) variant running under bash. Requires Docker Desktop on Windows with Linux containers.
# Differences from Linux:
# - sed inline usage via GNU sed (provided in Git for Windows runner in bash shell) supports -i without backup suffix.
# - 'sudo' may not be available; use conditional wrapper.

set -euo pipefail

SUDO=""
if command -v sudo >/dev/null 2>&1; then
  SUDO="sudo"
fi

source ${GITHUB_WORKSPACE}/.github/workflows/test_workflow_scripts/test-iid.sh

# Detect engine OSType and pick a sane default network driver
get_docker_network_driver() {
  local engine_os
  engine_os="$(docker info --format '{{.OSType}}' 2>/dev/null || echo '')"
  if [[ "$engine_os" == "windows" ]]; then
    echo "nat"    # Windows engine commonly uses 'nat'
  else
    echo "bridge" # Linux engine uses 'bridge'
  fi
}

# Create network if possible; NEVER cause script exit under 'set -e'
create_docker_network() {
  local network_name="keploy-network"
  local driver
  driver="$(get_docker_network_driver)"

  # Already exists?
  if docker network ls --format "{{.Name}}" | grep -q "^${network_name}$"; then
    echo "Network ${network_name} already exists"
    return 0
  fi

  echo "Creating Docker network: ${network_name} (driver=${driver})"

  # Try with detected driver
  if docker network create --driver "${driver}" "${network_name}" >/dev/null 2>&1; then
    echo "Successfully created network ${network_name} with driver ${driver}"
    return 0
  fi

  # Try default driver fallback
  if docker network create "${network_name}" >/dev/null 2>&1; then
    echo "Successfully created network ${network_name} with default driver"
    return 0
  fi

  # Couldn’t create; warn but DO NOT fail the script
  echo "Warning: Failed to create custom network. Falling back to default bridge/NAT."
  echo "This may affect container communication. Check Docker engine mode & permissions."
  return 2
}

# Validate Docker daemon is running
if ! docker info >/dev/null 2>&1; then
  echo "Error: Docker daemon is not running or not accessible"
  exit 1
fi

# Create network — guard with 'if' so set -e doesn't abort on nonzero
if create_docker_network; then
  NETWORK_PARAM="--net keploy-network"
  echo "Using custom network: keploy-network"
else
  NETWORK_PARAM=""  # fall back to engine default network
  echo "Using default network (no custom network available)"
fi

# Start MongoDB container
# docker run --name mongo --rm $NETWORK_PARAM -p 27017:27017 -d mongo

# # check whether mongo is running
# if docker ps | grep -q "mongo"; then
#     echo "Mongo is already running"
# else
#     echo "Mongo is not running, attempting to start..."
#     docker run --name mongo --rm $NETWORK_PARAM -p 27017:27017 -d mongo
# fi

# Set up environment
rm -rf keploy/  # Clean up old test data
docker build -t flask-app:1.0 .  # Build the Docker image

# check whether flask-app:1.0 is built
if docker images | grep -q "flask-app:1.0"; then
    echo "Flask-app:1.0 is already built"
else
    echo "Flask-app:1.0 is not built"
    exit 1
fi

# Configure keploy
sed -i 's/global: {}/global: {"header": {"Allow":[]}}/' "./keploy.yml"
sleep 5  # Allow time for configuration to apply

container_kill() {
    pid=$(pgrep -n keploy || true)
    if [ -n "${pid}" ]; then
      echo "$pid Keploy PID"
      echo "Killing keploy"
      $SUDO kill ${pid} || true
    fi
}

send_request(){
    local container_name=$1
    sleep 10
    app_started=false
    while [ "$app_started" = false ]; do
        if curl --silent http://localhost:6000/students; then
            app_started=true
        else
            sleep 3  # Check every 3 seconds
        fi
    done
    # Start making curl calls to record the testcases and mocks.
    curl -X POST -H "Content-Type: application/json" -d '{"student_id": "12345", "name": "John Doe", "age": 20}' http://localhost:6000/students
    curl -X POST -H "Content-Type: application/json" -d '{"student_id": "12346", "name": "Alice Green", "age": 22}' http://localhost:6000/students
    curl http://localhost:6000/students
    curl -X PUT -H "Content-Type: application/json" -d '{"name": "Jane Smith", "age": 21}' http://localhost:6000/students/12345
    curl http://localhost:6000/students
    curl -X DELETE http://localhost:6000/students/12345

    # Wait for 5 seconds for keploy to record the tcs and mocks.
    sleep 5
    container_kill
    wait || true
}

# Record sessions
for i in {1..2}; do
    container_name="flaskApp_${i}"
    send_request &
    $SUDO -E env PATH=$PATH "$RECORD_BIN" record -c "docker run -p 6000:6000 $NETWORK_PARAM --rm --name $container_name flask-app:1.0" --container-name "$container_name" &> "${container_name}.txt" || true
    if grep "ERROR" "${container_name}.txt"; then
        echo "Error found in pipeline..."
        cat "${container_name}.txt"
        exit 1
    fi
    if grep "WARNING: DATA RACE" "${container_name}.txt"; then
        echo "Race condition detected in recording, stopping pipeline..."
        cat "${container_name}.txt"
        exit 1
    fi
    sleep 5

    echo "Recorded test case and mocks for iteration ${i}"
done

# Shutdown mongo before test mode - Keploy should use mocks for database interactions
echo "Shutting down mongo before test mode..."
docker stop mongo || true
docker rm mongo || true
echo "MongoDB stopped - Keploy should now use mocks for database interactions"

# Testing phase
test_container="flashApp_test"
$SUDO -E env PATH=$PATH "$REPLAY_BIN" test -c "docker run -p8080:8080 $NETWORK_PARAM --name $test_container flask-app:1.0" --containerName "$test_container" --apiTimeout 60 --delay 20 --generate-github-actions=false &> "${test_container}.txt" || true
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
    cat "${test_container}.txt"
    exit 1
fi
