#!/usr/bin/env bash
# E2E: an app on mysqlclient (MariaDB Connector/C) records and replays against
# MariaDB and MySQL, every exchange recorded and every handshake served.
#
# MariaDB's client library and server agree on MariaDB's extended capabilities
# (the greeting's last four filler bytes and the HandshakeResponse41's): with
# MARIADB_CLIENT_EXTENDED_METADATA a column definition carries an extended type
# info field (a JSON column says format "json"), and with
# MARIADB_CLIENT_CACHE_METADATA a result set's column count is followed by a
# metadata-follows byte. A recorder that reads neither lost every result set
# with a JSON column ("... overruns the packet"), and a replay that sent the
# greeting without them got a HandshakeResponse41 matching no recorded one.
#
#   1. Record (database up): GET /rows (the JSON column) and GET /plain, five
#      requests, each on its own connection. Every exchange is recorded: the
#      log names no exchange left out, the mocks hold three SELECTs of the JSON
#      column, two of the others and five handshakes.
#   2. keploy test (database down): every test passes and no handshake is
#      missed.
#
# Env: RECORD_BIN, REPLAY_BIN; DB_IMAGE (default mariadb:10.6; mysql:8.0 too).
# The python3 on PATH must have mysqlclient. Readiness is event-driven: the
# server answering SELECT 1, the app's port, the test files keploy writes.
# Ports: 8096 (app), 3306 (database).

# keploy runs under sudo, which hands it only the variables its command line
# names (sudo-rs, Ubuntu 26.04's sudo, ignores -E): SUDO_ENV names the
# switches a workflow sets for keploy, each only while it is set.
# shellcheck source=../../sudo_env.sh
source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/sudo_env.sh" || exit 1

set -uo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/../../docker-build-retry.sh"

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-$RECORD_BIN}"
DB_IMAGE="${DB_IMAGE:-mariadb:10.6}"
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
export APP_PORT=8096 DB_PORT=3306 DB_PASSWORD=password
APP_CMD="python3 $SRC/app.py"
FAIL=0
fail() {
    echo "::error::[$DB_IMAGE] $*"
    FAIL=$((FAIL + 1))
}

python3 -c 'import MySQLdb; print("mysqlclient", MySQLdb.version_info, "client library", MySQLdb.get_client_info())' ||
    { echo "::error::python3 has no mysqlclient"; exit 1; }

WORK="$(mktemp -d)"
echo "work dir: $WORK"
cd "$WORK" || exit 1

