#!/usr/bin/env bash
# macOS variant for echo-sql (docker compose). Works with Compose v2 or v1.
set -euo pipefail

# --- Compose detection (v2 -> v1 fallback) ---
if docker compose version >/dev/null 2>&1; then
  DOCKER_COMPOSE=(docker compose)
elif command -v docker-compose >/dev/null 2>&1; then
  DOCKER_COMPOSE=(docker-compose)
else
  echo "❌ Docker Compose not found. Install Docker Desktop (v2) or 'brew install docker-compose' for v1."
  exit 1
fi

# --- Sanity checks ---
command -v docker >/dev/null 2>&1 || { echo "❌ docker CLI not found"; exit 1; }
[ -x "${RECORD_BIN:-}" ] || { echo "❌ RECORD_BIN not set or not executable"; exit 1; }
[ -x "${REPLAY_BIN:-}" ] || { echo "❌ REPLAY_BIN not set or not executable"; exit 1; }

# --- Isolate keploy home per run (avoid cross-job collisions) ---
export KEPLOY_HOME_ROOT="${TMPDIR:-/tmp}/keploy-run-${GITHUB_RUN_ID:-$$}-${GITHUB_JOB:-echo-sql}-$(date +%s)"
export HOME="$KEPLOY_HOME_ROOT/home"
mkdir -p "$HOME"

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

# --- Build Docker Image(s) ---
"${DOCKER_COMPOSE[@]}" build

# --- Clean previous keploy artifacts ---
sudo rm -rf keploy/

# --- Generate keploy config ---
sudo -E env HOME="$HOME" PATH="$PATH" "$RECORD_BIN" config --generate

# --- Update global noise -> ts (BSD sed) ---
config_file="./keploy.yml"
sed -i '' 's/global: {}/global: {"body": {"ts":[]}}/' "$config_file"

container_kill() {
  pid=$(pgrep -n keploy || true)
  if [ -n "$pid" ]; then
    echo "🔪 Killing keploy (pid $pid)"
    sudo kill "$pid" || true
  fi
}

send_request() {
  sleep 10
  app_started=false
  while [ "$app_started" = false ]; do
    if curl -sf -X GET http://localhost:8082/health >/dev/null; then
      app_started=true
    else
      sleep 3
    fi
  done
  echo "✅ App started"

  curl --request POST \
    --url http://localhost:8082/url \
    --header 'content-type: application/json' \
    --data '{"url":"https://google.com"}'

  curl --request POST \
    --url http://localhost:8082/url \
    --header 'content-type: application/json' \
    --data '{"url":"https://facebook.com"}'

  curl -sf -X GET http://localhost:8082/health >/dev/null || true

  # Give keploy time to record and then stop it
  sleep 5
  container_kill
  wait || true
}

for i in {1..2}; do
  container_name="echoApp"
  send_request &
  sudo -E env HOME="$HOME" PATH="$PATH" "$RECORD_BIN" record \
    -c "$("${DOCKER_COMPOSE[@]}" up; echo)" \
    --container-name "$container_name" \
    --generateGithubActions=false &> "${container_name}.txt" || true

  if grep -q "WARNING: DATA RACE" "${container_name}.txt"; then
    echo "❌ Race condition detected in recording"
    cat "${container_name}.txt"
    exit 1
  fi
  if grep -q "ERROR" "${container_name}.txt"; then
    echo "❌ Error found in recording"
    cat "${container_name}.txt"
    exit 1
  fi

  sleep 5
  echo "📀 Recorded test case and mocks for iteration ${i}"
done

# --- Stop services before test mode (mocks should be used) ---
echo "🧹 Bringing down compose services before test mode..."
"${DOCKER_COMPOSE[@]}" down || true

# --- Test mode ---
test_container="echoApp"
sudo -E env HOME="$HOME" PATH="$PATH" "$REPLAY_BIN" test \
  -c "$("${DOCKER_COMPOSE[@]}" up; echo)" \
  --containerName "$test_container" \
  --apiTimeout 60 \
  --delay 20 \
  --generate-github-actions=false &> "${test_container}.txt" || true

if grep -q "ERROR" "${test_container}.txt"; then
  echo "❌ Error found in test"
  cat "${test_container}.txt"
  exit 1
fi

if grep -q "WARNING: DATA RACE" "${test_container}.txt"; then
  echo "❌ Race condition detected in test"
  cat "${test_container}.txt"
  exit 1
fi

# --- Verify reports ---
all_passed=true
for i in {0..1}; do
  report_file="./keploy/reports/test-run-0/test-set-$i-report.yaml"
  if [ ! -f "$report_file" ]; then
    echo "❌ Missing report: $report_file"
    all_passed=false
    break
  fi
  test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
  echo "🔎 Test status for test-set-$i: $test_status"
  if [ "$test_status" != "PASSED" ]; then
    all_passed=false
    echo "❌ Test-set-$i did not pass."
    break
  fi
done

if [ "$all_passed" = true ]; then
  echo "✅ All tests passed"
  exit 0
else
  cat "${test_container}.txt"
  exit 1
fi
