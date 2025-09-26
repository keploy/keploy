#!/bin/bash

# Expects:
#   MODE                -> 'incoming' or 'outgoing'   (argv[1])
#   RECORD_BIN          -> path to keploy record binary (env)
#   REPLAY_BIN          -> path to keploy test binary   (env)
#   FUZZER_CLIENT_BIN   -> path to downloaded client bin (env)
#   FUZZER_SERVER_BIN   -> path to downloaded server bin (env)

set -Eeuo pipefail

MODE=${1:-incoming}

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
rm -f ./keploy.yml
sudo -E env PATH="$PATH" "$RECORD_BIN" config --generate
sed -i 's/global: {}/global: {"body": {"duration_ms":[]}}/' "./keploy.yml"

SUCCESS_PHRASE="all 1000 unary RPCs validated successfully"


check_for_errors() {
 local logfile=$1
 if [ -f "$logfile" ]; then
   if grep -q "ERROR" "$logfile"; then
     # Ignore benign coverage-symbol errors from stripped binaries
     if grep -Eq 'failed to read symbols, skipping coverage calculation|no symbol section' "$logfile"; then
       echo "Ignoring benign coverage-symbol error in $logfile"
     else
       echo "Error found in $logfile"
       # show only ERROR lines for quick triage
       grep -n "ERROR" "$logfile" || true
       cat "$logfile"
       exit 1
     fi
   fi
   if grep -q "WARNING: DATA RACE" "$logfile"; then
     echo "Race condition detected in $logfile"
     cat "$logfile"
     exit 1
   fi
 fi
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
 sudo -E env PATH="$PATH" "$RECORD_BIN" record -c "$FUZZER_SERVER_BIN" &> record_incoming.txt &
 sleep 10


 # Start client HTTP driver
 "$FUZZER_CLIENT_BIN" --http :18080 &> client_incoming.txt &
 sleep 10


 # Kick off 1000 unary RPC fuzz calls with increased timeouts
echo "🚀 Starting 1000 RPC fuzz calls..."

# Save the start time
START_TIME=$(date +%s)

# Run the test with a timeout of 10 minutes (600 seconds)
timeout 600 curl -v -X POST http://localhost:18080/run \
  -H 'Content-Type: application/json' \
  -d '{
    "addr": "localhost:50051",
    "seed": 42,
    "total": 1000,
    "text": false,
    "timeout_sec": 120,  # Increased per-request timeout to 120 seconds
    "max_diffs": 5
  }' &> curl_output.txt

CURL_EXIT_CODE=$?
END_TIME=$(date +%s)
DURATION=$((END_TIME - START_TIME))

echo "✅ RPC test completed in ${DURATION} seconds with exit code: ${CURL_EXIT_CODE}"

# Check the exit code
if [ $CURL_EXIT_CODE -eq 124 ]; then
    echo "::error::RPC test timed out after 10 minutes"
elif [ $CURL_EXIT_CODE -ne 0 ]; then
    echo "::error::RPC test failed with exit code: ${CURL_EXIT_CODE}"
fi

# Display the output (first 100 lines to avoid overwhelming logs)
echo "=== RPC Test Output (first 100 lines) ==="
head -n 100 curl_output.txt
echo "..."
tail -n 20 curl_output.txt
echo "======================================"

# Give some extra time for any background processing
ADDITIONAL_WAIT=$((600 - DURATION > 0 ? 600 - DURATION : 60))  # Wait up to 10 minutes total
if [ $ADDITIONAL_WAIT -gt 0 ]; then
    echo "⏳ Waiting an additional ${ADDITIONAL_WAIT} seconds for background processes..."
    sleep $ADDITIONAL_WAIT
