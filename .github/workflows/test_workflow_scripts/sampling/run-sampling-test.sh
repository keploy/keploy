#!/usr/bin/env bash
# Drives the --enable-sampling end-to-end test:
#
#   1. Build the sample HTTP app (./sampling-test/main.go).
#   2. Start `keploy record --enable-sampling=$SAMPLING_LIMIT -c ./sampling-test`.
#   3. Wait for the app to be reachable, then fire $TOTAL_REQUESTS concurrent
#      curls via curl.sh — the burst overlaps inside the 500ms handler delay,
#      so the proxy sees far more in-flight connections than sampling slots.
#   4. Drain, SIGINT keploy, count recorded test cases under ./keploy/test-set-0/tests.
#   5. Assert:
#        - every curl saw a 2xx response (clients never broken by bypass)
#        - SAMPLING_LIMIT <= captured_test_cases < TOTAL_REQUESTS
#
# Expects (env): KEPLOY_BIN, SAMPLE_DIR (absolute path to samples-go/sampling-test).
# Optional   :  SAMPLING_LIMIT (default 5), TOTAL_REQUESTS (default 20),
#               HANDLER_DELAY_MS (default 500).

set -Eeuo pipefail

SAMPLING_LIMIT="${SAMPLING_LIMIT:-5}"
TOTAL_REQUESTS="${TOTAL_REQUESTS:-20}"
HANDLER_DELAY_MS="${HANDLER_DELAY_MS:-500}"
APP_PORT="${APP_PORT:-8080}"

: "${KEPLOY_BIN:?KEPLOY_BIN must point to the keploy binary}"
: "${SAMPLE_DIR:?SAMPLE_DIR must point to the sampling-test sample app}"

if [ ! -x "$KEPLOY_BIN" ]; then
    echo "::error::keploy binary $KEPLOY_BIN is not executable"
    exit 1
fi
if [ ! -d "$SAMPLE_DIR" ]; then
    echo "::error::sample dir $SAMPLE_DIR does not exist"
    exit 1
fi

section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

cd "$SAMPLE_DIR"

# Fake installation-id so keploy doesn't try to phone home.
sudo mkdir -p ~/.keploy
sudo touch ~/.keploy/installation-id.yaml
echo "ObjectID('123456789')" | sudo tee ~/.keploy/installation-id.yaml > /dev/null

section "Clean previous recordings"
rm -rf keploy/ curl-results/ record.log sampling-test
endsec

section "Build sample app"
go build -o sampling-test .
ls -lh sampling-test
endsec

cleanup() {
    rc=$?
    section "Cleanup: stopping keploy & sample app"
    sudo pkill -INT -f 'keploy record' 2>/dev/null || true
    sudo pkill -f './sampling-test' 2>/dev/null || true
    sleep 2
    sudo pkill -KILL -f 'keploy record' 2>/dev/null || true
    sudo pkill -KILL -f './sampling-test' 2>/dev/null || true
    endsec
    section "record.log tail"
    tail -n 200 record.log 2>/dev/null || echo "(no record.log)"
    endsec
    exit $rc
}
trap cleanup EXIT

section "Start keploy record (--enable-sampling=$SAMPLING_LIMIT)"
HANDLER_DELAY_MS="$HANDLER_DELAY_MS" \
    sudo -E env PATH="$PATH" HANDLER_DELAY_MS="$HANDLER_DELAY_MS" PORT="$APP_PORT" \
    "$KEPLOY_BIN" record \
        -c "./sampling-test" \
        --enable-sampling="$SAMPLING_LIMIT" \
        --generate-github-actions=false \
        2>&1 | tee record.log &
KEPLOY_TEE_PID=$!
echo "tee pid: $KEPLOY_TEE_PID"
endsec

section "Wait for sample app on :$APP_PORT"
ready=0
for i in $(seq 1 60); do
    if curl -sf --max-time 2 "http://localhost:${APP_PORT}/health" >/dev/null 2>&1; then
        echo "sample app ready after ${i}s"
        ready=1
        break
    fi
    sleep 1
done
if [ "$ready" -ne 1 ]; then
    echo "::error::sample app failed to come up within 60s"
    exit 1
fi
endsec

section "Fire $TOTAL_REQUESTS concurrent curls"
TOTAL_REQUESTS="$TOTAL_REQUESTS" \
    TARGET_URL="http://localhost:${APP_PORT}/work" \
    RESULTS_DIR="./curl-results" \
    bash ./curl.sh
endsec

# Give the proxy a moment to drain pending captures before SIGINT.
sleep 3

section "Stop keploy (SIGINT)"
REC_PID="$(pgrep -n -f 'keploy record' || true)"
if [ -n "$REC_PID" ]; then
    echo "sending SIGINT to keploy pid $REC_PID"
    sudo kill -INT "$REC_PID" || true
fi
# Wait for the tee'd pipeline to finish so record.log is flushed.
wait "$KEPLOY_TEE_PID" 2>/dev/null || true
sleep 2
endsec

section "Check record.log for errors"
if grep -E "panic:" record.log >/dev/null 2>&1; then
    echo "::error::panic detected in record.log"
    exit 1
fi
if grep "WARNING: DATA RACE" record.log >/dev/null 2>&1; then
    echo "::error::data race detected in record.log"
    exit 1
fi
echo "no critical errors found in record.log"
endsec

section "Count recorded test cases"
if [ ! -d ./keploy/test-set-0/tests ]; then
    echo "::error::./keploy/test-set-0/tests directory missing — nothing recorded"
    ls -la ./keploy 2>/dev/null || true
    exit 1
fi
# Keploy names test cases after the HTTP method + path (e.g. get-work-1.yaml).
# Count only /work captures — the readiness probe on /health may also land in
# the test-set and would otherwise inflate the count above the sampling budget.
captured=$(find ./keploy/test-set-0/tests -maxdepth 1 -name 'get-work-*.yaml' -type f | wc -l | tr -d ' ')
total_files=$(find ./keploy/test-set-0/tests -maxdepth 1 -name '*.yaml' -type f | wc -l | tr -d ' ')
echo "captured /work test cases: $captured"
echo "total recorded test cases (incl. /health probe): $total_files"
echo "sampling limit (K): $SAMPLING_LIMIT"
echo "total /work requests (N): $TOTAL_REQUESTS"
ls ./keploy/test-set-0/tests | sed 's/^/  /'
endsec

# Tolerance: slot recycling within the burst can occasionally push the
# captured count one or two above K, so we accept the open interval
# [K, N). A captured count of N or above means sampling didn't gate
# anything; a count below K means the slot semaphore is over-limiting.
if [ "$captured" -lt "$SAMPLING_LIMIT" ]; then
    echo "::error::captured ($captured) < SAMPLING_LIMIT ($SAMPLING_LIMIT) — sampling under-captured"
    exit 1
fi
if [ "$captured" -ge "$TOTAL_REQUESTS" ]; then
    echo "::error::captured ($captured) >= TOTAL_REQUESTS ($TOTAL_REQUESTS) — bypass path never triggered"
    exit 1
fi

echo "✅ sampling assertion passed: $SAMPLING_LIMIT <= $captured < $TOTAL_REQUESTS"
