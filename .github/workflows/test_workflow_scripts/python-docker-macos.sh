#!/usr/bin/env bash

# macOS variant. Requires Docker Desktop for Mac running.
set -euo pipefail

# for the below shource make it such a way that if the file is not present or already present it does not error
source ./../../.github/workflows/test_workflow_scripts/test-iid-macos.sh
source "${GITHUB_WORKSPACE:-${PWD%/samples-*}}/.github/workflows/test_workflow_scripts/docker-build-retry.sh"

# Function to find available port
find_available_port() {
    local start_port=${1:-8080}
    local port=$start_port
    while lsof -i:$port >/dev/null 2>&1; do
        port=$((port + 1))
    done
    echo $port
}

# python uses base range 14000-15999, offset by JOB_ID hash
PORT_OFFSET=$(( $(echo "$JOB_ID" | cksum | awk '{print $1}') % 400 ))
BASE_PORT=$(( 14000 + PORT_OFFSET * 5 ))
APP_PORT=$(find_available_port $BASE_PORT)
DB_PORT=$(find_available_port $((APP_PORT + 1)))
PROXY_PORT=$(find_available_port $((DB_PORT + 1)))
DNS_PORT=$(find_available_port $((PROXY_PORT + 1)))

# Generate unique container names with JOB_ID suffix
APP_CONTAINER="flaskApp_${JOB_ID}"
DB_CONTAINER="mongo_${JOB_ID}"
KEPLOY_CONTAINER="keploy_${JOB_ID}"
APP_IMAGE="flask-app_${JOB_ID}:1.0"
NETWORK_NAME="keploy-network-${JOB_ID}"

echo "Using ports - APP: $APP_PORT, DB: $DB_PORT, PROXY: $PROXY_PORT, DNS: $DNS_PORT"
echo "Using containers - APP: $APP_CONTAINER, DB: $DB_CONTAINER, KEPLOY: $KEPLOY_CONTAINER"
echo "Using network: $NETWORK_NAME"

# Cleanup function to remove containers
cleanup() {
    echo "Cleaning up containers and network for job ${JOB_ID}..."
    docker rm -f "$DB_CONTAINER" >/dev/null 2>&1 || true
    docker rm -f "$APP_CONTAINER" >/dev/null 2>&1 || true
    docker rm -f "${APP_CONTAINER}_1" >/dev/null 2>&1 || true
    docker rm -f "${APP_CONTAINER}_2" >/dev/null 2>&1 || true
    docker rm -f "${APP_CONTAINER}_test_1" >/dev/null 2>&1 || true
    docker rm -f "$KEPLOY_CONTAINER" >/dev/null 2>&1 || true
    docker network rm "$NETWORK_NAME" >/dev/null 2>&1 || true
    echo "Cleanup completed for job ${JOB_ID}"
}

# Set trap to run cleanup on script exit (success, failure, or interrupt)
trap cleanup EXIT INT TERM

# Replace ports and container names in all files in current directory
echo "Updating configuration files with dynamic ports and container names..."
for file in $(find . -maxdepth 1 -type f \( -name "*.yml" -o -name "*.yaml" -o -name "*.py" -o -name "*.json" -o -name "*.sh" -o -name "*.env" -o -name "*.md" \)); do
    if [ -f "$file" ] && [ "$file" != "./golang-docker-macos.sh" ]; then
        # Replace 6000 with APP_PORT
        sed -i '' "s/6000/${APP_PORT}/g" "$file" 2>/dev/null || true
        # Replace mongo:27017 with $DB_CONTAINER:27017
        sed -i '' "s/mongo:27017/${DB_CONTAINER}:27017/g" "$file" 2>/dev/null || true
        echo "Updated $file"
    fi
done

# --- Networking: create a job-scoped network to avoid collisions ---
docker network rm "$NETWORK_NAME" 2>/dev/null || true
docker network create "$NETWORK_NAME"

# --- Start fresh Mongo (force remove any stale one first) ---
docker rm -f "$DB_CONTAINER" >/dev/null 2>&1 || true
# MongoDB 8 refuses to start on a Linux kernel >= 6.19 -- its vendored
# TCMalloc violates the rseq ABI, so it exits 1 within ~100ms, logging
# "MongoDB cannot start: Linux kernel versions 6.19 and newer has a known
# incompatibility with this version of MongoDB" (SERVER-121912). Every
# self-hosted macOS runner is past that line, and the bare `mongo` tag is
# 8.3, so this database has been dead on arrival here; what differed between
# lanes was only whether anything noticed. Pin 7: it predates the broken
# allocator. Revisit when a MongoDB release ships the fixed one -- 7.0 is the
# oldest series docker-library still publishes, so this has a shelf life.
docker_pull_retry mongo:7
# No --rm, for the same reason the gin-mongo lane dropped it: when this
# container dies its exit code and its logs are the only account of why, and
# --rm deletes both at the instant they matter. The cleanup() above removes it
# by name, and the workflow's `if: always()` step reaps by `docker ps -aq`
# even if this shell is killed outright.
docker run --name "$DB_CONTAINER" \
  --net "$NETWORK_NAME" --network-alias mongo \
  -p "${DB_PORT}:27017" -d mongo:7

