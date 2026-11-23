#!/bin/bash
# Step 5 of the proven-record-mappings lane (golang-linux.sh runs it last): a
# connection pool warmed at startup.
#
# The application (pooled/) opens a connection to its dependency, a
# client-speaks-first HTTP server, before it serves anything, and makes its
# first call over it only later, from a test. The MySQL detection probe looks
# at every connection to a port keploy knows nothing about. It used to give a
# silent client 250ms and then wait up to 2s for a server greeting, with the
# client's first bytes unread: a call made meanwhile was held back and stamped
# late, so a recording made without --sync mapped it to the next test, and its
# replay failed. Now whichever end speaks first decides, and the call goes on
# the moment it is made.
#
# Recorded twice under the switch, no --sync: with handshakes held (keploy
# reaches the dependency through the connection dialled while the
# application's connect was held) and with --disable-handshake-hold (the probe
# dials the dependency itself and hands that connection on). The pooled
# connection is left unused past the old probe's silence window before the
# test uses it. Each time:
#   1. keploy's probe decides the connection by its client speaking first,
#      never by a greeting that did not come;
#   2. the dependency sees the application's one connection, not a second one
#      keploy opened;
#   3. mappings.yaml maps the call to the test that made it;
#   4. the recording replays green under the switch with the dependency gone.
#
# The call is made synchronously, so the test that makes it is still open when
# it is recorded however slow the host is; a call made in the background would
# leave the mapping to a race between keploy's read of it and the test's end.
# What the old probe got wrong is pinned by 1 and 2, which do not depend on
# timing.
#
# Needs RECORD_BIN and REPLAY_BIN (this build: the step is only reached when
# both are), go, curl, yq and jq. Stops keploy by its own pid only.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$HERE/../../go-retry.sh"
RECORD_BIN="${RECORD_BIN:?RECORD_BIN must name the keploy binary under test}"
REPLAY_BIN="${REPLAY_BIN:?REPLAY_BIN must name the keploy binary under test}"

