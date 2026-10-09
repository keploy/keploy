#!/bin/bash
# Guard: a test recorded on an app port the host cannot reach.
#
# In Docker mode keploy records the ingress of every port the app listens on,
# including a port the app only calls itself on inside the container. Replay
# sends each test from the host to the port it was recorded on, so tests on a
# port the docker command does not publish are refused every time, and so are
# tests on a published port where the app listens only on 127.0.0.1 (docker
# forwards a published port to the container's own address, then drops the
# connection). keploy used to say nothing while recording, re-send each of
# them as an app "not yet accepting connections" or a transient reset, and,
# when such a test came first in the set, spend the readiness gate's whole
# ceiling (3 minutes by default) probing it. Its advice for an unpublished
# port was to publish it, which does nothing for a loopback-only server.
#
# The fixture (app.py) publishes 8095 and 8097, and calls its own 8096
# (0.0.0.0, unpublished) and 8097 (127.0.0.1 only). This asserts:
#   record: one warning for 8096 whose fix is to publish it, one for 8097
#           that says it listens only on 127.0.0.1 and does not advise
#           publishing, none for 8095;
#   replay: the 8096 and 8097 tests fail at once, each saying why its port
#           cannot be reached from the host; none is re-sent; the readiness
#           gate probes a reachable test instead of waiting out its ceiling;
#           the 8095 tests pass.
#
# Then the app's port published on another host port (-p 18095:8095, as a
# compose file's "18080:8080" or a CI job's ephemeral port is), recorded there
# and replayed with --port 18095 while the app takes 25s to start serving.
# Host port 18095 leads to the app's port 8095, where those tests were
# recorded, so it reaches the app: keploy used to compare 18095 with itself,
# call the publish to 8095 a publish elsewhere, skip the readiness gate's HTTP
# probe for it (passing on docker's own listener accepting instead) and fail
# the first tests at once as a port the host can never reach. This asserts:
#   record: the warning for 8095 names the host port it is published on and
#           says a port map in keploy.yml can send those tests there, not
#           --port, which sends every test of every set to one port;
#   replay: no 8095 test is said to be unreachable; the first request the app
#           serves on 8095 is the readiness gate's HTTP probe, so the gate
#           waited for the app's answer before any test went; the probe did
#           not time out; every 8095 test passes.

set -uo pipefail
# Explicit `set +e`: exit codes are captured explicitly and cleanup always runs.
set +e

source "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/test-iid.sh"
source "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/docker-build-retry.sh"

REPLAY_BIN="${REPLAY_BIN:-$RECORD_BIN}"
NAME=unpublished-port-fixture
IMAGE=unpublished-port-fixture:1.0
RUN_CMD="docker run -p 8095:8095 -p 8097:8097 --rm --name $NAME $IMAGE"

cleanup() {
    echo "cleanup..."
    sudo pkill -9 keploy 2>/dev/null || true
    docker rm -f "$NAME" >/dev/null 2>&1 || true
    sleep 1
}
trap cleanup EXIT

cd "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/python/unpublished-port" || exit 1

docker_build_retry docker build -t "$IMAGE" . || { echo "::error::fixture image build failed"; exit 1; }
sudo rm -rf keploy/ keploy.yml
# The fixture's http.server stamps each response with the current Date.
cat > keploy.yml <<'KEPLOY_CFG'
test:
    globalNoise:
        global: {"header": {"Date":[]}}
KEPLOY_CFG

drive_requests() {
    local port=${1:-8095}
    local up=0
    for _ in $(seq 1 90); do
        if docker logs "$NAME" 2>&1 | grep -q 'self-check passed'; then
            up=1
            break
        fi
        sleep 1
    done
    if [ "$up" -ne 1 ]; then
        echo "::error::the fixture did not finish starting within 90s"
        sudo pkill -INT keploy 2>/dev/null || true
        return 1
    fi
    local failed=0
    for i in 1 2; do
        if ! curl -fsS --max-time 10 -H 'Content-Type: application/json' \
            -d "{\"n\": $i}" "http://127.0.0.1:$port/relay"; then
            echo "::error::live POST /relay $i failed"
            failed=1
        fi
        echo
    done
    sleep 5
    sudo pkill -INT keploy 2>/dev/null || true
    return $failed
}

echo "=== record ==="
drive_requests > driver_logs.txt 2>&1 &
driver_pid=$!
sudo -E env PATH="$PATH" "$RECORD_BIN" record -c "$RUN_CMD" --container-name "$NAME" \
    --generateGithubActions=false --disable-ansi > record_logs.txt 2>&1
echo "record exit status: $?"
if ! wait "$driver_pid"; then
    echo "::error::the request driver failed while recording"
    cat driver_logs.txt
    tail -100 record_logs.txt
    exit 1
fi
cat driver_logs.txt
docker rm -f "$NAME" >/dev/null 2>&1 || true

fail=0
warn="test cases recorded on the app's port 8096 cannot be replayed"
if [ "$(grep -c "$warn" record_logs.txt)" -ne 1 ]; then
    echo "::error::want exactly one record-time warning for port 8096, got $(grep -c "$warn" record_logs.txt)"
    fail=1
