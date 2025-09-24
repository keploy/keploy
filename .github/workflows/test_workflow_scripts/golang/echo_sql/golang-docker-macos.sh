#!/usr/bin/env bash
# macOS variant for echo-sql (docker compose). Works with both Compose V2 and legacy docker-compose.

set -euo pipefail

# Ensure Homebrew bins are visible on self-hosted macOS runners
export PATH="/opt/homebrew/bin:/usr/local/bin:$PATH"

# Per-run HOME isolation to avoid collisions on a single runner
export KEPLOY_HOME_ROOT="${TMPDIR:-/tmp}/keploy-run-${GITHUB_RUN_ID:-$$}-${GITHUB_JOB:-echo-sql}-$(date +%s)"
export HOME="$KEPLOY_HOME_ROOT/home"
mkdir -p "$HOME"

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

# ---- Detect Compose (V2 plugin or legacy) ----
if docker compose version >/dev/null 2>&1; then
  COMPOSE=("docker" "compose")
elif command -v docker-compose >/dev/null 2>&1; then
  COMPOSE=("docker-compose")
else
  echo "::error::Docker Compose not found.
- If using Docker Desktop: enable 'Use Docker Compose V2' and ensure CLI plugin is installed.
- If using Colima: brew install docker-compose, then:
    mkdir -p ~/.docker/cli-plugins
    ln -sf \"$(command -v docker-compose)\" ~/.docker/cli-plugins/docker-compose
"
  exit 1
fi

# ---- Build images ----
"${COMPOSE[@]}" build

# Clean any preexisting keploy artifacts
sudo rm -rf keploy/

# Generate keploy config
sudo -E env HOME="$HOME" PATH="$PATH" "$RECORD_BIN" config --generate

# Tweak global noise -> ts
config_file="./keploy.yml"
# BSD sed requires a backup suffix; '' means "no backup file"
sed -i '' 's/global: {}/global: {"body": {"ts":[]}}/' "$config_file"

container_kill() {
  pid="$(pgrep -n keploy || true)"
  if [ -n "${pid}" ]; then
    echo "$pid Keploy PID"
    echo "Killing keploy"
    sudo kill "$pid" || true
  fi
}

send_request() {
  sleep 10
  app_started=false
  while [ "$app_started" = false ]; do
    if curl -fsS -X GET http://localhost:8082/health >/dev/null 2>&1; then
      app_started=true
    else
      sleep 3
    fi
  done
  echo "App started"

  curl -fsS --request POST \
    --url http://localhost:8082/url \
    --header 'content-type: application/json' \
    --data '{"url":"https://google.com"}' >/dev/null

  curl -fsS --request POST \
    --url http://localhost:8082/url \
    --header 'content-type: application/json' \
    --data '{"url":"https://facebook.com"}' >/dev/null

  curl -fsS -X GET http://localhost:8082/health >/dev/null

  sleep 5
  container_kill
  wait || true
}

for i in {1..2}; do
  container_name="echoApp"
  send_request &
  sudo -E env HOME="$HOME" PATH="$PATH" \
    "$RECORD_BIN" record \
    -c "$(${COMPOSE[@]} config >/dev/null 2>&1 && echo "${COMPOSE[*]} up")" \
    --container-name "$container_name" \
    --generateGithubActions=false \
    &> "${container_name}.txt" || true

  if grep -q "WARNING: DATA RACE" "${container_name}.txt"; then
    echo "Race condition detected in recording, stopping pipeline..."
    cat "${container_name}.txt"
    exit 1
  fi
  if grep -q "ERROR" "${container_name}.txt"; then
    echo "Error found in pipeline..."
    cat "${container_name}.txt"
    exit 1
  fi
  sleep 5
  echo "Recorded test case and mocks for iteration ${i}"
done

echo "Shutting down services before test mode..."
"${COMPOSE[@]}" down
echo "Services stopped - Keploy should now use mocks"

test_container="echoApp"
sudo -E env HOME="$HOME" PATH="$PATH" \
  "$REPLAY_BIN" test \
  -c "$(${COMPOSE[@]} config >/dev/null 2>&1 && echo "${COMPOSE[*]} up")" \
  --containerName "$test_container" \
  --apiTimeout 60 \
  --delay 20 \
  --generate-github-actions=false \
  &> "${test_container}.txt" || true

if grep -q "ERROR" "${test_container}.txt"; then
  echo "Error found in pipeline..."
  cat "${test_container}.txt"
  exit 1
fi

if grep -q "WARNING: DATA RACE" "${test_container}.txt"; then
  echo "Race condition detected in test, stopping pipeline..."
  cat "${test_container}.txt"
  exit 1
fi

all_passed=true
for i in {0..1}; do
  report_file="./keploy/reports/test-run-0/test-set-$i-report.yaml"
  test_status="$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')"
  echo "Test status for test-set-$i: ${test_status:-UNKNOWN}"
  if [ "${test_status:-FAILED}" != "PASSED" ]; then
    all_passed=false
    echo "Test-set-$i did not pass."
    break
  fi
done

if [ "$all_passed" = true ]; then
  echo "All tests passed"
  exit 0
else
  cat "${test_container}.txt"
  exit 1
fi