# ...and then WAIT for it, because this lane could not tell a dead database
# from a working one. MongoDB has been exiting on arrival here (see above),
# and the suite still reported 10/10 PASSED: every request 500'd, keploy
# recorded the 500s faithfully and replayed them faithfully, so the lane was
# proving only that the app stays broken the same way. A test set recorded
# against a dead dependency is worse than a red lane -- it is a green one.
mongo_postmortem() {
    echo "--- docker ps -a (this job's containers) ---"
    docker ps -a --filter "name=${DB_CONTAINER}" || true
    echo "--- container state ---"
    docker inspect "$DB_CONTAINER" \
      --format 'status={{.State.Status}} exit={{.State.ExitCode}} oom={{.State.OOMKilled}} error={{printf "%q" .State.Error}} started={{.State.StartedAt}} finished={{.State.FinishedAt}}' 2>&1 || true
    echo "--- container logs ---"
    docker logs --tail 200 "$DB_CONTAINER" 2>&1 | tail -40 || true
    echo "--- readiness probe ---"
    # Asked of the IMAGE, not the container: when the database has already
    # exited, `docker exec` can only answer "not running", which is the one
    # case where this question matters most. The wait probes with mongosh, so
    # an image without it fails the wait against a healthy database.
    docker run --rm --entrypoint sh mongo:7 -c 'command -v mongosh || echo "mongosh IS NOT IN THIS IMAGE (the wait cannot pass, whatever the database is doing)"' 2>&1 || true
    echo "--- image platform ---"
    docker image inspect mongo:7 --format '{{.Os}}/{{.Architecture}}' 2>&1 || true
}

echo "Waiting for MongoDB to accept connections..."
for i in $(seq 1 30); do
    if docker exec "$DB_CONTAINER" mongosh --quiet --eval 'db.runCommand({ping:1}).ok' >/dev/null 2>&1; then
        echo "MongoDB is up after $i attempt(s)."
        break
    fi
    # Only states that mean DEAD end the wait: Docker Desktop keeps its daemon
    # in a VM and can fail to answer for a moment, and `docker inspect` prints
    # a bare newline to stdout when it cannot, so "could not ask" must not read
    # as "the database is dead".
    if state=$(docker inspect "$DB_CONTAINER" --format '{{.State.Status}}' 2>/dev/null); then
        state=$(printf '%s' "$state" | tr -d '[:space:]')
        case "$state" in
            exited|dead|removing)
                echo "::error::MongoDB ${state} before it ever accepted a connection, so the app cannot reach its database and this run would record an outage instead of a test."
                mongo_postmortem
                exit 1
                ;;
        esac
    fi
    if [ "$i" -eq 30 ]; then
        echo "::error::MongoDB never came up, so the app cannot reach its database and this run would record an outage instead of a test."
        mongo_postmortem
        exit 1
    fi
    sleep 2
done

# --- Prepare app image & keploy config ---
rm -rf keploy/  # Clean up old test data
rm ./keploy.yml >/dev/null 2>&1 || true

docker_build_retry docker build -t $APP_IMAGE .

# Update the global noise to ts in the config file.
config_file="./keploy.yml"
# Keploy's config now carries only the settings that DIFFER from its
# defaults, so patching a default value out of the generated file with
# `sed` silently patched nothing: the noise rule vanished and every
# replay diffed on the fields it was meant to mask. Write what this
# test needs instead of editing what the generator happened to print.
cat > "$config_file" <<'KEPLOY_CFG'
test:
  globalNoise:
      global: {"body": {"ts":[]}}
KEPLOY_CFG

sleep 5

send_request_and_shutdown() {
  local container_name="${1:-}"
  # Wait for the app to be ready
  for i in {1..10}; do
    if curl --silent --fail http://localhost:$APP_PORT/students >/dev/null 2>&1; then
      echo "Application is up. Sending requests..."
      break
    fi
    echo "Waiting for application to start..."
    sleep 3
  done

  # Exercise endpoints to produce testcases & mocks
  curl -sS -X POST -H "Content-Type: application/json" \
    -d '{"student_id":"12345","name":"John Doe","age":20}' http://localhost:$APP_PORT/students >/dev/null
  curl -sS -X POST -H "Content-Type: application/json" \
    -d '{"student_id":"12346","name":"Alice Green","age":22}' http://localhost:$APP_PORT/students >/dev/null
  curl -sS http://localhost:$APP_PORT/students >/dev/null
  curl -sS -X PUT -H "Content-Type: application/json" \
    -d '{"name":"Jane Smith","age":21}' http://localhost:$APP_PORT/students/12345 >/dev/null
  curl -sS http://localhost:$APP_PORT/students >/dev/null
  curl -sS -X DELETE http://localhost:$APP_PORT/students/12345 >/dev/null

}

