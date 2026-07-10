#!/usr/bin/env bash

# macOS variant for echo-sql (docker compose). Uses BSD sed.
set -euo pipefail

# for the below source make it such a way that if the file is not present or already present it does not error
source ./../../.github/workflows/test_workflow_scripts/test-iid-macos.sh

# ---------------------------------------------------------------------------
# Docker daemon readiness barrier. On the self-hosted macOS runner Docker
# Desktop / colima can still be finishing startup when the job begins; a
# `docker compose build` fired against a not-yet-ready daemon fails the whole
# lane. Poll `docker info` (bounded) rather than assuming the daemon is up.
# ---------------------------------------------------------------------------
wait_for_docker_daemon() {
    local deadline=$(( SECONDS + 120 ))
    until docker info >/dev/null 2>&1; do
        if [ "$SECONDS" -ge "$deadline" ]; then
            echo "ERROR: Docker daemon not ready after 120s. Is Docker Desktop/colima running on the runner?"
            exit 1
        fi
        sleep 2
    done
    echo "Docker daemon is ready."
}
wait_for_docker_daemon

# Function to find available port
find_available_port() {
    local start_port=${1:-6000}
    local port=$start_port
    while lsof -i:$port >/dev/null 2>&1; do
        port=$((port + 1))
    done
    echo $port
}

# echo-sql uses base range 10000-11999, offset by JOB_ID hash
PORT_OFFSET=$(( $(echo "$JOB_ID" | cksum | awk '{print $1}') % 400 ))
BASE_PORT=$(( 10000 + PORT_OFFSET * 5 ))
APP_PORT=$(find_available_port $BASE_PORT)
DB_PORT=$(find_available_port $((APP_PORT + 1)))
PROXY_PORT=$(find_available_port $((DB_PORT + 1)))
DNS_PORT=$(find_available_port $((PROXY_PORT + 1)))

# Generate unique container names with JOB_ID suffix
APP_CONTAINER="echoApp_${JOB_ID}"
DB_CONTAINER="postgresDb_${JOB_ID}"
KEPLOY_CONTAINER="keploy_${JOB_ID}"
APP_IMAGE="go-app-${JOB_ID}"

echo "Using ports - APP: $APP_PORT, DB: $DB_PORT, PROXY: $PROXY_PORT, DNS: $DNS_PORT"
echo "Using containers - APP: $APP_CONTAINER, DB: $DB_CONTAINER, KEPLOY: $KEPLOY_CONTAINER"

# Job-scoped compose project name to avoid collisions with concurrent runs.
export COMPOSE_PROJECT_NAME="echo-sql-${JOB_ID}"

# Clean up stale Docker state from previous killed runs of this JOB_ID.
echo "Cleaning up stale docker compose project state..."
docker compose down --remove-orphans -v 2>/dev/null || true
for id in $(docker compose ps -aq 2>/dev/null); do
  docker rm -f "$id" 2>/dev/null || true
done
docker compose rm -f -s 2>/dev/null || true

# Cleanup function to remove containers
cleanup() {
    echo "Cleaning up containers and services for job ${JOB_ID}..."
    docker compose down >/dev/null 2>&1 || true
    docker rm -f "$DB_CONTAINER" >/dev/null 2>&1 || true
    docker rm -f "$APP_CONTAINER" >/dev/null 2>&1 || true
    docker rm -f "$KEPLOY_CONTAINER" >/dev/null 2>&1 || true
    echo "Cleanup completed for job ${JOB_ID}"
}

# Set trap to run cleanup on script exit (success, failure, or interrupt)
trap cleanup EXIT INT TERM

# Replace ports and container names in all files in current directory
echo "Updating configuration files with dynamic ports and container names..."
for file in $(find . -maxdepth 1 -type f \( -name "*.yml" -o -name "*.yaml" -o -name "*.go" -o -name "*.json" -o -name "*.sh" -o -name "*.env" -o -name "*.md" \)); do
    if [ -f "$file" ] && [ "$file" != "./golang-docker-macos.sh" ]; then
        # Replace 8082 with APP_PORT
        sed -i '' "s/8082/${APP_PORT}/g" "$file" 2>/dev/null || true
        # Replace echoApp with APP_CONTAINER
        sed -i '' "s/echoApp/${APP_CONTAINER}/g" "$file" 2>/dev/null || true
        # Replace postgresDb with DB_CONTAINER
        sed -i '' "s/postgresDb/${DB_CONTAINER}/g" "$file" 2>/dev/null || true
        # Replace 5432: with DB_PORT: in docker-compose files
        sed -i '' "s/5432:/${DB_PORT}:/g" "$file" 2>/dev/null || true
        # Replace go-app with APP_IMAGE
        sed -i '' "s/go-app/${APP_IMAGE}/g" "$file" 2>/dev/null || true
        echo "Updated $file"
    fi
done