listening() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
# pid_running PID: by its state in /proc (keploy runs under sudo, so kill -0
# from this user fails on a sudo that is alive).
pid_running() {
    local st
    case $1 in "" | *[!0-9]*) return 1 ;; esac
    { read -r st <"/proc/$1/stat"; } 2>/dev/null || return 1
    st=${st##*) }
    [ "${st%% *}" != Z ]
}
wait_listen() {
    until listening "$1"; do
        pid_running "$2" || return 1
        sleep 0.2
    done
}
test_files() { find keploy/test-set-0/tests -maxdepth 1 -name '*.yaml' -size +0c 2>/dev/null | wc -l; }
LEFT_OUT='framing lost|overruns|not recorded'
# wait_tests <n> <pid>: until keploy wrote n test cases; fails at once when
# keploy has exited, or has left an exchange out (it then writes no test case
# over that exchange, so the count would never come).
wait_tests() {
    until [ "$(test_files)" -ge "$1" ]; do
        pid_running "$2" || return 1
        grep -qE "$LEFT_OUT" record.log && return 1
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

DB_CID=""
db_sql() { docker exec "$DB_CID" sh -c "\"\$(command -v mariadb || command -v mysql)\" -h127.0.0.1 -uroot -p$DB_PASSWORD -N -e \"\$1\"" sh "$1"; }
start_db() {
    docker_pull_retry "$DB_IMAGE"
    DB_CID=$(docker run -d --init -p "127.0.0.1:$DB_PORT:3306" -e MYSQL_ROOT_PASSWORD="$DB_PASSWORD" \
        -e MARIADB_ROOT_PASSWORD="$DB_PASSWORD" "$DB_IMAGE") || { echo "::error::cannot start $DB_IMAGE"; exit 1; }
    # Ready when the server answers a query over TCP (the image's first,
    # socket-only server answers the socket alone, and exits).
    until db_sql 'SELECT 1' >/dev/null 2>&1; do
        [ "$(docker inspect -f '{{.State.Running}}' "$DB_CID" 2>/dev/null)" = true ] ||
            { echo "::error::$DB_IMAGE exited before it was ready"; docker logs "$DB_CID" | tail -30; exit 1; }
        sleep 0.5
    done
    echo "server $(db_sql 'SELECT VERSION()')"
    db_sql "CREATE DATABASE demo; USE demo;
        CREATE TABLE t (id INT PRIMARY KEY, name VARCHAR(140), data JSON, note LONGTEXT);
        INSERT INTO t VALUES (1, 'a', '{\"k\": 1}', 'first'), (2, 'b', '[1, 2]', NULL);" ||
        { echo "::error::cannot create the schema"; exit 1; }
}
stop_db() {
    [ -n "$DB_CID" ] && docker rm -f "$DB_CID" >/dev/null 2>&1
    DB_CID=""
}

REC_PID=""
cleanup() {
    [ -n "$REC_PID" ] && stop_pid "$REC_PID"
    stop_db
}
trap cleanup EXIT

get() {
    local code
    code=$(curl -s -o "resp-$2.json" -w '%{http_code}' "http://127.0.0.1:$APP_PORT$1")
    [ "$code" = 200 ] || fail "GET $1 answered $code while recording, want 200: $(head -c 300 "resp-$2.json")"
}

# 1. Record.
start_db
sudo env PATH="$PATH" "${SUDO_ENV[@]}" APP_PORT="$APP_PORT" DB_PORT="$DB_PORT" DB_PASSWORD="$DB_PASSWORD" "$RECORD_BIN" record -c "$APP_CMD" \
    --generateGithubActions=false --disable-tele >record.log 2>&1 </dev/null &
REC_PID=$!
wait_listen "$APP_PORT" "$REC_PID" || { echo "::error::the app did not start under keploy record"; tail -30 record.log; exit 1; }
get /rows 1
get /plain 2
get /rows 3
get /plain 4
get /rows 5
python3 -c 'import json, sys; r = json.load(open(sys.argv[1])); sys.exit(json.loads(r["rows"][0][2]) != {"k": 1})' resp-1.json 2>/dev/null ||
    fail "GET /rows did not read the JSON column: $(head -c 300 resp-1.json)"
wait_tests 5 "$REC_PID" || fail "keploy record wrote $(test_files) test cases, want 5"
stop_pid "$REC_PID"
REC_PID=""
own
stop_db

MOCKS=keploy/test-set-0/mocks.yaml
left_out=$(grep -cE "$LEFT_OUT" record.log)
if [ "$left_out" -ne 0 ]; then
    fail "keploy record left $left_out exchanges out"
    grep -E "$LEFT_OUT" record.log | head -5 | cut -c1-400
fi
mocked() { grep -cF "query: $1" "$MOCKS" 2>/dev/null; }
n=$(mocked 'SELECT id, name, data, note FROM t ORDER BY id')
[ "$n" = 3 ] || fail "the mocks hold $n SELECTs of the JSON column, want 3"
n=$(mocked 'SELECT id, name, note FROM t ORDER BY id')
[ "$n" = 2 ] || fail "the mocks hold $n SELECTs of the other columns, want 2"
n=$(grep -c 'packet_type: HandshakeResponse41' "$MOCKS" 2>/dev/null)
[ "$n" = 5 ] || fail "the mocks hold $n handshakes, want 5 (one per request)"

# 2. Replay, the database down.
sudo env PATH="$PATH" "${SUDO_ENV[@]}" APP_PORT="$APP_PORT" DB_PORT="$DB_PORT" DB_PASSWORD="$DB_PASSWORD" "$REPLAY_BIN" test -c "$APP_CMD" --delay 1 --health-path /healthz \
    --generateGithubActions=false --disable-tele >test.log 2>&1 </dev/null
RC=$?
own
REPORT="$(ls -1d keploy/reports/test-run-* 2>/dev/null | sort -t- -k3 -n | tail -n1)/test-set-0-report.yaml"
status=$(grep -m1 '^status:' "$REPORT" 2>/dev/null | awk '{print $2}')
if [ "$RC" -ne 0 ] || [ "$status" != PASSED ]; then
    fail "keploy test must pass every test (exit $RC, status ${status:-no report})"
    grep -nE '^status:|^    status:|test_case_id|body: ' "$REPORT" 2>/dev/null | head -40
    tail -30 test.log
fi
for field in success total; do
    n=$(grep -m1 "^$field:" "$REPORT" 2>/dev/null | awk '{print $2}')
    [ "$n" = 5 ] || fail "the report says $field: ${n:-none}, want 5"
done
misses=$(grep -c 'no mysql mocks matched' test.log)
if [ "$misses" -ne 0 ]; then
    fail "keploy test missed $misses MySQL mocks"
    grep 'no mysql mocks matched' test.log | head -3 | cut -c1-400
fi

if [ "$FAIL" -ne 0 ]; then
    echo "::error::[$DB_IMAGE] $FAIL check(s) failed; logs in $WORK"
    exit 1
fi
echo "[$DB_IMAGE] mysqlclient: 5 tests recorded with every exchange, replayed, all passed"
