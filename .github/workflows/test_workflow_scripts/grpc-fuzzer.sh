#!/bin/bash

MODE=$1 # 'incoming' or 'outgoing'

source ./.github/workflows/test_workflow_scripts/test-iid.sh

# Generate keploy config and add duration_ms to noise to avoid timing mismatches
if [ -f "./keploy.yml" ]; then
  rm ./keploy.yml
fi
sudo -E env PATH="$PATH" $RECORD_BIN config --generate
config_file="./keploy.yml"
# Set global noise body.duration_ms if config was generated with empty global
sed -i 's/global: {}/global: {"body": {"duration_ms":[]}}/' "$config_file"

SUCCESS_PHRASE="all 1000 unary RPCs validated successfully"

check_for_errors() {
  local logfile=$1
  if [ -f "$logfile" ]; then
    if grep -q "ERROR" "$logfile"; then
        echo "Error found in $logfile"
        cat "$logfile"
        exit 1
    fi
    if grep -q "WARNING: DATA RACE" "$logfile"; then
        echo "Race condition detected in $logfile"
        cat "$logfile"
        exit 1
    fi
  fi
}

ensure_success_phrase() {
  # Ensure the success phrase appears in at least one of the provided files
  for f in "$@"; do
    if [ -f "$f" ] && grep -qiF "$SUCCESS_PHRASE" "$f"; then
      echo "Validation success phrase found in $f"
      return 0
    fi
  done
  echo "‼️ Did not find success phrase: '$SUCCESS_PHRASE' in logs: $*"
  # Dump the logs for visibility
  for f in "$@"; do
    [ -f "$f" ] && { echo "--- $f ---"; tail -n +1 "$f"; echo "--------------"; }
  done
  exit 1
}

if [ "$MODE" == "incoming" ]; then
    echo "🧪 Testing with incoming requests"

    # Start server in record mode
    sudo -E env PATH="$PATH" $RECORD_BIN record -c "$GRPC_SERVER_BIN" &> record_incoming.txt &
    sleep 10

    # Start client
    $GRPC_CLIENT_BIN --http :18080 &> client_incoming.txt &
    sleep 10

    # Make curl request
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

    # Stop keploy record
    pid=$(pgrep keploy)
    echo "$pid Keploy PID"
    echo "Killing keploy"
    sudo kill $pid

    check_for_errors record_incoming.txt
    check_for_errors client_incoming.txt

    sudo -E env PATH="$PATH" $REPLAY_BIN test -c "$GRPC_SERVER_BIN" &> test_incoming.txt

    check_for_errors test_incoming.txt
    ensure_success_phrase test_incoming.txt
    echo "✅ Incoming mode passed."

elif [ "$MODE" == "outgoing" ]; then
    echo "🧪 Testing with outgoing requests"

    # Start server
    $GRPC_SERVER_BIN &> server_outgoing.txt &
    sleep 5

    # Start client in record mode
    sudo -E env PATH="$PATH" $RECORD_BIN record -c "$GRPC_CLIENT_BIN --http :18080" &> record_outgoing.txt &
    sleep 10

    # Make curl request
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

    # Stop keploy record
    pid=$(pgrep keploy)
    echo "$pid Keploy PID"
    echo "Killing keploy"
    sudo kill $pid
    sleep 5

    check_for_errors server_outgoing.txt
    check_for_errors record_outgoing.txt

    sudo -E env PATH="$PATH" $REPLAY_BIN test -c "$GRPC_CLIENT_BIN --http :18080" &> test_outgoing.txt

    check_for_errors test_outgoing.txt
    ensure_success_phrase test_outgoing.txt
    echo "✅ Outgoing mode passed."
else
    echo "❌ Invalid mode specified: $MODE"
    exit 1
fi

exit 0
