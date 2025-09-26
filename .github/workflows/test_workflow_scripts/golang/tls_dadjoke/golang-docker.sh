#!/bin/bash
# CI: build, record with Keploy, then test with captured mocks — with FULL live logs

set -Eeuo pipefail

# --- Sanity checks ---
[ -x "${RECORD_BIN:-}" ] || { echo "::error::RECORD_BIN not set or not executable"; exit 1; }
[ -x "${REPLAY_BIN:-}" ] || { echo "::error::REPLAY_BIN not set or not executable"; exit 1; }
command -v docker >/dev/null 2>&1 || { echo "::error::docker not found"; exit 1; }
command -v curl   >/dev/null 2>&1 || { echo "::error::curl not found"; exit 1; }
command -v nc >/dev/null 2>&1 || { echo "::warning::nc (netcat) not found; port checks will use curl."; }

# --- Minimal log sections (no ::group:: so nothing is collapsed) ---
section() { echo -e "\n====== $* ======\n"; }

# --- Helpers to stream logs to console + file (line-buffered) ---
run_stream() {               # foreground
  local log="$1"; shift
  stdbuf -oL -eL "$@" 2>&1 | tee "$log"
}
run_bg_stream() {            # background; echoes PID of tee
  local log="$1"; shift
  stdbuf -oL -eL "$@" 2>&1 | tee "$log" &
  echo $!
}

# --- Cleanup & error handling ---
cleanup() {
  section "Final Cleanup"
  docker stop jokeApp 2>/dev/null || true
  docker rm   jokeApp 2>/dev/null || true
  docker stop jokeApp_test 2>/dev/null || true
  docker rm   jokeApp_test 2>/dev/null || true
  echo "Cleanup complete."
}
die() {
  local rc=$?
  echo "::error::Pipeline failed on line ${BASH_LINENO[0]} (exit code=$rc)."
  section "Record Log (tail)"
  [ -f record.log ] && tail -n 200 record.log || echo "record.log not found."
  section "Test Log (tail)"
  [ -f test.log ] && tail -n 200 test.log || echo "test.log not found."
  exit "$rc"
}
trap die ERR
trap cleanup EXIT

# --- Wait for HTTP port ---
wait_for_http() {
  local port="$1" host="localhost"
  section "Waiting for application on port $port..."
  for i in {1..30}; do
    if command -v nc >/dev/null 2>&1; then
      if nc -z "$host" "$port" >/dev/null 2>&1; then
        echo "✅ Port $port is open."
        return 0
      fi
    else
      if curl -s "http://$host:$port/health" >/dev/null 2>&1 || curl -s "http://$host:$port" >/dev/null 2>&1; then
        echo "✅ Port $port responded."
        return 0
      fi
    fi
    echo "Waiting... ($i/30)"; sleep 2
  done
  echo "::error::App did not come up on :$port in time."; return 1
}

# --- Generate traffic for recording ---
generate_traffic() {
  section "Generating HTTP Traffic"
  curl -s -o /dev/null -X GET http://localhost:8080/joke
  curl -s -o /dev/null -X GET http://localhost:8080/joke
  echo "Traffic generation complete."
}

# --- Log checks ---
check_for_errors() {
  local logfile=$1
  section "Checking $logfile for errors"
  if [ -f "$logfile" ] && (grep -q "ERROR" "$logfile" || grep -q "WARNING: DATA RACE" "$logfile"); then
    echo "::error::Critical error or race detected in $logfile"
    return 1
  fi
  echo "No critical errors found in $logfile."
}
check_test_report() {
  section "Checking Test Reports"
  local latest_run_dir
  latest_run_dir=$(ls -td ./keploy/reports/test-run-* 2>/dev/null | head -n 1 || true)
  [ -n "${latest_run_dir:-}" ] || { echo "::error::Test report directory not found"; return 1; }
  local all_passed=true
  shopt -s nullglob
  for report_file in "$latest_run_dir"/test-set-*-report.yaml; do
    local status
    status=$(grep -m1 'status:' "$report_file" | awk '{print $2}')
    echo "Status for $(basename "$report_file"): $status"
    [ "$status" = "PASSED" ] || all_passed=false
  done
  $all_passed && echo "✅ All test sets passed." || { echo "::error::One or more test sets failed."; return 1; }
}

# --- Build ---
section "Build Docker Image"
docker build -t go-joke-app .

# --- Config ---
section "Setup & Configuration"
sudo -E env PATH=$PATH "$RECORD_BIN" config --generate
rm -rf keploy/

# --- Record (live logs) ---
section "Start Recording"
# Start Keploy record; stream stdout/err to console and record.log
KEPLOY_BG_PID=$(run_bg_stream record.log \
  sudo -E env PATH=$PATH "$RECORD_BIN" record \
    -c "docker run -p8080:8080 --rm --name jokeApp --network keploy-network go-joke-app" \
    --container-name jokeApp)
echo "Keploy record (tee) PID: $KEPLOY_BG_PID"

wait_for_http 8080
generate_traffic
echo "Waiting for recordings to flush..."; sleep 5

section "Stop Recording"
# Stop the app container; keploy will exit after the wrapped command ends
docker stop jokeApp || true
wait "$KEPLOY_BG_PID" || true
echo "Recording stopped."
check_for_errors record.log

# --- Test (live logs) ---
section "Start Testing with Mocks"
run_stream test.log \
  sudo -E env PATH=$PATH "$REPLAY_BIN" test \
    -c "docker run -p8080:8080 --rm --name jokeApp_test --network keploy-network go-joke-app" \
    --containerName jokeApp_test \
    --apiTimeout 60 --delay 20 --generate-github-actions=false
echo "Test run finished."

check_for_errors test.log
check_test_report

echo -e "\n✅ All tests passed successfully!"
