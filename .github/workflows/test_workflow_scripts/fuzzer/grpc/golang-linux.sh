#!/bin/bash

# Expects:
#   MODE                -> 'incoming' or 'outgoing'   (argv[1])
#   RECORD_BIN          -> path to keploy record binary (env)
#   REPLAY_BIN          -> path to keploy test binary   (env)
#   FUZZER_CLIENT_BIN   -> path to downloaded client bin (env)
#   FUZZER_SERVER_BIN   -> path to downloaded server bin (env)

# --- CHANGE 1: Added 'set -x' to print every command before it runs ---
set -Eeuox pipefail

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
 # The original script already prints the logs on failure, which is good.
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


 sleep 120


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


 # Replay
 sudo -E env PATH="$PATH" "$REPLAY_BIN" test -c "$FUZZER_SERVER_BIN" &> test_incoming.txt

 # --- CHANGE 2: Always display the replay log, even on success ---
 echo "--- Contents of replay log (test_incoming.txt) ---"
 cat test_incoming.txt
 echo "----------------------------------------------------"

 echo "checking for errors"
 check_for_errors test_incoming.txt


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


 echo "🎬 Starting replay of the client..."
 # Replay the client (relying on mocks)
 sudo -E env PATH="$PATH" "$REPLAY_BIN" test -c "$FUZZER_CLIENT_BIN --http :18080" &> test_outgoing.txt

 # --- CHANGE 2 (repeated for outgoing mode): Always display the replay log ---
 echo "--- Contents of replay log (test_outgoing.txt) ---"
 cat test_outgoing.txt
 echo "----------------------------------------------------"

 echo "🕵️‍♀️ Checking for errors and success phrase in the log..."
 check_for_errors test_outgoing.txt
 ensure_success_phrase test_outgoing.txt
 echo "✅ Outgoing mode passed."

else
 echo "❌ Invalid mode specified: $MODE"
 exit 1
fi

exit 0