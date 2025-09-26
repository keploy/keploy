#!/usr/bin/env bash
# macOS variant for echo-sql (docker compose). Uses BSD sed.
set -euo pipefail

# Safely source (don’t fail if missing)
IID_FILE="./../../.github/workflows/test_workflow_scripts/test-iid-macos.sh"
if [ -f "$IID_FILE" ]; then
  # shellcheck source=/dev/null
  source "$IID_FILE"
else
  echo "⚠️  $IID_FILE not found; continuing without it."
fi

# Build Docker Image(s)
docker compose build

# Remove any preexisting keploy tests and mocks.
rm -rf keploy/
rm ./keploy.yml >/dev/null 2>&1 || true

# Generate the keploy-config file.
"$RECORD_BIN" config --generate

# Update the global noise to ts in the config file.
config_file="./keploy.yml"
if [ -f "$config_file" ]; then
  sed -i '' 's/global: {}/global: {"body": {"ts":[]}}/' "$config_file" || true
else
  echo "⚠️  Config file $config_file not found, skipping sed replace."
fi

send_request(){
  echo "Sending requests to the application..."
  sleep 10
  local app_started=false
  while [ "$app_started" = false ]; do
    if curl -sS -X GET http://localhost:8082/health >/dev/null; then
      app_started=true
    else
      sleep 3
    fi
  done
  echo "App started"

  curl -sS --request POST \
    --url http://localhost:8082/url \
    --header 'content-type: application/json' \
    --data '{"url":"https://google.com"}' >/dev/null

  curl -sS --request POST \
    --url http://localhost:8082/url \
    --header 'content-type: application/json' \
    --data '{"url":"https://facebook.com"}' >/dev/null

  curl -sS -X GET http://localhost:8082/health >/dev/null
  sleep 3
  echo "Requests sent successfully."
}

for i in {1..2}; do
  container_name="echoApp"
  send_request &

  # Stream + save logs; no need to cat later
  ("$RECORD_BIN" record -c "docker compose up" \
     --container-name "$container_name" \
     --generateGithubActions=false \
     --record-timer=16s) 2>&1 | tee "${container_name}.txt"

  if grep -q "WARNING: DATA RACE" "${container_name}.txt"; then
    echo "❌ Data race detected in recording"; exit 1
  fi
  if grep -q "ERROR" "${container_name}.txt"; then
    echo "❌ Error found in recording"; exit 1
  fi

  sleep 5
  echo "✅ Recorded test case and mocks for iteration ${i}"
done

# Shutdown services before test mode - Keploy should use mocks for dependencies
echo "Shutting down docker compose services before test mode..."
docker compose down
echo "Services stopped - Keploy should now use mocks for dependency interactions"

# Start keploy in test mode (stream + save logs)
test_container="echoApp"
("$REPLAY_BIN" test \
   -c 'docker compose up' \
   --containerName "$test_container" \
   --apiTimeout 60 \
   --delay 10 \
   --generate-github-actions=false || true) 2>&1 | tee "${test_container}.txt"

if grep -q "ERROR" "${test_container}.txt"; then
  echo "❌ Error found in test"; exit 1
fi
if grep -q "WARNING: DATA RACE" "${test_container}.txt"; then
  echo "❌ Data race detected in test"; exit 1
fi

all_passed=true
for i in {0..1}; do
  report_file="./keploy/reports/test-run-0/test-set-$i-report.yaml"
  i
