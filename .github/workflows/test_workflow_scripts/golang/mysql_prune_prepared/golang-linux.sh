#!/usr/bin/env bash
# E2E (keploy #4810): the mock prune must not delete a COM_STMT_PREPARE that a
# kept COM_STMT_EXECUTE still needs.
#
# The app (./app) runs through go-sql-driver/mysql's default DSN, so each
# parameterised statement is PREPARE -> EXECUTE -> CLOSE on the wire, on a
# pool of one connection: the same INSERT is prepared again, under a new
# statement ID, for every order. One order reuses an idempotency key, its
# INSERT fails with ERR 1062, and the app answers 500.
#
#   1. Record: five orders (the startup window the prune never touches),
#      then two more, one with a key already used (ERR 1062, 500), and reads.
#   1b. On a copy of the recording, the damage the old prune left: every
#      PREPARE but the first of each query deleted, and the queries the
#      recorder now keeps on EXECUTEs, and the lifetime tags, removed (a
#      recording made before either). That set cannot be repaired from its
#      mocks: keploy test must fail, say why, and serve no test another
#      test's EXECUTE.
#   2. keploy test --remove-unused-mocks (the prune auto-replay runs): every
#      test passes. A live PREPARE is answered by the first recorded PREPARE
#      of its SQL, so the other copies go unconsumed, and the prune used to
#      delete them while it kept the EXECUTEs that name their statement IDs.
#   3. The pruned mocks: no MySQL mock names a statement (connection,
#      statement ID) that no kept PREPARE prepared.
#   4. keploy test again: every test passes, the duplicate-key test among
#      them, and no test is served a COM_STMT_EXECUTE recorded for another
#      test (each test's consumed EXECUTEs are in its own mapping, or recorded
#      outside every test).
#
# It runs under both mock windows: strict (the default) and lax
# (strictMockWindow: false, as the k8s-proxy's agent runs, where another
# test's mocks are in the pool). Readiness is event-driven: MySQL answering
# SELECT 1, the app's port, the test files keploy writes. Ports: 8095 (app),
# 3306 (MySQL). Env: RECORD_BIN, REPLAY_BIN; MYSQL_IMAGE (default mysql:8.0).
set -uo pipefail
# This lane guards the switch-off prune (--remove-unused-mocks), which the
# proven switch ignores (design v14 U18). So it runs keploy with the switch
# off (KEPLOY_PROVEN=0), where that still works: the default on Windows and
# macOS. Unset, a Linux keploy runs the proven model.
export KEPLOY_PROVEN=0

# docker_pull_retry: a registry hiccup is retried and named, not taken for a
# failed lane.
source "$(dirname "${BASH_SOURCE[0]}")/../../docker-build-retry.sh"

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-$RECORD_BIN}"
MYSQL_IMAGE="${MYSQL_IMAGE:-mysql:8.0}"
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP=8095
FAIL=0
fail() {
    echo "::error::$*"
    FAIL=$((FAIL + 1))
}

WORK="$(mktemp -d)"
echo "work dir: $WORK"
(cd "$SRC" && go build -o "$WORK/orders" ./app) || { echo "::error::build failed"; exit 1; }
cd "$WORK" || exit 1

listening() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
# wait_listen <port> <pid>: until something listens on port; fails at once
# when pid (the process that should start it) has exited.
wait_listen() {
    until listening "$1"; do
        kill -0 "$2" 2>/dev/null || return 1
        sleep 0.2
    done
}
test_files() { find keploy/test-set-0/tests -maxdepth 1 -name '*.yaml' -size +0c 2>/dev/null | wc -l; }
wait_tests() {
    until [ "$(test_files)" -ge "$1" ]; do
        kill -0 "$2" 2>/dev/null || return 1
        sleep 0.2
    done
}
own() { sudo chown -R "$(id -u):$(id -g)" . 2>/dev/null || true; }
stop_pid() {
    local sudo_pid="$1" child
    child="$(pgrep -P "$sudo_pid" 2>/dev/null | head -n1)"
    [ -n "$child" ] && sudo kill -INT "$child" 2>/dev/null
    wait "$sudo_pid" 2>/dev/null
}
newest_run() { ls -1d keploy/reports/test-run-* 2>/dev/null | sort -t- -k3 -n | tail -n1; }
REPORT() { echo "$(newest_run)/test-set-0-report.yaml"; }
set_status() { grep -m1 '^status:' "$(REPORT)" 2>/dev/null | awk '{print $2}'; }

