#!/usr/bin/env bash

set -Eeuo pipefail

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

section() { echo "::group::$*"; }
endsec() { echo "::endgroup::"; }

dump_logs() {
  section "Record Logs"
  for logfile in record-*.txt; do
    [ -e "$logfile" ] || continue
    echo "--- $logfile ---"
    cat "$logfile"
  done
  endsec

  section "Replay Log"
  cat test.txt 2>/dev/null || echo "Replay log not found."
  endsec
}

stop_keploy_record() {
  local rec_pid
  rec_pid="$(pgrep -n -f 'keploy record' || true)"
  echo "Keploy record PID: ${rec_pid:-not-found}"
  if [[ -n "${rec_pid:-}" ]]; then
    sudo kill -INT "$rec_pid" 2>/dev/null || true
  fi
}

cleanup_compose() {
  docker compose down -v --remove-orphans >/dev/null 2>&1 || true
}

final_cleanup() {
  local rc=$?
  if [[ $rc -ne 0 ]]; then
    echo "::error::http-postgres pipeline failed (exit code=$rc)"
    dump_logs
  fi
  stop_keploy_record
  cleanup_compose
}

trap final_cleanup EXIT

check_for_errors() {
  local logfile=$1
  echo "Checking $logfile for critical Keploy errors..."

  if [ ! -f "$logfile" ]; then
    echo "::error::Log file not found: $logfile"
    return 1
  fi

  if grep "ERROR" "$logfile" | grep "Keploy:" | grep -v "failed to read symbols, skipping coverage calculation"; then
    echo "::error::Critical Keploy errors found in $logfile"
    return 1
  fi

  if grep -q "WARNING: DATA RACE" "$logfile"; then
    echo "::error::Data race detected in $logfile"
    return 1
  fi

  echo "No critical errors found in $logfile."
}

check_test_report() {
  echo "Checking test reports..."

  if [ ! -d "./keploy/reports" ]; then
    echo "::error::Test report directory not found."
    return 1
  fi

  local latest_report_dir
  latest_report_dir=$(ls -td ./keploy/reports/test-run-* 2>/dev/null | head -n 1)
  if [ -z "$latest_report_dir" ]; then
    echo "::error::No test run directory found in ./keploy/reports/"
    return 1
  fi

  local all_passed=true
  local report_file=""
  for report_file in "$latest_report_dir"/test-set-*-report.yaml; do
    [ -e "$report_file" ] || { echo "::error::No report files found."; return 1; }

    local test_set_name
    test_set_name=$(basename "$report_file" -report.yaml)
    local test_status
    test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')

    echo "Status for ${test_set_name}: $test_status"
    if [ "$test_status" != "PASSED" ]; then
      all_passed=false
      echo "::error::${test_set_name} did not pass."
    fi
  done

  if [ "$all_passed" = false ]; then
    return 1
  fi

  echo "All test reports passed."
}

wait_for_http() {
  local port="${1:-8080}"
  local path="${2:-/companies}"
  local host="${3:-127.0.0.1}"
  local timeout_s="${4:-90}"

  section "Waiting for application on $host:$port$path"
  for ((i = 1; i <= timeout_s; i++)); do
    if curl -fsS --max-time 1 "http://$host:$port$path" -o /dev/null; then
      echo "Application is ready on $host:$port$path"
      endsec
      return 0
    fi
    sleep 1
  done

  echo "::error::Application did not become available on $host:$port$path in time."
  docker compose ps || true
  docker compose logs api || true
  endsec
  return 1
}

run_record_iteration() {
  local iteration="$1"
  local traffic_script="$2"
  local logfile="record-${iteration}.txt"

  section "Recording iteration ${iteration} with $(basename "$traffic_script")"
  cleanup_compose

  "$RECORD_BIN" record -c "docker compose up" --container-name "api" --generateGithubActions=false 2>&1 | tee "$logfile" &
  local record_pid=$!
  echo "Started Keploy record pipeline with PID: $record_pid"

  wait_for_http 8080 /companies 127.0.0.1 90

  echo "Running traffic script: $traffic_script"
  bash "$traffic_script"

  sleep 5

  section "Stopping record iteration ${iteration}"
  stop_keploy_record
  wait "$record_pid" || true
  endsec

  check_for_errors "$logfile"
  endsec
}

section "Initializing Environment"
echo "RECORD_BIN: $RECORD_BIN"
echo "REPLAY_BIN: $REPLAY_BIN"
rm -rf keploy/ record-*.txt test.txt
cleanup_compose
docker compose build
"$RECORD_BIN" config --generate
endsec

run_record_iteration 0 ./test.sh
run_record_iteration 1 ./test_projects.sh

section "Preparing Replay"
cleanup_compose
echo "Recording complete. Starting replay with generated test sets."
endsec

section "Replaying Tests"
"$REPLAY_BIN" test -c 'docker compose up' --containerName "api" --apiTimeout 60 --delay 20 --generate-github-actions=false 2>&1 | tee test.txt || true
check_for_errors test.txt
check_test_report
endsec

echo "http-postgres pipeline completed successfully."
