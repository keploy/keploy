#!/bin/bash

# Expects:
#   MODE                -> 'incoming' or 'outgoing'   (argv[1])
#   RECORD_BIN          -> path to keploy record binary (env)
#   REPLAY_BIN          -> path to keploy test binary   (env)
#   FUZZER_CLIENT_BIN   -> path to downloaded client bin (env)
#   FUZZER_SERVER_BIN   -> path to downloaded server bin (env)

set -Eeuo pipefail

MODE=${1:-incoming}

echo "root ALL=(ALL:ALL) ALL" | sudo tee -a /etc/sudoers
if [ -n "${KEPLOY_CI_API_KEY:-}" ]; then
  echo "📌 Setting up Keploy API Key..."
  export KEPLOY_API_KEY="$KEPLOY_CI_API_KEY"
fi

# If you keep test-iid.sh checks, source it from repo root:
if [ -f "./.github/workflows/test_workflow_scripts/test-iid.sh" ]; then
 # shellcheck disable=SC1091
 source "./.github/workflows/test_workflow_scripts/test-iid.sh"
fi

# sanity
command -v curl >/dev/null 2>&1 || { echo "curl not found"; exit 1; }
[ -x "${RECORD_BIN:-}" ] || { echo "RECORD_BIN not set or not executable"; exit 1; }
[ -x "${REPLAY_BIN:-}" ] || { echo "REPLAY_BIN not set or not executable"; exit 1; }
[ -x "${FUZZER_CLIENT_BIN:-}" ] || { echo "FUZZER_CLIENT_BIN not set or not executable"; exit 1; }
[ -x "${FUZZER_SERVER_BIN:-}" ] || { echo "FUZZER_SERVER_BIN not set or not executable"; exit 1; }

# Generate keploy config and add duration_ms noise to avoid timing diffs
rm -f ./keploy.yml keploy
sudo -E env PATH="$PATH" "$RECORD_BIN" config --generate
config_file="./keploy.yml"
if [ -f "$config_file" ]; then
  sed -i 's/global: {}/global: {"body": {"duration_ms":[]}}/' "$config_file"
else
  echo "⚠️ Config file $config_file not found, skipping sed replace."
fi

SUCCESS_PHRASE="all 1000 unary RPCs validated successfully"

# Validates the Keploy test report to ensure all test sets passed
check_test_report() {
    echo "Checking test reports..."
    if [ ! -d "./keploy/reports" ]; then
        echo "Test report directory not found!"
        return 1
    fi

    local latest_report_dir
    latest_report_dir=$(ls -td ./keploy/reports/test-run-* | head -n 1)
    if [ -z "$latest_report_dir" ]; then
        echo "No test run directory found in ./keploy/reports/"
        return 1
    fi
    
    local all_passed=true
    # Loop through all generated report files
    for report_file in "$latest_report_dir"/test-set-*-report.yaml; do
        [ -e "$report_file" ] || { echo "No report files found."; all_passed=false; break; }

        local test_set_name
        test_set_name=$(basename "$report_file" -report.yaml)
        local test_status
        test_status=$(grep -m 1 'status:' "$report_file" | awk '{print $2}')        
        echo "Status for ${test_set_name}: $test_status"
        if [ "$test_status" != "PASSED" ]; then
            all_passed=false
            echo "Test set ${test_set_name} did not pass."
        fi
    done

    if [ "$all_passed" = false ]; then
        echo "One or more test sets failed."
        return 1
    fi

    echo "All tests passed in reports."
    return 0
}

check_for_errors() {
  local logfile=$1
  echo "Checking for errors in $logfile..."
  if [ -f "$logfile" ]; then
    # Find critical Keploy errors, but exclude specific non-critical ones.
    if grep "ERROR" "$logfile" | grep "Keploy:" | grep -v "failed to read symbols, skipping coverage calculation"; then
      echo "::error::Critical error found in $logfile. Failing the build."
      # Print the specific errors that caused the failure
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
  echo "No critical errors found in $logfile."
}

ensure_success_phrase() {
 for f in "$@"; do
   if [ -f "$f" ] && grep -qiF "$SUCCESS_PHRASE" "$f"; then
     echo "Validation success phrase found in $f"
     return 0
   fi
 done
 echo "‼️ Did not find success phrase: '$SUCCESS_PHRASE' in logs: $*"
 for f in "$@"; do
   [ -f "$f" ] && { echo "--- $f ---"; tail -n +1 "$f"; echo "--------------"; }
 done
 exit 1
}