do_record_iteration() {
  local i="$1"
  local extra_flags="${2:-}"
  local label="${extra_flags:+_json}"
  local container_name="${APP_CONTAINER}_${i}${label}"

  send_request_and_shutdown "$container_name" &

  # shellcheck disable=SC2086
  "$RECORD_BIN" record $extra_flags \
    -c "docker run -p $APP_PORT:$APP_PORT --net $NETWORK_NAME --rm --name $container_name $APP_IMAGE" \
    --container-name "$container_name" \
    --generate-github-actions=false \
    --proxy-port $PROXY_PORT \
    --dns-port $DNS_PORT \
    --keploy-container "$KEPLOY_CONTAINER" \
    --record-timer=40s 2>&1 | tee "${container_name}.txt"

  if grep -q "WARNING: DATA RACE" "${container_name}.txt"; then
    echo "Race condition detected during record (${container_name})"
    cat "${container_name}.txt"
    exit 1
  fi

  echo "Successfully recorded test case and mocks for iteration ${i}${label:+ (json)}"
}

# --- Record sessions ---
for i in 1 2; do
  do_record_iteration "$i"
done

# shellcheck disable=SC1091
source "${GITHUB_WORKSPACE:-${PWD%/samples-*}}/.github/workflows/test_workflow_scripts/json-pass-helpers.sh"

if json_pass_supported; then
  for i in 1 2; do
    do_record_iteration "$i" "--storage-format json"
  done
fi

# --- Stop Mongo before test ---
echo "Shutting down mongo before test mode..."
docker stop $DB_CONTAINER >/dev/null 2>&1 || true

# --- Test phase ---
test_container="${APP_CONTAINER}_test_1"
echo "Starting test mode..."
"$REPLAY_BIN" test \
  -c "docker run --rm -p $APP_PORT:$APP_PORT --net $NETWORK_NAME --name $test_container $APP_IMAGE" \
  --container-name "$test_container" \
  --apiTimeout 60 \
  --delay 12 \
  --proxy-port $PROXY_PORT \
  --dns-port $DNS_PORT \
  --keploy-container "$KEPLOY_CONTAINER" \
  --generate-github-actions=false 2>&1 | tee "${test_container}.txt"

if grep -q "WARNING: DATA RACE" "${test_container}.txt"; then
    echo "Race condition detected during test (${test_container})"
    cat "${test_container}.txt"
    exit 1
fi

# if grep "ERROR" "${test_container}.txt"; then
#   echo "Error found while running test pipeline..."
#   cat "${test_container}.txt"
#   exit 1
# fi

# --- Verify reports ---
all_passed=true
sleep 2 # Give a moment for the report file to be written
for i in 0 1; do
  report_file="./keploy/reports/test-run-0/test-set-$i-report.yaml"
  if [ -f "$report_file" ]; then
    test_status="$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')"
    echo "Test status for test-set-$i: $test_status"
    if [ "$test_status" != "PASSED" ]; then
      all_passed=false
      echo "Test-set-$i did not pass."
      break
    fi
  else
    all_passed=false
    echo "Report not found: $report_file"
    break
  fi
done

if ! $all_passed; then
  echo "Some tests failed"
  exit 1
fi

if json_pass_supported; then
  test_container_json="${APP_CONTAINER}_test_json"
  "$REPLAY_BIN" test --storage-format json \
    -c "docker run --rm -p $APP_PORT:$APP_PORT --net $NETWORK_NAME --name $test_container_json $APP_IMAGE" \
    --container-name "$test_container_json" \
    --apiTimeout 60 \
    --delay 12 \
    --proxy-port $PROXY_PORT \
    --dns-port $DNS_PORT \
    --keploy-container "$KEPLOY_CONTAINER" \
    --generate-github-actions=false 2>&1 | tee "${test_container_json}.txt"

  if grep -q "WARNING: DATA RACE" "${test_container_json}.txt"; then
    cat "${test_container_json}.txt"
    exit 1
  fi
  if ! json_scan_reports; then
    cat "${test_container_json}.txt"
    exit 1
  fi
  echo "All tests passed (yaml + json)"
else
  echo "All tests passed (yaml only — json pass skipped for compat-matrix cell)"
fi