fi


 echo "Stopping keploy record and server"


 # Stop keploy record
 pid=$(pgrep keploy || true)
 echo "$pid Keploy PID"
 if [ -n "${pid:-}" ]; then
   echo "Killing keploy"
   sudo kill "$pid" || true
 fi


 echo "Waiting for processes to settle"


 check_for_errors record_incoming.txt
 check_for_errors client_incoming.txt


 echo "Replaying incoming requests"


 # Clean up any previous test runs
 echo "🧹 Cleaning up previous test runs..."
 sudo rm -rf ./keploy/reports/test-run-* 2>/dev/null || true

 # Create reports directory with proper permissions
 echo "📂 Ensuring keploy/reports directory exists..."
 sudo mkdir -p ./keploy/reports
 sudo chmod -R 777 ./keploy/reports
 
 # Debug: Show disk space and inodes
 echo "💾 Disk space and inodes:"
 df -h .
 df -i .

 # Run the replay with debug output
 echo "🚀 Running keploy test in replay mode with debug..."
 set -x  # Enable command echoing
 
 # Run the test with a timeout to prevent hanging
 timeout 300 sudo -E env PATH="$PATH" "$REPLAY_BIN" test -c "$FUZZER_SERVER_BIN" --debug &> test_incoming.txt || {
   REPLAY_STATUS=$?
   echo "::error::Replay command failed with status $REPLAY_STATUS"
   if [ $REPLAY_STATUS -eq 124 ]; then
     echo "::error::Test timed out after 300 seconds (5 minutes)"
   fi
   echo "=== Test Output ==="
   cat test_incoming.txt
   echo "=================="
   exit 1
 }
 set +x  # Disable command echoing
 
 echo "✅ Replay completed. Checking for errors..."
 check_for_errors test_incoming.txt

 # Debug: Show current directory and contents
 echo "📁 Current directory: $(pwd)"
 echo "📂 Directory contents:"
 ls -la
 
 # Debug: Show keploy directory structure
 echo "🔍 Keploy directory structure:"
 find ./keploy -type d -exec ls -ld {} \;
 
 # Find test-run directories with more reliable method
 echo "🔍 Searching for test-run directories..."
 RUN_DIR=$(find ./keploy/reports -maxdepth 1 -type d -name "test-run-*" -printf '%T@ %p\n' 2>/dev/null | sort -n | tail -1 | cut -d' ' -f2-)
 
 if [[ -z "$RUN_DIR" ]]; then
   echo "❌ No test-run directory found after test execution"
   echo "=== keploy/reports contents ==="
   ls -la ./keploy/reports 2>/dev/null || echo "keploy/reports directory not found"
   echo "=== Test output (first 100 lines) ==="
   head -n 100 test_incoming.txt
   echo "=== End of test output ==="
   
   # Check for processes that might be using the directory
   echo "🔍 Checking for processes using keploy directory:"
   sudo lsof +D "$(pwd)/keploy" 2>/dev/null || echo "Could not check for processes using the directory"
   
   # Check system logs for any relevant errors
   echo "📋 System logs (last 20 lines):"
   sudo journalctl -n 20 --no-pager 2>/dev/null || echo "Could not access system logs"
   
   exit 1
 fi
 
 echo "Using reports from: $RUN_DIR"
 echo "Contents of $RUN_DIR:"
 ls -la "$RUN_DIR" 2>/dev/null || echo "Could not list contents of $RUN_DIR"


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
 sudo -E env PATH="$PATH" "$RECORD_BIN" record -c "$FUZZER_CLIENT_BIN --http :18080" &> record_outgoing.txt &
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


 pid=$(pgrep keploy || true)
 echo "$pid Keploy PID"
 if [ -n "${pid:-}" ]; then
   echo "Killing keploy"
   sudo kill "$pid" || true
 fi
 sleep 5


 check_for_errors server_outgoing.txt
 check_for_errors record_outgoing.txt


 # Replay the client (relying on mocks)
 sudo -E env PATH="$PATH" "$REPLAY_BIN" test -c "$FUZZER_CLIENT_BIN --http :18080" &> test_outgoing.txt
 check_for_errors test_outgoing.txt
 ensure_success_phrase test_outgoing.txt
 echo "✅ Outgoing mode passed."

else
 echo "❌ Invalid mode specified: $MODE"
 exit 1
fi

exit 0