fi
if ! grep "$warn" record_logs.txt | grep -q -- 'Publish it: add -p 8096:8096 to the docker command'; then
    echo "::error::the record-time warning does not give the fix (-p 8096:8096)"
    fail=1
fi
# The agent saw the app listen on 0.0.0.0:8096, so publishing is the whole fix.
if grep "$warn" record_logs.txt | grep -q 'does not reach a socket on 127.0.0.1'; then
    echo "::error::the 8096 warning could not tell where the app listens"
    fail=1
fi
warn97="test cases recorded on the app's port 8097 cannot be replayed"
if [ "$(grep -c "$warn97" record_logs.txt)" -ne 1 ]; then
    echo "::error::want exactly one record-time warning for the loopback-only port 8097, got $(grep -c "$warn97" record_logs.txt)"
    fail=1
fi
if ! grep "$warn97" record_logs.txt | grep -q 'the app listens on port 8097 only on 127.0.0.1 inside the container'; then
    echo "::error::the 8097 warning does not say the app listens only on 127.0.0.1"
    fail=1
fi
if grep "$warn97" record_logs.txt | grep -q 'Publish it'; then
    echo "::error::the 8097 warning advises publishing a port that is published and still unreachable"
    fail=1
fi
if grep -q "port 8095 cannot be" record_logs.txt; then
    echo "::error::a warning names the reachable port 8095"
    fail=1
fi
grep "cannot be replayed" record_logs.txt
ls keploy/test-set-0/tests/
if [ "$fail" -ne 0 ]; then
    tail -80 record_logs.txt
    exit 1
fi

echo "=== replay ==="
start=$(date +%s)
sudo -E env PATH="$PATH" "$REPLAY_BIN" test -c "$RUN_CMD" --container-name "$NAME" --delay 10 \
    --generateGithubActions=false --disable-ansi > test_logs.txt 2>&1
echo "test exit status: $? after $(( $(date +%s) - start ))s"

if ! grep -q "the app's port 8096 cannot be reached from the host" test_logs.txt; then
    echo "::error::replay did not say that port 8096 cannot be reached from the host"
    fail=1
fi
if ! grep -q "the app's port 8097 cannot be reached from the host: the app listens on port 8097 only on 127.0.0.1" test_logs.txt; then
    echo "::error::replay did not say that port 8097 cannot be reached from the host because the app listens only on 127.0.0.1"
    fail=1
fi
if grep -q "not yet accepting connections" test_logs.txt; then
    echo "::error::replay still reports an unreachable port as an app not yet accepting connections"
    fail=1
fi
if grep -q "never completed an HTTP round-trip" test_logs.txt; then
    echo "::error::the readiness gate waited out its ceiling probing an unreachable port"
    fail=1
fi
grep -E "cannot be reached from the host|not yet accepting|re-sending|round-trip|readiness" test_logs.txt | head -20

report=keploy/reports/test-run-0/test-set-0-report.yaml
if [ ! -f "$report" ]; then
    echo "::error::no replay report"
    tail -100 test_logs.txt
    exit 1
fi
# Every test on 8095 passes; every test on 8096 and 8097 fails. Only those.
failed_ids=$(yq -r '.tests[] | select(.status == "FAILED") | .test_case_id' "$report" | sort)
passed_ids=$(yq -r '.tests[] | select(.status == "PASSED") | .test_case_id' "$report" | sort)
echo "passed: $(echo $passed_ids)"
echo "failed: $(echo $failed_ids)"
failed_ports=""
for id in $failed_ids; do
    port=$(yq -r '.spec.app_port // .app_port' "keploy/test-set-0/tests/$id.yaml")
    failed_ports="$failed_ports $port"
    if [ "$port" != 8096 ] && [ "$port" != 8097 ]; then
        echo "::error::test $id on port $port failed; only the tests on the unreachable 8096 and 8097 may"
        fail=1
    fi
    # Not re-sent: a reset there comes back on every try.
    if grep "re-sending" test_logs.txt | grep -qF "\"testCaseID\": \"$id\""; then
        echo "::error::test $id on the unreachable $port was re-sent"
        fail=1
    fi
done
for id in $passed_ids; do
    port=$(yq -r '.spec.app_port // .app_port' "keploy/test-set-0/tests/$id.yaml")
    if [ "$port" = 8096 ] || [ "$port" = 8097 ]; then
        echo "::error::test $id on the unreachable $port passed, which the host cannot reach"
        fail=1
    fi
done
for port in 8096 8097; do
    if ! echo "$failed_ports" | grep -qw "$port"; then
        echo "::error::no test on port $port failed; want tests on 8095 passing and on 8096 and 8097 failing"
        fail=1
    fi
done
if [ -z "$passed_ids" ]; then
    echo "::error::want tests on 8095 passing"
    fail=1
fi
if [ "$fail" -ne 0 ]; then
    tail -120 test_logs.txt
    exit 1