if [ "$MODE" = "incoming" ]; then
 echo "🧪 Testing with incoming requests"

  # Start server with keploy in record mode
  if [[ "$RECORD_SRC" == "latest" ]]; then
  sudo -E env PATH="$PATH" "$RECORD_BIN" record -c "$FUZZER_SERVER_BIN" --bigPayload 2>&1 | tee record_incoming.txt &
  else
  sudo -E env PATH="$PATH" "$RECORD_BIN" record -c "$FUZZER_SERVER_BIN" 2>&1 | tee record_incoming.txt &
  fi
 
 sleep 10


 # Start client HTTP driver
 "$FUZZER_CLIENT_BIN" --http :18080 2>&1 | tee client_incoming.txt &
 sleep 10


 # Kick off 1000 unary RPC fuzz calls
 curl -sS -X POST http://localhost:18080/run \
   -H 'Content-Type: application/json' \
   -d '{
     "addr": "localhost:50051",
     "seed": 42,
     "total": 1000,
     "text": false,
     "timeout_sec": 60,
     "max_diffs": 5
   }'

 sleep 10

 echo "Stopping keploy record and server"

REC_PID="$(pgrep -n -f 'keploy record' || true)"
echo "$REC_PID Keploy PID"
echo "Killing keploy"
sudo kill -INT "$REC_PID" 2>/dev/null || true

 sleep 5

 echo "Ensuring fuzzer server is stopped..."
 sleep 10
 sudo pkill -f "$FUZZER_SERVER_BIN" || true
 sleep 2 # Give a moment for the port to be released

 echo "Waiting for processes to settle"


 check_for_errors record_incoming.txt
 check_for_errors client_incoming.txt


 echo "Replaying incoming requests"


 # Replay
 sudo -E env PATH="$PATH" "$REPLAY_BIN" test -c "$FUZZER_SERVER_BIN" --api-timeout=200 --skip-coverage=true --disableMockUpload 2>&1 | tee test_incoming.txt
 echo "checking for errors"
 check_for_errors test_incoming.txt
 check_test_report


 # ✅ For INCOMING mode: no success-phrase check. Instead, verify Keploy reports PASSED.
 RUN_DIR=$(ls -1dt ./keploy/reports/test-run-* 2>/dev/null | head -n1 || true)
 if [[ -z "${RUN_DIR:-}" ]]; then
   echo "::error::No test-run directory found under ./keploy/reports"
   exit 1
 fi
 echo "Using reports from: $RUN_DIR"


 all_passed=true
 found_any=false
 for rpt in "$RUN_DIR"/test-set-*-report.yaml; do
   [[ -f "$rpt" ]] || continue
   found_any=true
   status=$(awk '/^status:/{print $2; exit}' "$rpt")
   echo "Test status for $(basename "$rpt"): ${status:-<missing>}"
   [[ "$status" == "PASSED" ]] || all_passed=false
 done


 if ! $found_any; then
   echo "::error::No test-set report files found in $RUN_DIR"
   exit 1
 fi


 if $all_passed; then
   echo "✅ Incoming mode passed."
 else
   echo "::error::One or more test sets failed in $RUN_DIR"
   exit 1
 fi

elif [ "$MODE" = "outgoing" ]; then
 echo "🧪 Testing with outgoing requests"


 # Start server (no keploy here)
 "$FUZZER_SERVER_BIN" &> server_outgoing.txt &
 sleep 5


 # Record the client (it makes outgoing RPCs)
 sudo -E env PATH="$PATH" "$RECORD_BIN" record -c "$FUZZER_CLIENT_BIN --http :18080" 2>&1 | tee record_outgoing.txt &
 sleep 10


 curl -sS -X POST http://localhost:18080/run \
   -H 'Content-Type: application/json' \
   -d '{
     "addr": "localhost:50051",
     "seed": 42,
     "total": 1000,
     "text": false,
     "timeout_sec": 60,
     "max_diffs": 5
   }'


 sleep 10


REC_PID="$(pgrep -n -f 'keploy record' || true)"
echo "$REC_PID Keploy PID"
echo "Killing keploy"
sudo kill -INT "$REC_PID" 2>/dev/null || true

 sleep 5

 echo "Ensuring fuzzer client is stopped..."
 sleep 10
 sudo pkill -f "$FUZZER_CLIENT_BIN" || true
 sleep 2 # Give a moment for the port to be released

 check_for_errors server_outgoing.txt
 check_for_errors record_outgoing.txt


 # Replay the client (relying on mocks)
 sudo -E env PATH="$PATH" "$REPLAY_BIN" test -c "$FUZZER_CLIENT_BIN --http :18080" --skip-coverage=true --disableMockUpload 2>&1 | tee test_outgoing.txt
 echo "checking for errors"
 check_for_errors test_outgoing.txt
 check_test_report
 ensure_success_phrase test_outgoing.txt
 echo "✅ Outgoing mode passed. "

else
 echo "❌ Invalid mode specified: $MODE"
 exit 1
fi

exit 0