WORK="$(mktemp -d)"
LOGS="$PWD"
BROKER=127.0.0.1:9099
APP_PORT=8091
broker_pid=""
record_pid=""
cleanup() {
    if [ -n "$record_pid" ]; then
        sudo kill -INT "$record_pid" 2>/dev/null || true
        wait "$record_pid" 2>/dev/null || true
    fi
    if [ -n "$broker_pid" ]; then
        kill "$broker_pid" 2>/dev/null || true
    fi
    cp "$WORK"/*_logs.txt "$LOGS"/ 2>/dev/null || true
    sudo rm -rf "$WORK"
}
trap cleanup EXIT

fail() {
    echo "::error::$1"
    shift
    for f in "$@"; do
        [ -f "$f" ] && tail -60 "$f"
    done
    exit 1
}

check_race() {
    if grep -q "WARNING: DATA RACE" "$1"; then
        fail "Race condition detected in $1" "$1"
    fi
}

(cd "$HERE/pooled" && go_retry build -o "$WORK/pooled" .)
cd "$WORK"

./pooled -broker "$BROKER" > broker.log 2>&1 &
broker_pid=$!
until curl -fsS "http://$BROKER/conns" > /dev/null 2>&1; do
    kill -0 "$broker_pid" 2>/dev/null || fail "the dependency did not start" broker.log
    sleep 0.2
done

# record NAME [FLAGS...]: record the application under the switch into
# $WORK/NAME, with FLAGS, and check what step 1-3 of the header say.
record() {
    local name=$1
    shift
    local dir="$WORK/$name"
    local log="$WORK/pooled_${name}_record_logs.txt"
    mkdir -p "$dir"
    local before
    before=$(curl -fsS "http://$BROKER/conns") || fail "$name: the dependency did not answer /conns" broker.log

    # --debug: the probe's verdict on each connection is a debug line.
    KEPLOY_PROVEN=1 "$RECORD_BIN" record -c "./pooled -app :$APP_PORT -to $BROKER" --path "$dir" \
        --debug --generateGithubActions=false "$@" > "$log" 2>&1 &
    record_pid=$!
    until curl -fsS "http://localhost:$APP_PORT/ping" > /dev/null 2>&1; do
        kill -0 "$record_pid" 2>/dev/null || fail "$name: keploy record exited before the app answered" "$log"
        sleep 0.5
    done
    # The pooled connection stays unused well past the 250ms the old probe gave
    # a silent client, so the old probe was waiting on a greeting when the call
    # came. This only sets the scene: the probe under test decides by whichever
    # end speaks first, whenever that is.
    sleep 1
    local answer
    answer=$(curl -fsS "http://localhost:$APP_PORT/publish?id=1") || fail "$name: GET /publish failed while recording" "$log"
    [ "$answer" = "ok publish 1" ] || fail "$name: GET /publish answered '$answer', want 'ok publish 1'" "$log"
    curl -fsS "http://localhost:$APP_PORT/ping" > /dev/null || fail "$name: GET /ping failed while recording" "$log"

    sudo kill -INT "$record_pid" 2>/dev/null || true
    local record_rc=0
    wait "$record_pid" || record_rc=$?
    record_pid=""
    check_race "$log"
    [ "$record_rc" -eq 0 ] || fail "$name: keploy record exited $record_rc" "$log"

    # 1. The probe decided the pooled connection by the client's first bytes,
    # never by waiting out a greeting that did not come.
    local port=${BROKER##*:} verdicts
    verdicts=$(grep 'mysql probe result' "$log" | grep "\"destPort\": $port" || true)
    [ -n "$verdicts" ] || fail "$name: keploy logged no MySQL probe verdict for the dependency's port $port" "$log"
    if echo "$verdicts" | grep -v '"reason": "client-spoke-first"' >/dev/null; then
        echo "$verdicts"
        fail "$name: the probe decided the pooled connection by something other than its client speaking first" "$log"
    fi
    # 2. One connection for the application's one.
    local after conns
    after=$(curl -fsS "http://$BROKER/conns") || fail "$name: the dependency did not answer /conns" broker.log
    conns=$(( after - before ))
    [ "$conns" -eq 1 ] || fail "$name: the dependency saw $conns connections for the application's one pooled connection" "$log"

    # 3. The call is mapped to the test that made it.
    shopt -s nullglob
    local sets=( "$dir"/keploy/test-set-* )
    shopt -u nullglob
    [ ${#sets[@]} -eq 1 ] || fail "$name: want one recorded test set, got ${#sets[@]}" "$log"
    local set_dir=${sets[0]}
    local mappings="$set_dir/mappings.yaml"
    [ -f "$mappings" ] || fail "$name: the recording wrote no $mappings" "$log"
    local publish_test="" tc id owned publish_mock
    for tc in "$set_dir"/tests/*.yaml; do
        case "$(yq '.spec.req.url' "$tc")" in
            */publish*) publish_test=$(yq '.name' "$tc") ;;
        esac
    done
    [ -n "$publish_test" ] || fail "$name: no recorded /publish test" "$log"
    publish_mock=$(yq ea 'select(.kind == "Http" and .spec.req.url == "/publish") | .name' "$set_dir/mocks.yaml")
    [ "$(echo "$publish_mock" | grep -c .)" -eq 1 ] || fail "$name: want one recorded POST /publish call, got: ${publish_mock:-none}" "$set_dir/mocks.yaml"
    for tc in "$set_dir"/tests/*.yaml; do
        id=$(yq '.name' "$tc")
        owned=$(ID="$id" yq '.tests[] | select(.id == strenv(ID)) | (.mock_entries // []) | .[].name' "$mappings")
        if [ "$id" = "$publish_test" ]; then
            [ "$owned" = "$publish_mock" ] || fail "$name: $id made the call $publish_mock but owns: ${owned:-nothing}" "$mappings" "$set_dir/mocks.yaml"
        else
            [ -z "$owned" ] || fail "$name: $id made no call, yet owns: $owned (the call was stamped outside the test that made it)" "$mappings" "$set_dir/mocks.yaml"
        fi
    done
    echo "✓ $name: a pool's connection warmed at startup is decided by its client speaking first, keploy adds no connection to it, and its call is mapped to the test that made it ($publish_test owns $publish_mock)"
}

# replay NAME: replay $WORK/NAME under the switch (step 4).
replay() {
    local name=$1
    local log="$WORK/pooled_${name}_test_logs.txt"
    local test_rc=0
    KEPLOY_PROVEN=1 "$REPLAY_BIN" test -c "./pooled -app :$APP_PORT -to $BROKER" --path "$WORK/$name" --delay 3 \
        --generateGithubActions=false > "$log" 2>&1 || test_rc=$?
    check_race "$log"
    "$REPLAY_BIN" report --format json --path "$WORK/$name" > "ndjson_$name.txt" 2> "ndjson_${name}_err.txt" || true
    if [ "$test_rc" -ne 0 ] || ! jq -es 'length > 0 and all(.status == "PASSED")' "ndjson_$name.txt" > /dev/null; then
        jq -s 'map({test_case_id, status})' "ndjson_$name.txt" || true
        fail "$name: the recording must replay green under KEPLOY_PROVEN=1 (exit $test_rc)" "$log"
    fi
    echo "✓ $name: the pooled-connection recording replays green"
}

record held
record unheld --disable-handshake-hold

# The replays answer from the recordings alone: the dependency is gone.
kill "$broker_pid" 2>/dev/null || true
wait "$broker_pid" 2>/dev/null || true
broker_pid=""
replay held
replay unheld