fi
echo "=== remapped port: record ==="
# The first replay's container is started with --rm; make sure it is gone, or
# the driver below reads its logs and the name is still taken.
docker rm -f "$NAME" >/dev/null 2>&1 || true
REMAP_DIR="$PWD/remapped"
sudo rm -rf "$REMAP_DIR"
mkdir -p "$REMAP_DIR"
REMAP_RECORD_CMD="docker run -p 18095:8095 --rm --name $NAME $IMAGE"
REMAP_TEST_CMD="docker run -p 18095:8095 -e PUBLIC_START_DELAY=25 --rm --name $NAME $IMAGE"
drive_requests 18095 > remap_driver_logs.txt 2>&1 &
driver_pid=$!
sudo -E env PATH="$PATH" "$RECORD_BIN" record -c "$REMAP_RECORD_CMD" --container-name "$NAME" --path "$REMAP_DIR" \
    --generateGithubActions=false --disable-ansi > remap_record_logs.txt 2>&1
echo "record exit status: $?"
if ! wait "$driver_pid"; then
    echo "::error::the request driver failed while recording on the remapped port"
    cat remap_driver_logs.txt
    tail -100 remap_record_logs.txt
    exit 1
fi
docker rm -f "$NAME" >/dev/null 2>&1 || true
warn95="test cases recorded on the app's port 8095 cannot be replayed at that port from the host: it is published on host port 18095 instead"
if [ "$(grep -c "$warn95" remap_record_logs.txt)" -ne 1 ]; then
    echo "::error::want exactly one record-time warning naming host port 18095 for port 8095, got $(grep -c "$warn95" remap_record_logs.txt)"
    fail=1
fi
if ! grep "$warn95" remap_record_logs.txt | grep -qF "send these tests to host port 18095 with a port map in keploy.yml (test.replaceWith.global.port: {8095: 18095})"; then
    echo "::error::the 8095 warning does not say a port map in keploy.yml can send those tests to host port 18095"
    fail=1
fi
if grep "$warn95" remap_record_logs.txt | grep -q -- "--port"; then
    echo "::error::the 8095 warning points at --port, which sends every test of every set to one port"
    fail=1
fi
grep "cannot be replayed" remap_record_logs.txt

echo "=== remapped port: replay with --port 18095, the app serving 25s after it starts ==="
start=$(date +%s)
sudo -E env PATH="$PATH" "$REPLAY_BIN" test -c "$REMAP_TEST_CMD" --container-name "$NAME" --path "$REMAP_DIR" \
    --delay 5 --port 18095 --generateGithubActions=false --disable-ansi > remap_test_logs.txt 2>&1
echo "test exit status: $? after $(( $(date +%s) - start ))s"
docker rm -f "$NAME" >/dev/null 2>&1 || true
# 8095 is the app's port; keploy before this check knew of no other than the
# host port the test went to, and named that one.
if grep -qE "the app's port (8095|18095) cannot be reached from the host" remap_test_logs.txt; then
    echo "::error::replay said a test on the app's port 8095, sent to host port 18095 which is published to it, cannot be reached"
    fail=1
fi
# The app holds 8095 back 25s and logs each request it serves there. The
# readiness gate's HTTP probe of host port 18095 has to be the first: a gate
# that skipped the probe passes on docker's own listener accepting, and the
# first thing the app sees is a test, or, as before this, nothing at all.
first95=$(grep -m1 -F '[app:8095] "' remap_test_logs.txt)
if ! printf '%s\n' "$first95" | grep -qF '"GET /keploy/__readiness_probe__ '; then
    echo "::error::the first request the app served on 8095 was not the readiness gate's HTTP probe: ${first95:-it served none}"
    fail=1
fi
if grep -q "never completed an HTTP round-trip" remap_test_logs.txt; then
    echo "::error::the readiness gate's HTTP probe of host port 18095 timed out"
    fail=1
fi
grep -E "cannot be reached from the host|re-sending|round-trip|readiness|\[app:8095\]" remap_test_logs.txt | head -20
remap_report="$REMAP_DIR/keploy/reports/test-run-0/test-set-0-report.yaml"
if [ ! -f "$remap_report" ]; then
    echo "::error::no replay report for the remapped port"
    tail -100 remap_test_logs.txt
    exit 1
fi
remap_passed=0
for id in $(yq -r '.tests[] | .test_case_id' "$remap_report"); do
    port=$(yq -r '.spec.app_port // .app_port' "$REMAP_DIR/keploy/test-set-0/tests/$id.yaml")
    [ "$port" = 8095 ] || continue
    status=$(yq -r ".tests[] | select(.test_case_id == \"$id\") | .status" "$remap_report")
    if [ "$status" != PASSED ]; then
        echo "::error::test $id on the app's port 8095, sent to host port 18095 which leads to it, $status"
        fail=1
    else
        remap_passed=$((remap_passed + 1))
    fi
done
if [ "$remap_passed" -eq 0 ]; then
    echo "::error::want tests on 8095 passing through host port 18095"
    fail=1
fi
if [ "$fail" -ne 0 ]; then
    tail -120 remap_test_logs.txt
    exit 1
fi

echo "PASS: each unreachable port is named at record time and at replay, with what would make it reachable, and a port published elsewhere is reached through --port"
exit 0