MYSQL_CID=""
start_mysql() {
    docker_pull_retry "$MYSQL_IMAGE"
    MYSQL_CID=$(docker run -d --init -p 127.0.0.1:3306:3306 -e MYSQL_ROOT_PASSWORD=password \
        -e MYSQL_DATABASE=orders "$MYSQL_IMAGE") || { echo "::error::cannot start $MYSQL_IMAGE"; exit 1; }
    # Ready when the server answers a query over TCP (the image's first,
    # socket-only server answers the socket alone, and exits).
    until docker exec "$MYSQL_CID" mysql -h127.0.0.1 -uroot -ppassword -e 'SELECT 1' orders >/dev/null 2>&1; do
        [ "$(docker inspect -f '{{.State.Running}}' "$MYSQL_CID" 2>/dev/null)" = true ] ||
            { echo "::error::MySQL exited before it was ready"; docker logs "$MYSQL_CID" | tail -30; exit 1; }
        sleep 0.5
    done
}
stop_mysql() {
    [ -n "$MYSQL_CID" ] && docker rm -f "$MYSQL_CID" >/dev/null 2>&1
    MYSQL_CID=""
}

REC_PID=""
cleanup() {
    [ -n "$REC_PID" ] && stop_pid "$REC_PID"
    stop_mysql
}
trap cleanup EXIT

# order <key> <want status>: POST /orders, checked against the live database.
order() {
    local code
    code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "http://127.0.0.1:$APP/orders" \
        -H 'Content-Type: application/json' -d "{\"key\":\"$1\",\"item\":\"widget-$1\",\"qty\":2}")
    [ "$code" = "$2" ] || fail "POST /orders $1 answered $code while recording, want $2"
}
read_order() {
    local code
    code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$APP/orders/$1")
    [ "$code" = "$2" ] || fail "GET /orders/$1 answered $code while recording, want $2"
}

TESTS=11
record() {
    rm -rf keploy
    start_mysql
    sudo -E env PATH="$PATH" "$RECORD_BIN" record -c "./orders" \
        --generateGithubActions=false --disable-tele >record.log 2>&1 </dev/null &
    REC_PID=$!
    wait_listen "$APP" "$REC_PID" || { echo "::error::the app did not start under keploy record"; tail -30 record.log; exit 1; }
    # Tests 1-5: the startup window (models.StartupMockTestCaseWindow), whose
    # mocks the prune keeps whatever was consumed.
    for k in s1 s2 s3 s4 s5; do order "$k" 201; done
    order a1 201
    order a2 201
    order a1 500 # duplicate idempotency key: ERR 1062
    read_order a1 200
    order a3 201
    read_order a3 200
    wait_tests "$TESTS" "$REC_PID" || fail "keploy record wrote $(test_files) test cases, want $TESTS"
    stop_pid "$REC_PID"
    REC_PID=""
    own
    stop_mysql
}

# replay <name> [flags...]: keploy test of the recording, MySQL down; RC is
# its exit code.
replay() {
    local name="$1" before
    shift
    before="$(newest_run)"
    sudo -E env PATH="$PATH" "$REPLAY_BIN" test -c "./orders" --delay 1 --health-path /healthz \
        --generateGithubActions=false --disable-tele "$@" >"$name.log" 2>&1 </dev/null
    RC=$?
    own
    if [ "$(newest_run)" = "$before" ] || [ ! -f "$(REPORT)" ]; then
        fail "$name: keploy test wrote no report (exit $RC)"
        tail -40 "$name.log"
    fi
}
expect_pass() {
    if [ "$RC" -ne 0 ] || [ "$(set_status)" != PASSED ]; then
        fail "$1: the set must pass (exit $RC, status $(set_status))"
        grep -nE '^status:|^    status:|test_case_id|body: ' "$(REPORT)" | head -60
    fi
}

