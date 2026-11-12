#!/bin/bash
source "$(dirname "${BASH_SOURCE[0]}")/../../go-retry.sh"
source "$(dirname "${BASH_SOURCE[0]}")/../../wait-recording.sh"

# This script tests the go-grpc sample application in two modes:
# 'incoming': Tests the gRPC server by recording its incoming gRPC calls.
# 'outgoing': Tests the gRPC client by recording its outgoing gRPC calls.
#
# Expects:
#   MODE                -> 'incoming' or 'outgoing'   (argv[1])
#   RECORD_BIN          -> path to keploy record binary (env)
#   REPLAY_BIN          -> path to keploy test binary   (env)

set -Eeuo pipefail

MODE=${1:-incoming}

echo "root ALL=(ALL:ALL) ALL" | sudo tee -a /etc/sudoers

# --- Sanity Checks ---
[ -x "${RECORD_BIN:-}" ] || { echo "RECORD_BIN not set or not executable"; exit 1; }
[ -x "${REPLAY_BIN:-}" ] || { echo "REPLAY_BIN not set or not executable"; exit 1; }
command -v go >/dev/null 2>&1 || { echo "go not found"; exit 1; }
command -v curl >/dev/null 2>&1 || { echo "curl not found"; exit 1; }

echo "root ALL=(ALL:ALL) ALL" | sudo tee -a /etc/sudoers

# --- Build Application ---
echo "Building gRPC server and client binaries..."
# Keploy's incoming gRPC proxy binds the original app port on IPv4 in this CI
# path. GitHub runners can resolve localhost to ::1 first, so keep the sample
# client on the same loopback family as the readiness check and proxy listener.
if grep -q '"localhost:50051"' client/client.go; then
    sed -i 's/"localhost:50051"/"127.0.0.1:50051"/' client/client.go
fi
go_retry build -o grpc-server .
go_retry build -o grpc-client ./client
chmod +x ./grpc-server ./grpc-client

# --- Helper Functions ---

# Kills all running application and keploy processes
cleanup() {
    echo "Cleaning up running processes..."
    pkill -f keploy || true
    pkill -f grpc-server || true
    pkill -f grpc-client || true
    sleep 2
    pkill -9 -f keploy || true
    pkill -9 -f grpc-server || true
    pkill -9 -f grpc-client || true
    rm -rf "${HEALTH:-}"
    echo "Cleanup complete."
}
trap cleanup EXIT

# log_errors LOG: prints the lines of LOG this lane fails on, an ERROR (but
# the non-critical "Unsupported DNS query type") or a data race, and succeeds
# if there are any. check_for_errors fails a finished log on them, and the
# waits below (await_ready) a log still being written.
log_errors() {
  [ -f "$1" ] || return 1
  awk '(/ERROR/ && !/Unsupported DNS query type/) || /WARNING: DATA RACE/ { print; found = 1 } END { exit !found }' "$1"
}

# Checks a log file for critical errors or data races
check_for_errors() {
  local logfile=$1
  echo "Checking for errors in $logfile..."
  if log_errors "$logfile" >/dev/null; then
    echo "Error or race condition found in $logfile"
    cat "$logfile"
    exit 1
  fi
  echo "No critical errors found in $logfile."
}

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
        test_status=$(grep 'status:' "$report_file" | sed -n 1p | awk '{print $2}')
        
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