# ---------------------------------------------------------------------------
# Defense-in-depth: normalize the Postgres health gate to a TCP probe.
#
# The DURABLE fix lives upstream in the sample's compose (keploy/samples-go#237).
# Background: the stock healthcheck was `pg_isready -U postgres`, which probes the
# UNIX SOCKET. During the postgres image's first-boot init the entrypoint runs a
# TEMPORARY socket-only server (listen_addresses empty, init.sql runs) before the
# real server binds 0.0.0.0:5432 — so the socket probe flips the container
# "healthy" seconds before the TCP listener exists; Compose then releases go-app,
# whose TCP connect (host=postgresDb:5432, 10s single-attempt, NO retry) is
# refused and it Fatal()s -> exit 1, the app port never opens, record fails. Wide
# on the emulated amd64 postgres:10.5 image on Apple-silicon runners, so the race
# is near-permanent there. (Reproduced deterministically on Linux by widening the
# init window: socket healthcheck -> go-app exit 1 "connect: connection refused";
# TCP healthcheck -> connects cleanly.)
#
# We also fix it here, IDEMPOTENTLY, so this lane stays green even when it checks
# out a samples-go revision predating #237, and to cover the path where keploy
# recreates (rather than reuses) the postgres service. Once #237 is in the pinned
# samples-go these seds simply no-op (the target strings are already gone). go-app
# is additionally protected by the wait_for_postgres_ready TCP barrier below.
# ---------------------------------------------------------------------------
for cf in docker-compose.yml docker-compose.yaml; do
    [ -f "$cf" ] || continue
    sed -i '' 's|pg_isready -U postgres -d postgres|pg_isready -h 127.0.0.1 -p 5432 -U postgres -d postgres|g' "$cf" 2>/dev/null || true
    sed -i '' 's|start_period: 30s|start_period: 90s|g' "$cf" 2>/dev/null || true
    sed -i '' 's|retries: 5|retries: 30|g' "$cf" 2>/dev/null || true
done

# Build Docker Image(s)
docker compose build

# Remove any preexisting keploy tests and mocks.
rm -rf keploy/
rm ./keploy.yml >/dev/null 2>&1 || true

# Generate the keploy-config file.
$RECORD_BIN config --generate

# Update the global noise to ts in the config file.
config_file="./keploy.yml"
if [ -f "$config_file" ]; then
  sed -i '' 's/global: {}/global: {"body": {"ts":[]}}/' "$config_file" || true
else
  echo "⚠️ Config file $config_file not found, skipping sed replace."
fi

container_kill() {
    pid=$(pgrep -n keploy)
    echo "$pid Keploy PID"
    echo "Killing keploy"
    sudo kill $pid
}

# Dump app + DB diagnostics. Called when a readiness barrier times out so the CI
# log shows WHY (app exited, DB unreachable, ...) instead of a bare timeout.
dump_diag() {
    echo "===== DIAGNOSTICS (${1:-unknown}) ====="
    docker compose ps -a 2>&1 || true
    echo "----- app container logs (${APP_CONTAINER}) -----"
    docker logs --tail 200 "$APP_CONTAINER" 2>&1 || true
    echo "----- postgres container logs (${DB_CONTAINER}) -----"
    docker logs --tail 200 "$DB_CONTAINER" 2>&1 || true
    echo "===== END DIAGNOSTICS ====="
}

# Bring Postgres up and block until it accepts TCP connections BEFORE keploy
# starts the app, so:
#   1. the go-app never races a not-yet-listening DB (its connect has a 10s,
#      NO-RETRY timeout and Fatal()s -> exit 1 on failure), and
#   2. the slow, emulated DB init does NOT eat into the --record-timer window
#      (that timer starts counting the moment keploy runs `docker compose up`).
# When keploy's `docker compose up` reuses this already-running postgres (same
# project + service name), `depends_on: service_healthy` releases the app
# immediately against a fully-initialised DB. If keploy instead recreates the
# service, the TCP healthcheck (fixed in samples-go#237, and force-normalized by
# the sed above) is what keeps the gate correct — so the app is protected on
# either path.
wait_for_postgres_ready() {
    echo "Pre-starting postgres and waiting for it to accept TCP connections..."
    docker compose up -d postgres
    local ready=false
    for _ in $(seq 1 90); do
        # Probe over TCP (127.0.0.1), exactly like the app connects. This only
        # succeeds once the REAL server is listening, so it survives the init
        # temp-server bounce (socket-up / TCP-down) that fools pg_isready's
        # default socket probe.
        if docker exec "$DB_CONTAINER" pg_isready -h 127.0.0.1 -p 5432 -U postgres -d postgres >/dev/null 2>&1; then
            ready=true
            break
        fi
        sleep 2
    done
    if [ "$ready" = false ]; then
        echo "ERROR: postgres (${DB_CONTAINER}) did not accept TCP connections within 180s."
        dump_diag "postgres-readiness timeout"
        exit 1
    fi
    echo "Postgres is TCP-ready."
}