# mysql_rows: one line per MySQL mock: name, connID, first request type, its
# statement ID, first response type, its statement ID.
mysql_rows() {
    yq -N 'select(.kind == "MySQL") | [.name, .spec.metadata.connID,
        .spec.requests[0].header.packet_type, (.spec.requests[0].message.statement_id // "-"),
        (.spec.responses[0].header.packet_type // "-"), (.spec.responses[0].message.statement_id // "-")] | @tsv' "$1"
}
# orphans <mocks.yaml>: the MySQL mocks naming a statement no PREPARE in the
# file prepared, one per line.
orphans() {
    mysql_rows "$1" | awk -F'\t' '
        $5 == "COM_STMT_PREPARE_OK" { prepared[$2 SUBSEP $6] = 1 }
        $3 ~ /^COM_STMT_(EXECUTE|CLOSE|RESET|SEND_LONG_DATA)$/ { refs[NR] = $1 "\t" $3 "\t" $2 SUBSEP $4; key[NR] = $2 SUBSEP $4 }
        END { for (i in refs) if (!(key[i] in prepared)) { split(refs[i], f, "\t"); print f[1] " " f[2] } }'
}
count() { mysql_rows "$1" | awk -F'\t' -v t="$2" '$3 == t' | wc -l; }

# old_prune <mocks.yaml>: the damage an older keploy's prune left (#4810) on
# a recording made before EXECUTEs kept their query and before recorders
# stated each mock's lifetime: every PREPARE but the first recorded of each
# query deleted, and the stored queries and the lifetime tags
# (spec.metadata.lifetime) removed. An older keploy that rewrites a newer
# recording drops both too, as fields it does not know.
old_prune() {
    local drop
    drop=$(yq -N 'select(.kind == "MySQL" and .spec.requests[0].header.packet_type == "COM_STMT_PREPARE") | [.name, .spec.requests[0].message.query] | @tsv' "$1" |
        awk -F'\t' 'NF == 2 && seen[$2]++ {print $1}' | paste -sd, -)
    [ -n "$drop" ] || { echo "::error::no repeated PREPARE to delete in $1"; return 1; }
    DROP="$drop" yq -i 'select(.name as $n | (strenv(DROP) | split(",") | any_c(. == $n)) | not)' "$1" &&
        yq -i 'del(.spec.requests[].meta.query)' "$1" &&
        yq -i 'del(.spec.metadata.lifetime)' "$1"
}

# served_across <report>: the COM_STMT_EXECUTE mocks a test consumed that
# belong to another test (in another test's mappings.yaml entry and not in its
# own), one per line. Only EXECUTEs: they carry the answers the app reads. A
# PREPARE answers every test's PREPARE of its query, and a CLOSE answers
# nothing, so any test may consume either. A mock in no test's entry
# (recorded before the first test) is any test's to consume.
served_across() {
    local mappings=keploy/test-set-0/mappings.yaml executes t m mine other
    [ -f "$mappings" ] || { echo "no mappings.yaml"; return; }
    executes=$(yq -N 'select(.kind == "MySQL" and .spec.requests[0].header.packet_type == "COM_STMT_EXECUTE") | .name' keploy/test-set-0/mocks.yaml) ||
        { echo "unreadable mocks.yaml"; return; }
    while IFS=$'\t' read -r t m; do
        grep -qxF -- "$m" <<<"$executes" || continue
        mine=$(T="$t" M="$m" yq -N '[.tests[] | select(.id == strenv(T)) | .mock_entries[] | select(.name == strenv(M))] | length' "$mappings") || { echo "unreadable $mappings"; return; }
        other=$(T="$t" M="$m" yq -N '[.tests[] | select(.id != strenv(T)) | .mock_entries[] | select(.name == strenv(M))] | length' "$mappings") || { echo "unreadable $mappings"; return; }
        if [ "$mine" = 0 ] && [ "$other" != 0 ]; then echo "$t was served $m"; fi
    done < <(yq -N '.tests[] | .test_case_id as $t | (.mock_mismatches.actual_mocks // [])[] | select(.kind == "MySQL") | [$t, .name] | @tsv' "$1")
}
# consumed_mysql <report>: how many MySQL mocks the report says its tests
# consumed; 0 means the report cannot show a cross-test serve.
consumed_mysql() {
    yq -N '[.tests[] | (.mock_mismatches.actual_mocks // [])[] | select(.kind == "MySQL")] | length' "$1"
}

for p in "$APP" 3306; do
    if listening "$p"; then echo "::error::port $p is in use"; exit 1; fi
done

for mode in strict lax; do
    echo "== $mode mock window =="
    mkdir -p "$WORK/$mode" && cd "$WORK/$mode" || exit 1
    cp "$WORK/orders" .
    if [ "$mode" = lax ]; then
        printf 'test:\n    strictMockWindow: false\n' >keploy.yml
    fi
    record
    cp keploy/test-set-0/mocks.yaml mocks.recorded.yaml
    echo "recorded: $(count mocks.recorded.yaml COM_STMT_PREPARE) PREPARE, $(count mocks.recorded.yaml COM_STMT_EXECUTE) EXECUTE, $(count mocks.recorded.yaml COM_STMT_CLOSE) CLOSE"
    [ "$(count mocks.recorded.yaml COM_STMT_PREPARE)" -ge "$TESTS" ] ||
        fail "$mode: the recording holds $(count mocks.recorded.yaml COM_STMT_PREPARE) PREPAREs: the driver did not prepare each statement"
    [ -z "$(orphans mocks.recorded.yaml)" ] || fail "$mode: the recording itself has orphaned statements: $(orphans mocks.recorded.yaml | tr '\n' ' ')"

    # 1b: the damaged copy.
    rm -rf "$WORK/$mode-damaged" && mkdir -p "$WORK/$mode-damaged" &&
        cp -r keploy orders "$WORK/$mode-damaged/" && { [ ! -f keploy.yml ] || cp keploy.yml "$WORK/$mode-damaged/"; }
    (
        FAIL=0 # this subshell's own failures: its exit status
        cd "$WORK/$mode-damaged" || exit 1
        old_prune keploy/test-set-0/mocks.yaml || exit 1
        echo "damaged: $(count keploy/test-set-0/mocks.yaml COM_STMT_PREPARE) PREPARE, $(orphans keploy/test-set-0/mocks.yaml | wc -l) EXECUTEs naming a deleted statement"
        replay "$mode-damaged"
        if [ "$RC" -eq 0 ] || [ "$(set_status)" != FAILED ]; then
            fail "$mode: the damaged set must fail (exit $RC, status $(set_status))"
        fi
        grep -q "gone from the test set" "$(REPORT)" "$mode-damaged.log" ||
            fail "$mode: nothing says the damaged set lost its PREPAREs"
        across="$(served_across "$(REPORT)")"
        [ -z "$across" ] || fail "$mode: the damaged set served a test another test's COM_STMT_EXECUTE: $(echo "$across" | tr '\n' ' ')"
        if grep -q 'WARNING: DATA RACE' "$mode-damaged.log"; then fail "$mode: data race in the damaged set's keploy log"; fi
        exit "$FAIL"
    ) || FAIL=$((FAIL + 1))

    replay "$mode-prune" --remove-unused-mocks --debug
    expect_pass "$mode: keploy test --remove-unused-mocks"
    # The precondition of #4810: the prune had to keep PREPAREs that kept
    # EXECUTEs need. Without it the checks below prove nothing. It keeps
    # their CLOSEs too, which alone would not show it.
    deps=$(grep -oE '"keptAsDependencyMocks": ?\[[^]]*\]' "$mode-prune.log" | grep -oE '"[^"]+"' | grep -vxF '"keptAsDependencyMocks"' | tr -d '"')
    prepares=$(yq -N 'select(.kind == "MySQL" and .spec.requests[0].header.packet_type == "COM_STMT_PREPARE") | .name' mocks.recorded.yaml)
    { [ -n "$deps" ] && grep -qxF -f <(printf '%s\n' "$deps") <<<"$prepares"; } ||
        fail "$mode: the prune kept no PREPARE as a dependency: the recording no longer reproduces #4810"
    echo "pruned: $(count keploy/test-set-0/mocks.yaml COM_STMT_PREPARE) PREPARE, $(count keploy/test-set-0/mocks.yaml COM_STMT_EXECUTE) EXECUTE, $(count keploy/test-set-0/mocks.yaml COM_STMT_CLOSE) CLOSE kept"
    left="$(orphans keploy/test-set-0/mocks.yaml)"
    if [ -n "$left" ]; then
        fail "$mode: the prune left $(echo "$left" | wc -l) MySQL mocks naming a statement whose PREPARE it deleted: $(echo "$left" | tr '\n' ' ')"
    fi

    replay "$mode-after-prune"
    expect_pass "$mode: keploy test after the prune"
    [ "$(consumed_mysql "$(REPORT)")" -gt 0 ] || fail "$mode: the report lists no consumed MySQL mock, so it cannot show what each test was served"
    across="$(served_across "$(REPORT)")"
    [ -z "$across" ] || fail "$mode: a test was served another test's COM_STMT_EXECUTE: $(echo "$across" | tr '\n' ' ')"
    if grep -q 'WARNING: DATA RACE' ./*.log; then fail "$mode: data race in a keploy log"; fi
    cd "$WORK" || exit 1
done

if [ "$FAIL" -ne 0 ]; then
    echo "$FAIL check(s) failed"
    exit 1
fi
echo "all checks passed"
