#!/bin/bash
# gin-mongo: a reply MongoDB sends after the app closed its connection is not
# recorded as a mock the app read.
#
# When the app stops, its MongoDB driver closes its connections. Its streaming
# monitor keeps an awaitable hello waiting on mongod for up to maxAwaitTimeMS
# (the heartbeat frequency); while it waits, mongod answers the client's FIN by
# interrupting it: ok: 0, code 279, ClientDisconnect. keploy forwards that
# reply to the client, which has closed its socket and resets the connection:
# the app never reads it. Recorded, it was a config mock, and replay served it
# to the app's monitor hello; the driver cleared its pool and failed whatever
# app call was in flight (an app's start-up ping did, and keploy test exited 1
# after a green record).
#
# The stock gin-mongo lane stops recording after the monitor's first hello was
# answered, so it passes either way. Here the sample's URI sets
# heartbeatFrequencyMS=60000: the monitor's first awaitable hello waits 60 s
# from the app's start, and a recording stopped as soon as keploy has the test
# cases stops inside that wait on any runner. The lane fails:
#   - when a recorded mock holds a ClientDisconnect reply;
#   - when a recorded mock holds an awaitable hello (maxAwaitTimeMS): the stop
#     came after the monitor's first awaitable hello was answered, and the
#     reply mongod interrupted was a later one, streamed, which the Mongo
#     recorder never keeps, so the run checks nothing;
#   - unless keploy's relay says it kept from the parser the bytes mongod sent
#     the app after its close ("did not take what the destination sent it
#     after", --debug, on a connection to 27017);
#   - when keploy record exits non-zero, logs an ERROR or a data race;
#   - unless keploy test then passes the three test cases (a sanity check of
#     the recording; a replay of main's recording passes too).
source "$(dirname "${BASH_SOURCE[0]}")/../../go-retry.sh"
source "$(dirname "${BASH_SOURCE[0]}")/../../wait-recording.sh"
source "${GITHUB_WORKSPACE:-${PWD%/samples-*}}/.github/workflows/test_workflow_scripts/docker-build-retry.sh"
source "${GITHUB_WORKSPACE:-${PWD%/samples-*}}/.github/workflows/test_workflow_scripts/mongo-ci.sh"

# The recording's tee (rec_pid) and its keploy (rec_keploy), once started. A
# lane that fails while keploy records stops it (bounded) on its way out, so
# no keploy, agent or app outlives the step.
rec_pid="" rec_keploy="" rec_fifo=""
on_exit() {
    if [ -n "$rec_keploy" ] && kill -0 "$rec_keploy" 2>/dev/null; then
        echo "stopping the recording on the way out"
        kill -INT "$rec_keploy" 2>/dev/null || true
        wait_recording_end "$rec_pid" record.txt 120 || true
    fi
    # A tee left waiting on the FIFO: the keploy job could not open it, so no
    # writer ever came (a keploy that cannot start opens it before its exec
    # fails, and tee then ends on its own).
    if [ -n "$rec_pid" ] && kill -0 "$rec_pid" 2>/dev/null; then
        kill "$rec_pid" 2>/dev/null || true
    fi
    [ -n "$rec_fifo" ] && rm -f "$rec_fifo"
    docker rm -f mongoDb >/dev/null 2>&1 || true
}
trap on_exit EXIT
if ! start_mongo; then
    docker ps -a || true
    exit 1
fi

# The monitor's awaitable hello waits for the heartbeat frequency: 60 s.
uri='?retryWrites=true&w=majority"'
if ! grep -qF "$uri" handler.go; then
    echo "::error::gin-mongo's handler.go no longer builds its URI as '...$uri': set heartbeatFrequencyMS=60000 where it does now"
    exit 1
fi
sed -i 's|?retryWrites=true&w=majority"|?retryWrites=true\&w=majority\&heartbeatFrequencyMS=60000"|' handler.go
grep -n "heartbeatFrequencyMS=60000" handler.go || exit 1

rm -rf keploy/ keploy.yml
cat > keploy.yml <<'KEPLOY_CFG'
test:
    globalNoise:
        global: {"body": {"ts":[]}}
KEPLOY_CFG
go_retry build -o ginApp || exit 1

# An ERROR keploy logs, or a data race: the lane fails on either.
record_bad() {
    awk '(/ERROR/ && !/failed to read from connection.*use of closed network connection/) || /WARNING: DATA RACE/ { print; found = 1 } END { exit !found }' "$1"
}
app_listening() { (exec 3<>/dev/tcp/127.0.0.1/8080) 2>/dev/null; }
three_captured() { log_captured record.txt post-url:2 get-verify-email:1; }

