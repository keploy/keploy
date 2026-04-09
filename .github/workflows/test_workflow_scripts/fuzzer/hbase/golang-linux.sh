#!/usr/bin/env bash

# Keploy record/test wrap the long-running HBase fuzzer server.
# The incoming testcase is the POST /run request; the JSON body selects the
# fuzzer's own mode. Use mode=record while Keploy is recording, then mode=replay
# for the stronger replay check while Keploy test is active.

set -Eeuo pipefail

section() { echo "::group::$*"; }
endsec() { echo "::endgroup::"; }

dump_logs() {
  section "Record Log"
  cat record.txt 2>/dev/null || echo "Record log not found."
  endsec
  section "Test Log"
  cat test.txt 2>/dev/null || echo "Test log not found."
  endsec
}

check_for_errors() {
  local logfile=$1
  echo "Checking for errors in $logfile..."
  if [[ -f "$logfile" ]]; then
    if grep "ERROR" "$logfile" | grep "Keploy:" | grep -v "failed to read symbols, skipping coverage calculation"; then
      echo "::error::Critical error found in $logfile. Failing the build."
      echo "--- Failing Errors ---"
      grep "ERROR" "$logfile" | grep "Keploy:" | grep -v "failed to read symbols, skipping coverage calculation"
      echo "----------------------"
      exit 1
    fi
    if grep -q "WARNING: DATA RACE" "$logfile"; then
      echo "::error::Race condition detected in $logfile"
      exit 1
    fi
  fi
  echo "No critical errors in $logfile."
}

check_test_report() {
  echo "Checking test reports..."
  if [[ ! -d "./keploy/reports" ]]; then
    echo "Test report directory not found!"
    return 1
  fi

  local latest_report_dir
  latest_report_dir=$(ls -td ./keploy/reports/test-run-* 2>/dev/null | head -n 1 || true)
  if [[ -z "$latest_report_dir" ]]; then
    echo "No test run directory found in ./keploy/reports/"
    return 1
  fi

  local all_passed=true
  for report_file in "$latest_report_dir"/test-set-*-report.yaml; do
    [[ -e "$report_file" ]] || { echo "No report files found."; all_passed=false; break; }

    local test_set_name
    test_set_name=$(basename "$report_file" -report.yaml)
    local test_status
    test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')

    echo "Status for ${test_set_name}: $test_status"
    if [[ "$test_status" != "PASSED" ]]; then
      all_passed=false
      echo "Test set ${test_set_name} did not pass."
    fi
  done

  if [[ "$all_passed" == false ]]; then
    echo "One or more test sets failed."
    return 1
  fi

  echo "All tests passed in reports."
  return 0
}

wait_for_port() {
  local port="$1"
  section "Waiting for port $port..."
  for _ in {1..120}; do
    if nc -z localhost "$port" >/dev/null 2>&1; then
      echo "✅ Port $port is open."
      endsec
      return 0
    fi
    sleep 1
  done
  echo "::error::Port $port did not become available in time."
  endsec
  return 1
}

ensure_hbase_host_alias() {
  if getent hosts hbase >/dev/null 2>&1 && getent hosts hbase | grep -q '127\.0\.0\.1'; then
    echo "✅ hbase already resolves to 127.0.0.1"
    return 0
  fi
  echo "127.0.0.1 hbase" | sudo tee -a /etc/hosts >/dev/null
  echo "✅ Added hbase -> 127.0.0.1 to /etc/hosts"
}

trigger_run() {
  local mode="$1"
  local outfile="$2"
  curl -fsS -X POST http://localhost:18091/run \
    -H "Content-Type: application/json" \
    -d "$(cat <<EOF
{
  "addr": "127.0.0.1:2181",
  "seed": 42,
  "total_ops": 2000,
  "timeout_sec": 600,
  "mode": "$mode",
  "golden_path": "./golden/hbase_golden.json",
  "drop_tables_first": true,
  "table_prefix": "kfuzz",
  "max_sessions": 4,
  "max_diffs": 20,
  "op_timeout_ms": 10000
}
EOF
)" | tee "$outfile"
}

