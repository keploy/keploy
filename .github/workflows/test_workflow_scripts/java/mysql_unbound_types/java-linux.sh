#!/usr/bin/env bash
# E2E: MySQL COM_STMT_EXECUTEs that send no parameter types
# (new_params_bind_flag 0) are recorded and replayed.
#
# A client sends an EXECUTE without its parameter types when they did not
# change since the statement's last EXECUTE that sent them, and the server
# reads its values with those: Connector/J re-executing a server-prepared
# statement without rebinding it, and Connector/J 5.1 rebinding it with the
# same types. keploy's decoder read every such value as a string, so the
# EXECUTE failed to decode: its exchange was not recorded, and the test case
# over it was left out of the recording (or, at replay, the connection was
# dropped).
#
# For Connector/J 8.4 (GET /again: no rebinding) and 5.1 (GET /loop, /again,
# /strings and /small), against a real MySQL 8.0:
#   1. Record: every request is recorded as a test case, no MySQL exchange is
#      left unrecorded, and the EXECUTEs sent without types are recorded with
#      the values they bound, read with the statement's types.
#   2. Replay, MySQL down: every test passes, and again with
#      --mock-noise-strict, where a recorded EXECUTE whose parameters drifted
#      is not served.
# And a recording made by a released keploy (LEGACY_VERSION, before the fix),
# of Connector/J 5.1 rebinding two strings (GET /strings) and a small integer
# and a string (GET /small): it recorded those EXECUTEs, every value read as
# a string, with the type FieldTypeString (254) that was never on the wire;
# a small integer as bytes of garbage. This build reads them with the
# statement's types, so it must not compare types a packet did not send, and
# must read the live packet as that keploy did for such a recording: the
# replay, plain and strict, must pass.
#
# COM_STMT_EXECUTEs with a query attribute (GET /pattr: a server-prepared
# statement executed with a traceparent that differs on every call, as
# Connector/J 8.1+ sends OpenTelemetry's): they must replay, plain and strict.
# keploy compared an EXECUTE's attributes as its statement's parameters, so
# no EXECUTE of a traced statement matched its recording: the strict replay
# refused every one, and a plain one was answered by recorded order.
#
# COM_QUERYs with query attributes, from Connector/J 8.4 (GET /attr: a
# traceparent that differs on every call, as OpenTelemetry's does; /attrs: a
# second attribute; /trace: one fixed traceparent; /attrread: a statement
# reading its attribute with mysql_query_attribute_string()). A released
# keploy read no attribute's name: it took the name for the value and glued
# the value to the head of the query, so a query with a per-call traceparent
# never replayed, and one with two attributes was left out of the recording.
# This build must record each query as it was sent, with its attributes
# named, and the replay, plain and strict, must pass: an attribute is not
# compared unless the statement reads it. And a released keploy's recording
# of /trace (its queries' text holding the fixed traceparent) must still
# replay: this build reads the live packet as that keploy did for it.
#
# Readiness is event-driven: MySQL answering a query, the app's /healthz, and
# a last /healthz whose test case is written after every earlier request's.
# Ports: 8096 (app), 3306 (MySQL). Env: RECORD_BIN, REPLAY_BIN; MYSQL_IMAGE
# (default mysql:8.0); LEGACY_RECORD_BIN (default: LEGACY_VERSION, v3.6.126,
# downloaded); WORK (default: a new temporary directory).
set -uo pipefail

source "$(dirname "${BASH_SOURCE[0]}")/../../docker-build-retry.sh"

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-$RECORD_BIN}"
MYSQL_IMAGE="${MYSQL_IMAGE:-mysql:8.0}"
LEGACY_VERSION="${LEGACY_VERSION:-v3.6.126}"
# The app's GET /trace sends it with every query (app/src/main/java/Loop.java).
FIXED_TRACEPARENT=00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FAIL=0
fail() {
    echo "::error::$*"
    FAIL=$((FAIL + 1))
}
WORK="${WORK:-$(mktemp -d)}"
mkdir -p "$WORK"
echo "work dir: $WORK"