port_open() { (echo >"/dev/tcp/127.0.0.1/$1") >/dev/null 2>&1; }
HEALTH=$(mktemp -d)
# health_answered: /health answered, or failed other than by a refused
# connection (curl exit 7). Only a refusal means the app is not up yet; what
# it says once it is up is final.
health_answered() {
    health_rc=0
    health_code=$(curl --silent --show-error --max-time 5 -o "$HEALTH/body" -w '%{http_code}' \
        http://127.0.0.1:8080/health 2>"$HEALTH/err") || health_rc=$?
    [ "$health_rc" != 7 ]
}

# send_requests PIDS LOG: waits for the HTTP driver to answer /health (it
# calls the gRPC server, so it answers once the whole chain does), then sends
# the four requests. The wait lasts as long as PIDS live and LOG shows
# progress and no error (await_ready): in record mode the app starts only
# once keploy's agent is ready, which on a loaded runner takes as long as
# loading its eBPF programs does. Ten tries three seconds apart gave up while
# it still was (pipeline 11579: "Application failed to start on port 8080").
# Its first answer decides: retrying an unhealthy one fed the wait its own
# progress (each /health the server sees is a line in its log).
send_requests() {
    local pids=$1 log=$2
    echo "Waiting for application's HTTP server to start..."
    await_ready "the app's :8080" "$pids" "$log" log_errors health_answered || exit 1
    if [ "$health_rc" != 0 ] || [ "${health_code:0:1}" != 2 ]; then
        echo "::error::the app's /health answered HTTP ${health_code:-none} (curl exit $health_rc): $(cat "$HEALTH/body" "$HEALTH/err" 2>/dev/null)"
        exit 1
    fi
    echo "Application is ready. Sending requests..."
    # 1. POST request
    curl --fail --silent --show-error --max-time 5 -X POST http://127.0.0.1:8080/users -H "Content-Type: application/json" -d '{"name": "test-user", "email": "test@gmail.com", "age": 20}'
    # 2. GET request
    curl --fail --silent --show-error --max-time 5 -X GET http://127.0.0.1:8080/users
    # 3. PUT request
    curl --fail --silent --show-error --max-time 5 -X PUT http://127.0.0.1:8080/users -H "Content-Type: application/json" -d '{"id": 1, "name": "test-user-updated", "email": "test@gmail.com", "age": 20}'
    # 4. DELETE request
    curl --fail --silent --show-error --max-time 5 -X DELETE http://127.0.0.1:8080/users -H "Content-Type: application/json" -d '{"id": 1}'
    echo "Requests sent."
}

# wait_for_port PORT PIDS LOG: until PORT accepts a connection, for as long as
# PIDS (what opens it) live and LOG shows progress and no error.
wait_for_port() {
    local port=$1 pids=$2 log=$3
    echo "Waiting for port $port to be open..."
    if ! await_ready "port $port" "$pids" "$log" log_errors port_open "$port"; then
        ss -ltnp || true
        exit 1
    fi
}

# wait_captured LOG PIDS SLUG[:N]...: until keploy's recording (LOG) says it
# captured each test case the requests made, by name (log_captured), rather
# than a fixed sleep before it is stopped.
wait_captured() {
    local log=$1 pids=$2
    shift 2
    if ! await_ready "keploy's capture of $*" "$pids" "$log" log_errors log_captured "$log" "$@"; then
        echo "::error::$log names no capture of: $(log_missing "$log" "$@")"
        exit 1
    fi
}
# What send_requests makes keploy capture: in incoming mode, the gRPC calls
# the driver makes (its /health calls GetUsers too); in outgoing mode, the
# HTTP requests themselves.
INCOMING_CAPTURES="grpc-userservice-getusers:2 grpc-userservice-createuser grpc-userservice-updateuser grpc-userservice-deleteuser"
OUTGOING_CAPTURES="get-health post-users get-users put-users delete-users"

# Kills the keploy process and waits for it to terminate
kill_keploy_process() {
    # The recording this lane started (its pipeline's keploy, rec_keploy), not
    # whichever process named keploy has the lowest PID on the host.
    REC_PID=${rec_keploy:-$(pgrep keploy | sort -n | head -1)}
    echo "$REC_PID Keploy PID"
    echo "Killing keploy"
    sudo kill -INT "$REC_PID" 2>/dev/null || true
}

# --- Main Logic ---

# Reset state before each run
rm -rf ./keploy*
# Only when the sample does not ship one. `config --generate` used to do
# nothing at all over an existing keploy.yml -- it asked, stdin answered EOF,
# and it skipped and exited 0 -- so the shipped config is what these runs have
# always used, noise rules and all. It refuses out loud now rather than
# pretending, which is right, and this says what the run actually wants.
[ -f keploy.yml ] || "$RECORD_BIN" config --generate

# shellcheck disable=SC1091
source "${GITHUB_WORKSPACE:-${PWD%/samples-*}}/.github/workflows/test_workflow_scripts/json-pass-helpers.sh"

if [ "$MODE" = "incoming" ]; then
    echo "🧪 Testing incoming gRPC requests (testing grpc-server)"
    # Record: Keploy wraps the server to capture incoming gRPC calls. The client is just a driver.
    "$RECORD_BIN" record -c "./grpc-server" --generateGithubActions=false 2>&1 | tee record_incoming.log &
    # tee ends once keploy and everything it started (the agent, the app)
    # have exited: they all write to it.
    rec_pid=$!
    rec_keploy=$(jobs -p %%) # the pipeline's keploy
    wait_for_port 50051 "$rec_pid" record_incoming.log
    # The driver once the server is up: started first, its gRPC channel was
    # in reconnect backoff when the server came, and its first calls failed.
    ./grpc-client &> client_incoming.log &
    client_pid=$!

    send_requests "$client_pid $rec_pid" record_incoming.log
    # shellcheck disable=SC2086 # a list of names
    wait_captured record_incoming.log "$rec_pid" $INCOMING_CAPTURES

    kill_keploy_process
    # Wait for the recording to end, not for a fixed time: keploy drains
    # what it captured before it exits, and that takes as long as the
    # runner's load makes it. The next step reuses the ports and the agent.
    wait_recording_end "$rec_pid" record_incoming.log || exit 1

    check_for_errors record_incoming.log

    if json_pass_supported; then
        # The yaml pass left grpc-client (the HTTP→gRPC driver on :8080) alive
        # because kill_keploy_process only stops keploy and its -c child (the
        # server). Without this teardown, the new ./grpc-client below fails to
        # bind :8080 and send_requests races against the stale client.
        # Wait for it to exit, not for a fixed time: the next one binds :8080.
        kill "$client_pid" 2>/dev/null || true
        wait "$client_pid" 2>/dev/null || true
        "$RECORD_BIN" record --storage-format json -c "./grpc-server" --generateGithubActions=false 2>&1 | tee record_incoming_json.log &
        # tee ends once keploy and everything it started (the agent, the app)
        # have exited: they all write to it.
        rec_pid=$!
        rec_keploy=$(jobs -p %%) # the pipeline's keploy
        wait_for_port 50051 "$rec_pid" record_incoming_json.log
        ./grpc-client &> client_incoming_json.log &
        client_pid=$!
        send_requests "$client_pid $rec_pid" record_incoming_json.log
        # shellcheck disable=SC2086
        wait_captured record_incoming_json.log "$rec_pid" $INCOMING_CAPTURES
        kill_keploy_process
        # Wait for the recording to end, not for a fixed time: keploy drains
        # what it captured before it exits, and that takes as long as the
        # runner's load makes it. The next step reuses the ports and the agent.
        wait_recording_end "$rec_pid" record_incoming_json.log || exit 1
        check_for_errors record_incoming_json.log
    fi

    # Test: Keploy replays the captured gRPC calls against the server.
    "$REPLAY_BIN" test -c "./grpc-server" --generateGithubActions=false 2>&1 | tee test_incoming.log || true

    check_for_errors test_incoming.log
    if ! check_test_report; then
        echo "Test report check failed for incoming mode."
        cat test_incoming.log
        exit 1
    fi

    if json_pass_supported; then
        "$REPLAY_BIN" test --storage-format json -c "./grpc-server" --generateGithubActions=false 2>&1 | tee test_incoming_json.log || true
        check_for_errors test_incoming_json.log
        if ! json_scan_reports; then
            cat test_incoming_json.log
            exit 1
        fi
        echo "✅ Incoming mode passed (yaml + json)."
    else
        echo "✅ Incoming mode passed (yaml only)."
    fi

elif [ "$MODE" = "outgoing" ]; then
    echo "🧪 Testing outgoing gRPC requests (testing grpc-client)"
    # Record: Keploy wraps the client to capture its outgoing gRPC calls. The server is a dependency.
    ./grpc-server &> server_outgoing.log &
    server_pid=$!
    wait_for_port 50051 "$server_pid" server_outgoing.log
    "$RECORD_BIN" record -c "./grpc-client" --generateGithubActions=false 2>&1 | tee record_outgoing.log &
    # tee ends once keploy and everything it started (the agent, the app)
    # have exited: they all write to it.
    rec_pid=$!
    rec_keploy=$(jobs -p %%) # the pipeline's keploy

    send_requests "$rec_pid $server_pid" record_outgoing.log
    # shellcheck disable=SC2086
    wait_captured record_outgoing.log "$rec_pid" $OUTGOING_CAPTURES

    kill_keploy_process
    # Wait for the recording to end, not for a fixed time: keploy drains
    # what it captured before it exits, and that takes as long as the
    # runner's load makes it. The next step reuses the ports and the agent.
    wait_recording_end "$rec_pid" record_outgoing.log || exit 1
    
    check_for_errors record_outgoing.log

    if json_pass_supported; then
        # The yaml pass spawned grpc-server manually (not under keploy -c), so
        # kill_keploy_process did NOT take it down. Without this teardown the
        # new ./grpc-server below silently fails to bind :50051, wait_for_port
        # succeeds against the stale server, and the send_requests sequence
        # tries to UpdateUser id=1 — which was deleted at the end of the yaml
        # pass — producing curl exit 22 / HTTP 500 "user not found".
        # Wait for it to exit, not for a fixed time: the next one binds :50051.
        kill "$server_pid" 2>/dev/null || true
        wait "$server_pid" 2>/dev/null || true
        ./grpc-server &> server_outgoing_json.log &
        server_pid=$!
        wait_for_port 50051 "$server_pid" server_outgoing_json.log
        "$RECORD_BIN" record --storage-format json -c "./grpc-client" --generateGithubActions=false 2>&1 | tee record_outgoing_json.log &
        # tee ends once keploy and everything it started (the agent, the app)
        # have exited: they all write to it.
        rec_pid=$!
        rec_keploy=$(jobs -p %%) # the pipeline's keploy
        send_requests "$rec_pid $server_pid" record_outgoing_json.log
        # shellcheck disable=SC2086
        wait_captured record_outgoing_json.log "$rec_pid" $OUTGOING_CAPTURES
        kill_keploy_process
        # Wait for the recording to end, not for a fixed time: keploy drains
        # what it captured before it exits, and that takes as long as the
        # runner's load makes it. The next step reuses the ports and the agent.
        wait_recording_end "$rec_pid" record_outgoing_json.log || exit 1
        check_for_errors record_outgoing_json.log
    fi

    # Test: Keploy mocks the server's responses for the client. The real server is NOT run.
    "$REPLAY_BIN" test -c "./grpc-client" --generateGithubActions=false 2>&1 | tee test_outgoing.log || true

    check_for_errors test_outgoing.log
    if ! check_test_report; then
        echo "Test report check failed for outgoing mode."
        cat test_outgoing.log
        exit 1
    fi

    if json_pass_supported; then
        "$REPLAY_BIN" test --storage-format json -c "./grpc-client" --generateGithubActions=false 2>&1 | tee test_outgoing_json.log || true
        check_for_errors test_outgoing_json.log
        if ! json_scan_reports; then
            cat test_outgoing_json.log
            exit 1
        fi
        echo "✅ Outgoing mode passed (yaml + json)."
    else
        echo "✅ Outgoing mode passed (yaml only)."
    fi

else
    echo "❌ Invalid mode specified: '$MODE'. Use 'incoming' or 'outgoing'."
    exit 1
fi