send_request(){
    echo "Sending requests to the application..."
    # Bounded wait-for-app-readiness barrier. Poll the app port until it actually
    # serves an HTTP response (any status is fine — /health maps to GET /:param
    # and 404s, which still proves the HTTP server AND its DB connection are up).
    # No bare sleep as the barrier; on timeout dump app + DB logs and fail loudly
    # instead of looping forever against a dead port (the old `while` loop hung
    # until the job/record timer, hiding the real cause).
    local app_started=false
    for _ in $(seq 1 60); do
        if curl --silent --output /dev/null "http://localhost:${APP_PORT}/health"; then
            app_started=true
            break
        fi
        sleep 2
    done
    if [ "$app_started" = false ]; then
        echo "ERROR: app on port ${APP_PORT} did not become ready within 120s."
        dump_diag "app-readiness timeout"
        return 1
    fi
    echo "App started"
    # Make curl calls to record the test cases and mocks.
    curl --request POST \
      --url http://localhost:${APP_PORT}/url \
      --header 'content-type: application/json' \
      --data '{
      "url": "https://google.com"
    }'

    curl --request POST \
      --url http://localhost:${APP_PORT}/url \
      --header 'content-type: application/json' \
      --data '{
      "url": "https://facebook.com"
    }'

    curl -X GET http://localhost:${APP_PORT}/health

    # Wait for 3 seconds for keploy to record the test cases and mocks.
    sleep 3
    echo "Requests sent successfully."
}

do_record_iteration() {
    local i="$1"
    local extra_flags="${2:-}"
    local label="${extra_flags:+_json}"
    local container_name="${APP_CONTAINER}"
    local log_file_name="${APP_CONTAINER}_${i}${label}"

    # Deterministic barrier: DB must be up and TCP-ready before keploy launches
    # the app (see wait_for_postgres_ready). keploy's `docker compose up` reuses
    # this postgres, so the app connects on its first, no-retry attempt.
    wait_for_postgres_ready

    send_request &

    # shellcheck disable=SC2086
    $RECORD_BIN record $extra_flags -c "docker compose up" --container-name "$container_name" --generateGithubActions=false --record-timer "40s" --proxy-port=$PROXY_PORT --dns-port=$DNS_PORT --keploy-container "$KEPLOY_CONTAINER" 2>&1 | tee "${log_file_name}.txt"

    if grep "WARNING: DATA RACE" "${log_file_name}.txt"; then
        echo "Race condition detected in recording, stopping pipeline..."
        exit 1
    fi
    if grep "ERROR" "${log_file_name}.txt"; then
        echo "Error found in pipeline..."
        exit 1
    fi
    sleep 5

    echo "Recorded test case and mocks for iteration ${i}${label:+ (json)}"
}

for i in {1..2}; do
    do_record_iteration "$i"
done

# shellcheck disable=SC1091
source "${GITHUB_WORKSPACE:-${PWD%/samples-*}}/.github/workflows/test_workflow_scripts/json-pass-helpers.sh"

if json_pass_supported; then
    for i in {1..2}; do
        do_record_iteration "$i" "--storage-format json"
    done
fi



# Shutdown services before test mode - Keploy should use mocks for dependencies
echo "Shutting down docker compose services before test mode..."
docker compose down
echo "Services stopped - Keploy should now use mocks for dependency interactions"

# Start keploy in test mode.
test_container="${APP_CONTAINER}"
$REPLAY_BIN test -c 'docker compose up' --containerName "$test_container" --debug --apiTimeout 60 --delay 10 --generate-github-actions=false --proxy-port=$PROXY_PORT --dns-port=$DNS_PORT --keploy-container "$KEPLOY_CONTAINER" 2>&1 | tee "${test_container}.txt"

if grep "ERROR" "${test_container}.txt"; then
    echo "Error found in pipeline..."
    exit 1
fi

if grep "WARNING: DATA RACE" "${test_container}.txt"; then
    echo "Race condition detected in test, stopping pipeline..."
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
        break
    fi

done

if [ "$all_passed" != true ]; then
    echo "Some tests failed"
    exit 1
fi

if json_pass_supported; then
    $REPLAY_BIN test --storage-format json -c 'docker compose up' --containerName "$test_container" --debug --apiTimeout 60 --delay 10 --generate-github-actions=false --proxy-port=$PROXY_PORT --dns-port=$DNS_PORT --keploy-container "$KEPLOY_CONTAINER" 2>&1 | tee "${test_container}_json.txt"
    if grep "ERROR" "${test_container}_json.txt"; then
        cat "${test_container}_json.txt"
        exit 1
    fi
    if grep "WARNING: DATA RACE" "${test_container}_json.txt"; then
        cat "${test_container}_json.txt"
        exit 1
    fi
    if ! json_scan_reports; then
        cat "${test_container}_json.txt"
        exit 1
    fi
    echo "All tests passed (yaml + json)"
else
    echo "All tests passed (yaml only — json pass skipped for compat-matrix cell)"
fi