if [ -z "${LEGACY_RECORD_BIN:-}" ]; then
    mkdir -p "$WORK/legacy-bin"
    curl -fsSL --retry 5 --retry-delay 2 --retry-connrefused \
        "https://github.com/keploy/keploy/releases/download/$LEGACY_VERSION/keploy_linux_amd64.tar.gz" |
        tar -xz -C "$WORK/legacy-bin" keploy || { echo "::error::could not download keploy $LEGACY_VERSION"; exit 1; }
    LEGACY_RECORD_BIN="$WORK/legacy-bin/keploy"
fi

own() { sudo chown -R "$(id -u):$(id -g)" . 2>/dev/null || true; }
stop_pid() {
    local sudo_pid="$1" child
    child="$(pgrep -P "$sudo_pid" 2>/dev/null | head -n1)"
    [ -n "$child" ] && sudo kill -INT "$child" 2>/dev/null
    wait "$sudo_pid" 2>/dev/null
}
MYSQL_CID=""
REC_PID=""
cleanup() {
    [ -n "$REC_PID" ] && stop_pid "$REC_PID"
    [ -n "$MYSQL_CID" ] && docker rm -f "$MYSQL_CID" >/dev/null 2>&1
}
trap cleanup EXIT

execute_rows() {
    yq -N 'select(.kind == "MySQL" and .spec.requests[0].header.packet_type == "COM_STMT_EXECUTE") |
        [.spec.requests[0].message.new_params_bind_flag, ([.spec.requests[0].message.parameters[]?.type] | join(",")),
         ([.spec.requests[0].message.parameters[]?.value] | join("|"))] | @tsv' keploy/test-set-0/mocks.yaml |
        sed '/^$/d'
}