# keploy record and the tee that copies its output to the step's log are both
# this shell's own background jobs, joined by a FIFO (as go-retry.sh joins go
# to its tee): wait gives keploy's own exit status, and tee ends once keploy,
# its agent and the app have all exited (wait_recording_end). A pipeline
# (keploy | tee &) would hide keploy's status, and a process substitution
# (> >(tee)) has no PID of its own to wait on.
rec_fifo=$(mktemp -u "${RUNNER_TEMP:-/tmp}/keploy-record.XXXXXX")
mkfifo "$rec_fifo" || { echo "::error::cannot create the recording's FIFO"; exit 1; }
tee record.txt < "$rec_fifo" &
rec_pid=$!
"$RECORD_BIN" record -c "./ginApp" --debug > "$rec_fifo" 2>&1 &
rec_keploy=$!
# The app listens once keploy has started it (a port that opens: a request
# here would be a test case of its own).
await_ready "gin-mongo under keploy record" "$rec_pid" record.txt record_bad app_listening || exit 1
curl --fail --show-error --max-time 30 --request POST --url http://localhost:8080/url \
    --header 'content-type: application/json' --data '{"url": "https://facebook.com"}' || exit 1
curl --fail --show-error --max-time 30 --request POST --url http://localhost:8080/url \
    --header 'content-type: application/json' --data '{"url": "https://google.com"}' || exit 1
curl --fail --show-error --max-time 30 --request GET \
    --url 'http://localhost:8080/verify-email?email=test@gmail.com' --header 'Accept: application/json' || exit 1
# Stopped as soon as keploy has the three test cases: inside the monitor's
# first awaitable wait.
await_ready "the three test cases" "$rec_pid" record.txt record_bad three_captured || exit 1
echo "stopping keploy record (pid $rec_keploy)"
kill -INT "$rec_keploy" 2>/dev/null || true
wait_recording_end "$rec_pid" record.txt || exit 1
wait "$rec_keploy" 2>/dev/null
record_status=$?
rec_keploy=""
if [ "$record_status" -ne 0 ]; then
    echo "::error::keploy record exited ${record_status}"
    exit 1
fi
echo "keploy record exited ${record_status}"

if found="$(record_bad record.txt)"; then
    echo "::error::keploy record logged an ERROR or a data race"
    printf '%s\n' "$found"
    exit 1
fi
if ! grep -qs '^kind: Mongo$' keploy/test-set-0/mocks.yaml; then
    echo "::error::no Mongo mock was recorded: this build has no Mongo parser (keploy's private parsers), so the lane checks nothing"
    exit 1
fi
if grep -n -B25 "ClientDisconnect" keploy/test-set-0/mocks.yaml; then
    echo "::error::a mock holds the reply mongod sent after the app closed its connection (ClientDisconnect): the app never read it"
    exit 1
fi
if grep -n "maxAwaitTimeMS" keploy/test-set-0/mocks.yaml; then
    echo "::error::a mock holds the monitor's awaitable hello: the recording stopped after its first wait was answered, so the reply mongod interrupted was a streamed one the recorder never keeps, and this run checks nothing"
    exit 1
fi
sed 's/\x1b\[[0-9;]*m//g' record.txt | grep -E "did not take what the destination sent it after" | grep -E ':27017"' | cut -c1-400 > not_taken.txt
if [ ! -s not_taken.txt ]; then
    echo "::error::keploy's relay logged no bytes the app did not take after closing its MongoDB connection: the reply mongod sent after the app's close reached the parser, or never came, and this run checks nothing"
    sed 's/\x1b\[[0-9;]*m//g' record.txt | grep -E "half-closed|stopping Keploy|did not take" | cut -c1-300 | head -20
    exit 1
fi
echo "keploy kept from the parser what the app did not take after closing:"
cat not_taken.txt

"$REPLAY_BIN" test -c "./ginApp" --delay 10 2>&1 | tee test_logs.txt
replay_status=${PIPESTATUS[0]}
if [ "$replay_status" -ne 0 ]; then
    echo "::error::keploy test exited ${replay_status}"
    exit "$replay_status"
fi
if grep "WARNING: DATA RACE" test_logs.txt; then
    echo "::error::a data race in keploy test"
    exit 1
fi
report=./keploy/reports/test-run-0/test-set-0-report.yaml
status=$(grep -m1 '^status:' "$report" 2>/dev/null | awk '{print $2}')
total=$(grep -m1 '^total:' "$report" 2>/dev/null | awk '{print $2}')
echo "$(basename "$report"): ${status:-none}, ${total:-0} tests"
if [ "$status" != "PASSED" ] || [ "${total:-0}" -ne 3 ]; then
    echo "::error::keploy test did not pass the three recorded test cases"
    exit 1
fi
echo "✅ no reply sent after the app's close was recorded, and the recording replays"