cleanup() {
  local rc=$?
  if [[ $rc -ne 0 ]]; then
    echo "::error::Pipeline failed (exit code=$rc). Dumping final logs..."
  else
    section "Script finished successfully. Dumping final logs..."
  fi

  dump_logs

  if [[ -d go-fuzzers/hbase ]]; then
    section "Stopping HBase cluster..."
    (cd go-fuzzers/hbase && docker compose down -v || true)
    endsec
  fi

  if [[ $rc -eq 0 ]]; then
    endsec
  fi
}

trap cleanup EXIT

section "Initializing Environment"
echo "HBASE_FUZZER_BIN: $HBASE_FUZZER_BIN"
echo "RECORD_KEPLOY_BIN: $RECORD_KEPLOY_BIN"
echo "REPLAY_KEPLOY_BIN: $REPLAY_KEPLOY_BIN"
rm -rf keploy/ keploy.yml golden/ record.txt test.txt go-fuzzers/
mkdir -p golden/
sudo chmod +x "$HBASE_FUZZER_BIN"
sudo chown -R "$(whoami)":"$(whoami)" golden
endsec

section "Cloning go-fuzzers Repository"
echo "Cloning keploy/go-fuzzers using PRO_ACCESS_TOKEN..."
if [[ -n "${PRO_ACCESS_TOKEN:-}" ]]; then
  git clone "https://${PRO_ACCESS_TOKEN}@github.com/keploy/go-fuzzers.git"
else
  echo "::warning::PRO_ACCESS_TOKEN not set, attempting clone without auth (will fail for private repo)"
  git clone https://github.com/keploy/go-fuzzers.git
fi
cd go-fuzzers/hbase
GO_FUZZERS_PATH=$(pwd)
echo "Now in directory: $GO_FUZZERS_PATH"
endsec

section "Starting HBase Cluster"
docker compose -f docker-compose.yml up -d
wait_for_port 2181
ensure_hbase_host_alias
endsec

section "Generating Keploy Configuration"
sudo "$RECORD_KEPLOY_BIN" config --generate
sed -i 's/global: {}/global: {"body": {"duration_ms":[]}, "header": {"Content-Length":[]}}/' ./keploy.yml
echo "Keploy config generated and updated."
endsec

section "Start Recording Server"
"$RECORD_KEPLOY_BIN" record -c "$HBASE_FUZZER_BIN -http :18091" 2>&1 | tee record.txt &
RECORD_PIPE_PID=$!
echo "Keploy record process started with PID: $RECORD_PIPE_PID"
endsec

section "Trigger Fuzzer in Record Mode"
wait_for_port 18091
trigger_run record record-response.json
grep -q '"ok":true' record-response.json
grep -q '"mismatches":0' record-response.json
endsec

section "Stop Recording"
REC_PID="$(pgrep -n -f 'keploy record' || true)"
if [[ -n "$REC_PID" ]]; then
  echo "Stopping keploy record process (PID: $REC_PID)..."
  sudo kill -INT "$REC_PID" 2>/dev/null || true
fi
sleep 5
check_for_errors "record.txt"
endsec

section "Shutting Down HBase Cluster For Replay"
docker compose down -v
endsec
sleep 5

section "Start Replay Server"
"$REPLAY_KEPLOY_BIN" test -c "$HBASE_FUZZER_BIN -http :18091" --delay 10 --api-timeout=600 2>&1 | tee test.txt &
TEST_PIPE_PID=$!
echo "Keploy test process started with PID: $TEST_PIPE_PID"
endsec

section "Trigger Fuzzer in Replay Mode"
wait_for_port 18091
trigger_run replay replay-response.json
grep -q '"ok":true' replay-response.json
grep -q '"mismatches":0' replay-response.json
endsec

section "Stop Replay"
TEST_PID="$(pgrep -n -f 'keploy test' || true)"
if [[ -n "$TEST_PID" ]]; then
  echo "Stopping keploy test process (PID: $TEST_PID)..."
  sudo kill -INT "$TEST_PID" 2>/dev/null || true
fi
sleep 5
check_for_errors "test.txt"
check_test_report
endsec