# check_executes <expect>: the EXECUTEs are recorded as this build records
# them (current) or as a released keploy did (legacy).
check_executes() {
    local unbound bad
    unbound=$(execute_rows | awk -F'\t' '$1 == 0' | wc -l)
    [ "$unbound" -gt 0 ] || fail "$CELL: no EXECUTE was sent without its types, so nothing here tested that"
    if [ "$1" = legacy ]; then
        # What the released keploy wrote for an EXECUTE sent without types:
        # every value read as a string (a small integer's bytes of garbage),
        # with the type FieldTypeString (254).
        bad=$(execute_rows | awk -F'\t' '$1 == 0 && $2 != "254,254"')
    else
        # Every EXECUTE, sent with or without its types: an INT and a
        # string, or two strings; or (GET /pattr) an INT and its traceparent.
        bad=$(execute_rows | awk -F'\t' '$2 == "3,253" && $3 ~ /^[0-9]+\|(item|again|small)-[0-9]+$/ { next }
            $2 == "253,253" && $3 ~ /^s[0-9]+\|item-[0-9]+$/ { next }
            $2 == "3,254" && $3 ~ /^[0-9]+\|00-[0-9a-f]+-[0-9a-f]+-01$/ { next } { print }')
    fi
    [ -z "$bad" ] || fail "$CELL: EXECUTEs recorded with the wrong types or values: $(echo "$bad" | head -3 | tr '\n' ' ')"
    echo "$CELL: $unbound EXECUTEs sent without types"
}

# attr_rows: query, attribute names and values of each recorded COM_QUERY
# that carries query attributes (yq prints an empty line for each other
# document).
attr_rows() {
    local tab=$'\t'
    yq -N 'select(.kind == "MySQL" and .spec.requests[0].header.packet_type == "COM_QUERY" and (.spec.requests[0].message.parameters | length) > 0) |
        [.spec.requests[0].message.query, ([.spec.requests[0].message.parameters[] | .name // ""] | join(",")),
         ([.spec.requests[0].message.parameters[] | .value // ""] | join("|"))] | join("'"$tab"'")' keploy/test-set-0/mocks.yaml |
        sed '/^$/d'
}

# check_attrs <expect> <count>: count queries with attributes are recorded,
# as this build records them (current) or as a released keploy did (legacy:
# GET /trace's, the name taken for the value and the value glued to the head
# of the query).
check_attrs() {
    local n bad
    n=$(attr_rows | wc -l)
    [ "$n" -eq "$2" ] || fail "$CELL: recorded $n queries with attributes, want $2: $(attr_rows | head -3 | tr '\n' ' ')"
    if [ "$1" = legacy ]; then
        bad=$(attr_rows | awk -F'\t' -v tp="7$FIXED_TRACEPARENT" 'index($1, tp) == 1 && substr($1, length(tp) + 1) ~ /^SELECT [0-9]+ AS v$/ && $2 == "" && $3 == "traceparent" { next } { print }')
    else
        # Each value as sent: a traceparent, and /attrs' span s<i> after it.
        bad=$(attr_rows | awk -F'\t' '$1 ~ /^SELECT [0-9]+ AS v$/ && $2 == "traceparent" && $3 ~ /^00-[0-9a-f]+-[0-9a-f]+-01$/ { next }
            $1 ~ /^SELECT [0-9]+ AS v$/ && $2 == "traceparent,span" && $3 ~ /^00-[0-9a-f]+-[0-9a-f]+-01\|s[0-9]+$/ { next }
            $1 == "SELECT mysql_query_attribute_string('"'"'span'"'"') AS v" && $2 == "span" && $3 ~ /^s[0-9]+$/ { next } { print }')
    fi
    [ -z "$bad" ] || fail "$CELL: queries with attributes recorded wrong: $(echo "$bad" | head -3 | tr '\n' ' ')"
}

# tests_of <regex>: how many recorded test cases request a URL matching it
# (by URL, not file name: a released keploy names them otherwise).
tests_of() {
    local f n=0
    for f in keploy/test-set-0/tests/*.yaml; do
        [ -f "$f" ] || continue
        yq -e '.spec.req.url' "$f" 2>/dev/null | grep -Eq "$1" && n=$((n + 1))
    done
    echo "$n"
}

# replay <name> <flags...>: replay the recording in the current directory
# with REPLAY_BIN; every test must pass.
replay() {
    local name="$1" rc report status
    shift
    sudo rm -rf keploy/reports
    sudo -E env PATH="$PATH" "$REPLAY_BIN" test -c "java -cp $CP Loop" --delay 3 --health-path /healthz \
        --generateGithubActions=false --disable-tele "$@" >"test-$name.log" 2>&1 </dev/null
    rc=$?
    own
    report=$(find keploy/reports -name 'test-set-0-report.yaml' | sort | tail -1)
    status=$(grep -m1 '^status:' "$report" 2>/dev/null | awk '{print $2}')
    if [ "$rc" -ne 0 ] || [ "$status" != PASSED ]; then
        fail "$CELL: the $name replay must pass (exit $rc, status ${status:-none})"
        grep -nE 'no matching mock|failed to decode|strict' "test-$name.log" | head -5 | cut -c1-300
    fi
    if grep -q 'WARNING: DATA RACE' "test-$name.log"; then fail "$CELL: data race in the $name replay's log"; fi
}

# run <cell> <pom> <record bin> <expect> <requests...>: build the app with
# pom, record the requests with the record bin, check the recording as
# expect says (current: this build's; legacy: a released keploy's), and
# replay it, plain and strict.
run() {
    local pom="$2" bin="$3" expect="$4" code path out
    CELL="$1"
    shift 4
    echo "== $CELL =="
    mkdir -p "$WORK/$CELL" && cd "$WORK/$CELL" || exit 1
    cp -r "$SRC/app" . && (cd app && mvn -q -B -f "$pom" package -DskipTests) || { fail "$CELL: the app did not build"; return; }
    CP="app/target/stmt-loop-1.jar:$(find app/target/lib -name '*.jar' | sort | paste -sd: -)"

    docker_pull_retry "$MYSQL_IMAGE"
    MYSQL_CID=$(docker run -d --init -p 127.0.0.1:3306:3306 -e MYSQL_ROOT_PASSWORD=password -e MYSQL_DATABASE=loop \
        "$MYSQL_IMAGE" --default-authentication-plugin=mysql_native_password) || { fail "$CELL: MySQL did not start"; return; }
    until docker exec "$MYSQL_CID" mysql -h127.0.0.1 -uroot -ppassword -e 'SELECT 1' loop >/dev/null 2>&1; do
        [ "$(docker inspect -f '{{.State.Running}}' "$MYSQL_CID" 2>/dev/null)" = true ] || { fail "$CELL: MySQL exited"; return; }
        sleep 0.5
    done
    # mysql_query_attribute_string() (GET /attrread).
    out=$(docker exec "$MYSQL_CID" mysql -h127.0.0.1 -uroot -ppassword \
        -e 'INSTALL COMPONENT "file://component_query_attributes"' 2>&1) || {
        fail "$CELL: could not install MySQL's query attributes component: $(echo "$out" | grep -v 'Using a password' | head -3)"
        docker rm -f "$MYSQL_CID" >/dev/null 2>&1
        MYSQL_CID=""
        return
    }

    sudo -E env PATH="$PATH" "$bin" record -c "java -cp $CP Loop" --generateGithubActions=false --disable-tele \
        >record.log 2>&1 </dev/null &
    REC_PID=$!
    until curl -fsS http://127.0.0.1:8096/healthz >/dev/null 2>&1; do
        kill -0 "$REC_PID" 2>/dev/null || { fail "$CELL: keploy record exited before the app was up"; tail -30 record.log; return; }
        sleep 0.5
    done
    for path in "$@"; do
        code=$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:8096$path")
        [ "$code" = 200 ] || fail "$CELL: GET $path answered $code while recording"
    done
    # A last /healthz: once its test case is written, every earlier
    # request's is written or was left out.
    curl -fsS http://127.0.0.1:8096/healthz >/dev/null
    until [ "$(tests_of '/healthz$')" -ge 2 ]; do
        kill -0 "$REC_PID" 2>/dev/null || break
        sleep 0.5
    done
    stop_pid "$REC_PID"
    REC_PID=""
    own
    docker rm -f "$MYSQL_CID" >/dev/null 2>&1
    MYSQL_CID=""

    local tests executes=0 attrs=0
    for path in "$@"; do
        case "$path" in
        /attr/* | /attrs/* | /trace/* | /attrread/*) attrs=$((attrs + ${path##*/})) ;;
        *) executes=1 ;;
        esac
    done
    tests=$(tests_of '/(loop|again|strings|small|attr|attrs|trace|attrread|pattr)/[0-9]+$')
    [ "$tests" -eq "$#" ] || fail "$CELL: recorded $tests test cases of the $# requests: the rest were left out"
    if grep -q 'a captured exchange was not recorded as a mock' record.log; then
        fail "$CELL: keploy record left MySQL exchanges out: $(grep -m1 'a captured exchange was not recorded as a mock' record.log | cut -c1-300)"
    fi
    [ "$attrs" -eq 0 ] || check_attrs "$expect" "$attrs"
    [ "$executes" -eq 0 ] || check_executes "$expect"
    echo "$CELL: $tests test cases, $(execute_rows | wc -l) EXECUTEs recorded, $(attr_rows | wc -l) queries with attributes"

    replay plain
    replay strict --mock-noise-strict
    if grep -q 'WARNING: DATA RACE' record.log; then fail "$CELL: data race in the record log"; fi
    cd "$WORK" || exit 1
}

run cj8.4 pom.xml "$RECORD_BIN" current /loop/3 /again/3 /again/4 /attr/3 /attrs/2 /trace/2 /attrread/2 /pattr/3
run cj5.1 pom51.xml "$RECORD_BIN" current /loop/1 /loop/3 /loop/5 /again/3 /strings/3 /small/3
run cj5.1-legacy-recording pom51.xml "$LEGACY_RECORD_BIN" legacy /strings/3 /strings/2 /small/3
run cj8.4-legacy-recording pom.xml "$LEGACY_RECORD_BIN" legacy /trace/3

if [ "$FAIL" -ne 0 ]; then
    echo "$FAIL check(s) failed"
    exit 1
fi
echo "all checks passed"
