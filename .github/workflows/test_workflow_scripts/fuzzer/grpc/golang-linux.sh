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

# --- ✨ NEW: Robust function to wait for a port to be open ---
wait_for_port() {
  local port=$1
  local service_name=$2
  local timeout=30
  echo "Waiting for $service_name on port $port..."
  while ! nc -z localhost "$port" > /dev/null 2>&1 && [ $timeout -gt 0 ]; do
    sleep 1
    timeout=$((timeout - 1))
  done

  if [ $timeout -eq 0 ]; then
    echo "::error::$service_name failed to start on port $port in time."
    exit 1
  fi
  echo "$service_name is ready."
}


check_for_errors() {
  local logfile=$1
  if [ -f "$logfile" ]; then
    if grep -q "ERROR" "$logfile"; then
      if grep -Eq 'failed to read symbols, skipping coverage calculation|no symbol section' "$logfile"; then
        echo "Ignoring benign coverage-symbol error in $logfile"
      else
        echo "Error found in $logfile"
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

stop_keploy_gracefully() {
  echo "Stopping keploy gracefully..."
  pid=$(pgrep keploy || true)
  if [ -z "${pid:-}" ]; then
    echo "Keploy process not found."
    return
  fi

  echo "${pid} is the Keploy PID"
  sudo kill -INT "$pid" || true
  
  timeout=30
  while [ $timeout -gt 0 ] && ps -p "$pid" > /dev/null; do
    sleep 1
    timeout=$((timeout - 1))
  done

  if ps -p "$pid" > /dev/null; then
    echo "Keploy did not shut down gracefully after 30s, forcing kill."
    sudo kill -KILL "$pid" || true
  else
    echo "Keploy shut down gracefully."
  fi
}


if [ "$MODE" = "incoming" ]; then
  echo "🧪 Testing with incoming requests"

  # Start server with keploy in record mode
  sudo -E env PATH="$PATH" "$RECORD_BIN" record -c "$FUZZER_SERVER_BIN" &> record_incoming.txt &
  # --- ✨ CHANGED: Use wait_for_port instead of sleep ---
  wait_for_port 50051 "Fuzzer Server"

  # Start client HTTP driver
  "$FUZZER_CLIENT_BIN" --http :18080 &> client_incoming.txt &
  # --- ✨ CHANGED: Use wait_for_port instead of sleep ---
  wait_for_port 18080 "Fuzzer Client"

  # Kick off 1000 unary RPC fuzz calls
  echo "Starting fuzzing run..."
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

  echo "Fuzzing complete. Giving Keploy time to process recordings..."
  sleep 30

  stop_keploy_gracefully
  echo "Waiting for processes to settle"
  check_for_errors record_incoming.txt
  check_for_errors client_incoming.txt
  echo "Replaying incoming requests"

  # Replay
  sudo -E env PATH="$PATH" "$REPLAY_BIN" test -c "$FUZZER_SERVER_BIN" &> test_incoming.txt
  echo "checking for errors"
  check_for_errors test_incoming.txt

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
  # --- ✨ CHANGED: Use wait_for_port instead of sleep ---
  wait_for_port 50051 "Fuzzer Server"

  # Record the client (it makes outgoing RPCs)
  sudo -E env PATH="$PATH" "$RECORD_BIN" record -c "$FUZZER_CLIENT_BIN --http :18080" &> record_outgoing.txt &
  # --- ✨ CHANGED: Use wait_for_port instead of sleep ---
  wait_for_port 18080 "Fuzzer Client"

  echo "Starting fuzzing run..."
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

  echo "Fuzzing complete. Giving Keploy time to process recordings..."
  sleep 30

  stop_keploy_gracefully
  sleep 5
  check_for_errors server_outgoing.txt
  check_for_errors record_outgoing.txt

  # Replay the client (relying on mocks)
  echo "Replaying outgoing requests..."
  sudo -E env PATH="$PATH" "$REPLAY_BIN" test -c "$FUZZER_CLIENT_BIN --http :18080" &> test_outgoing.txt
  check_for_errors test_outgoing.txt
  ensure_success_phrase test_outgoing.txt
  echo "✅ Outgoing mode passed."

else
  echo "❌ Invalid mode specified: $MODE"
  exit 1
fi

exit 0