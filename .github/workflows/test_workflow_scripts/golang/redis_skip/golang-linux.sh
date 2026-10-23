#!/usr/bin/env bash
# E2E: OSS keploy records and mocks no Redis. A connection whose client opens
# with a Redis command is relayed to the real Redis, in record and in test
# mode, and the agent says so once per Redis destination
# (integrations.RedisSkipNotice), with the Keploy Enterprise URL.
#
# The app (./app) is an HTTP API over two Redis servers (go-redis; it opens
# four connections to each at startup, and pipelines SET, GET and DEL on one
# endpoint), a memcached and an echo server (./echo), TCP dependencies OSS
# keploy records through its Generic parser. They are the nearest look-alikes
# of a Redis client: memcached's text protocol reads like an inline Redis
# command, and the echo server is sent a RESP command array whose command
# (NOPE) is no Redis command. Recorded in order: PUT and GET /kv/a, PUT and
# GET /kv/b, GET /pipeline/p1, PUT and GET /cache/c1 (memcached and Redis A in
# one request), GET /resp/r1 (the echo server), GET /health.
#
#   1. keploy record writes the nine test cases and no Redis mock (this
#      build), and still records memcached and the echo server through
#      Generic: the skip claims no non-Redis connection.
#   2. The notice: exactly once per Redis destination in each keploy process,
#      at WARN, over at least four connections to each, carrying the
#      destination and the URL; never for memcached or the echo server.
#   3. keploy test, with both Redis servers live and memcached and the echo
#      server down, passes every test: Redis is reached for real, every
#      connection the Redis servers count was relayed by keploy, and
#      memcached and the echo server are answered from their Generic mocks.
#   4. CLEAN CUT. A test set recorded by the latest released OSS keploy holds
#      Generic Redis mocks; this build replays it, Redis live, and passes,
#      and the logs prove its Redis mocks were never consulted: the agent
#      said once that it left every one of them out of the pool, the Generic
#      parser's pool, listed at each memcached and echo request, names none
#      of them (and does name those mocks), Generic served the memcached and
#      echo mocks and none of the Redis ones, and every connection the Redis
#      servers counted was relayed. Checked on the recording the
#      record_latest cell makes (while the latest release predates the skip)
#      and, in every cell, on the committed fixture ./fixture (its test set,
#      recorded by keploy v3.6.159, the latest release on 2026-10-22), each
#      replay on a fresh copy of it:
#        - by default;
#        - under the proven switch (its report says proven: true), whose
#          exact serving would fail the set on Redis calls "not made" were its
#          Redis mocks held to a test or to the set's end;
#        - with --remove-unused-mocks, which must keep every Redis mock (the
#          Generic recorder writes them as config mocks, which it keeps).
#      Each replay's copy of the fixture has one Redis mock answer wrong on
#      purpose (fresh_fixture): GET kv:a's (mock-25) answers "omega", where
#      the Redis held "alpha" when the set was recorded and holds it again
#      once the replay's PUT /kv/a has run. A replay that serves Redis from
#      the fixture's mocks (a keploy without the skip: every one before it)
#      then fails GET /kv/a on its verdict, not only on the logs. An agent
#      that keeps the Redis mocks in its pool while the skip relays Redis is
#      caught by the pool listing (pooled) and, under the proven switch, by
#      the calls "not made" the replay holds its tests to.
#   4b. The fixture with the mappings.yaml keploy v3.6.159's own replay of it
#      wrote (./fixture/mappings-v3.6.159.yaml), as a user's older test set
#      holds one: each test listed with the Generic Redis mocks it consumed.
#      It was made by replaying a copy of ./fixture/test-set-0 with the
#      v3.6.159 release binary, every dependency down (that release mocks
#      Redis through Generic), all nine tests passing, and keeping the
#      mappings.yaml it wrote. Replayed by default and under the proven
#      switch, whose ownership is then that mapping's: still no test is held
#      to a Redis mock.
#   4c. The same set as Keploy Enterprise records it: its Redis mocks in
#      Enterprise's kind (kind: Redis, the exchange under RequestBin and
#      ResponseBin), which this build cannot read, the mapping naming them
#      so. Replayed by default and under the proven switch: the read of the
#      set says once, at WARN, how many Redis mocks it does not read, no
#      ERROR is logged for them, and the set passes, Redis relayed.
#   5. No ERROR in a log this build wrote, and no data race in any log.
#
# RECORD_PROVEN=1 records under the proven switch (the
# record_build_proven_replay_build cell): keploy record then writes the set's
# mappings.yaml from its test windows and accounts for every call it could not
# capture; a Redis it relays is no capture loss. Its recording is replayed by
# default and under the proven switch, besides the replays above.
#
# Not record_build_replay_latest: a release that predates the skip mocks
# Redis from Generic mocks at replay, and this build records none.
#
# Not covered here: a Redis mock a set's mappings.yaml names in a concurrency
# group or marks unresolved (Keploy Enterprise's record, KEPLOY_PROVEN), which
# the replay leaves out of the set's concurrency as of its mappings
# (withoutLeftOutConcurrency, unit-tested in pkg/service/replay). The lane
# sends its requests one at a time, and no release that records Redis through
# Generic writes concurrency groups.
#
# The dependencies run in docker compose (./docker-compose.yml). Readiness is
# event-driven: the containers answering, the app's port, the test files
# keploy writes. Ports: 8098 (app), 6379 and 6380 (Redis), 11211 (memcached),
# 7379 (echo). Env: RECORD_BIN, REPLAY_BIN; REDIS_IMAGE (default
# redis:7.4-alpine), MEMCACHED_IMAGE (default memcached:1.6-alpine);
# FIXTURE_OUT, when set, gets the latest release's recording of the test set
# as ./fixture holds it (FIXTURE_OUT/test-set-0: how ./fixture was made; the
# wrong answer of 4 is made on each replay's copy, never in ./fixture).
set -uo pipefail

SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SRC/../../docker-build-retry.sh"
source "$SRC/../../go-retry.sh"
source "$SRC/../../test-iid.sh" || true

: "${RECORD_BIN:?RECORD_BIN: the keploy that records}"
: "${REPLAY_BIN:?REPLAY_BIN: the keploy that replays (this build)}"
REDIS_IMAGE="${REDIS_IMAGE:-redis:7.4-alpine}"
MEMCACHED_IMAGE="${MEMCACHED_IMAGE:-memcached:1.6-alpine}"
APP=8098
REDIS_A=127.0.0.1:6379
REDIS_B=127.0.0.1:6380
CACHE=127.0.0.1:11211
ECHO_PORT=7379
ECHO=127.0.0.1:$ECHO_PORT
URL="https://keploy.io/docs/keploy-cloud/cloud-installation/"
NOTICE="Redis calls are skipped"
SKIP_LINE="relaying a Redis connection to the real server"
LEFT_OUT="keploy leaves this test set's Redis mocks out of its replay"
# UNREAD: what the read of a set says of the Redis mocks (Keploy
# Enterprise's) this build cannot read.
UNREAD="Redis mocks? this keploy does not read"
# ERRORS_ALLOWED: an extended regex of ERROR lines this build may log here;
# none so far.
ERRORS_ALLOWED='^$'

FAIL=0
fail() {
    echo "::error::$*"
    FAIL=$((FAIL + 1))
}
is_build() {
    case "$1" in
        */build/keploy | */build-no-race/keploy) return 0 ;;
    esac
    return 1
}
echo "record: $RECORD_BIN"
echo "replay: $REPLAY_BIN"
if ! is_build "$REPLAY_BIN"; then
    echo "::error::REPLAY_BIN ($REPLAY_BIN) is not this build: a release that predates the Redis skip mocks Redis at replay; this lane replays with the build only"
    exit 1
fi

WORK="${WORK:-$(mktemp -d)}"
# Both are read after the script has changed directory: absolute, then.
WORK=$(realpath -m "$WORK")
[ -z "${FIXTURE_OUT:-}" ] || FIXTURE_OUT=$(realpath -m "$FIXTURE_OUT")
mkdir -p "$WORK"
echo "work dir: $WORK"
(cd "$SRC" && go_retry mod download && go build -o "$WORK/redis-app" ./app && go build -o "$WORK/echo-server" ./echo) || { echo "::error::build failed"; exit 1; }
cd "$WORK" || exit 1
# A runner may reuse WORK: nothing of an earlier run is read as this one's.
sudo rm -rf keploy keploy.yml fixture-* own-*
rm -f ./*.log

listening() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
# pid_running PID: whether PID still runs, by its state in /proc, which shows
# root's processes to this user too: gone, or a zombie, is not running. Not
# kill -0: keploy runs under sudo here, and while sudo checks its policy it
# holds every uid as root, so kill -0 from this user fails (EPERM) on a sudo
# that is alive, and keploy would be taken for ended before it started.
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
wait_tests() {
    until [ "$(test_files)" -ge "$1" ]; do
        pid_running "$2" || return 1
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
plain() { sed -r 's/\x1b\[[0-9;]*m//g' "$1"; }

# --- Dependencies -----------------------------------------------------------
# Two Redis servers and a memcached, in docker compose (./docker-compose.yml),
# published on loopback; the app runs on the host under keploy.
export COMPOSE_FILE="$SRC/docker-compose.yml" COMPOSE_PROJECT_NAME=redisskip REDIS_IMAGE MEMCACHED_IMAGE
REC_PID=""
ECHO_PID=""
stop_echo() {
    [ -n "$ECHO_PID" ] || return 0
    kill "$ECHO_PID" 2>/dev/null
    wait "$ECHO_PID" 2>/dev/null
    ECHO_PID=""
}
cleanup() {
    [ -n "$REC_PID" ] && stop_pid "$REC_PID"
    stop_echo
    docker compose down -v --remove-orphans >/dev/null 2>&1
    own
}
trap cleanup EXIT
docker compose down -v --remove-orphans >/dev/null 2>&1
for p in "$APP" 6379 6380 11211 "$ECHO_PORT"; do
    if listening "$p"; then echo "::error::port $p is in use"; exit 1; fi
done
docker_compose_pull_retry
docker compose up -d || { echo "::error::cannot start the lane's Redis and memcached"; docker compose logs | tail -40; exit 1; }
running() { [ -n "$(docker compose ps --status running -q "$1" 2>/dev/null)" ]; }
for svc in redis-a redis-b; do
    until [ "$(docker compose exec -T "$svc" redis-cli ping 2>/dev/null | tr -d '\r')" = PONG ]; do
        running "$svc" || { echo "::error::$svc exited before it was ready"; docker compose logs "$svc" | tail -20; exit 1; }
        sleep 0.5
    done
done
memcached_version() {
    (exec 3<>/dev/tcp/127.0.0.1/11211 && printf 'version\r\n' >&3 && head -n1 <&3) 2>/dev/null
}
until memcached_version | grep -q '^VERSION'; do
    running memcached || { echo "::error::memcached exited before it was ready"; docker compose logs memcached | tail -20; exit 1; }
    sleep 0.5
done
# The echo server runs on the host, outside keploy, until the replays.
"$WORK/echo-server" "$ECHO" >echo-server.log 2>&1 </dev/null &
ECHO_PID=$!
wait_listen "$ECHO_PORT" "$ECHO_PID" || { echo "::error::the echo server did not start"; cat echo-server.log; exit 1; }
echo "Redis A, Redis B, memcached and the echo server are up"
# conns <service>: the connections the Redis has accepted so far, the one
# redis-cli opens to ask among them (INFO counts it).
conns() { docker compose exec -T "$1" redis-cli INFO stats | tr -d '\r' | awk -F: '$1 == "total_connections_received" { print $2 }'; }
# since <before> <after>: the connections a Redis accepted between two conns
# readings, less the later reading's own.
since() { echo $(($2 - $1 - 1)); }

# --- Log and mock readers ---------------------------------------------------
# notices <log> <destination>: the notice lines naming the destination.
notices() { plain "$1" | grep -F "$NOTICE" | grep -cE "\"destination\": ?\"${2//./\\.}\""; }
# check_notices <step> <log>: the notice is said exactly once for each Redis
# destination, at WARN, with the URL, and for nothing else.
check_notices() {
    local all
    all=$(plain "$2" | grep -cF "$NOTICE")
    for dst in "$REDIS_A" "$REDIS_B"; do
        [ "$(notices "$2" "$dst")" -eq 1 ] || fail "$1: the notice was said $(notices "$2" "$dst") times for $dst, want once"
    done
    [ "$all" -eq 2 ] || fail "$1: the notice was said $all times, want twice (once per Redis destination)"
    [ "$(plain "$2" | grep -F "$NOTICE" | grep -cF "$URL")" -eq "$all" ] || fail "$1: a notice does not carry $URL"
    [ "$(plain "$2" | grep -F "$NOTICE" | grep -cwF WARN)" -eq "$all" ] || fail "$1: a notice is not logged at WARN"
    [ "$(notices "$2" "$CACHE")" -eq 0 ] || fail "$1: the notice was said for memcached ($CACHE): the skip claimed a non-Redis connection"
    [ "$(notices "$2" "$ECHO")" -eq 0 ] || fail "$1: the notice was said for the echo server ($ECHO): the skip claimed a RESP array that names no Redis command"
    plain "$2" | grep -F "$NOTICE" | head -2
}
# no_errors <step> <log>: this build logged no ERROR (but ERRORS_ALLOWED).
no_errors() {
    local errs
    errs=$(plain "$2" | grep -wE 'ERROR' | grep -vE "$ERRORS_ALLOWED")
    if [ -n "$errs" ]; then
        fail "$1: keploy logged $(wc -l <<<"$errs") ERROR line(s)"
        head -10 <<<"$errs"
    fi
}
# classify <mocks.yaml>: one line per Generic mock: its name and what its first
# request is: redis (a RESP command array naming a command the app's go-redis
# sends), lookalike (the RESP array the app sends the echo server), memcached
# (a text get or set) or other.
classify() {
    yq -o=json -I=0 'select(.kind == "Generic") | {"name": .name, "type": .spec.genericrequests[0].message[0].type, "data": .spec.genericrequests[0].message[0].data}' "$1" |
        python3 -c '
import base64, json, re, sys
REDIS = {"HELLO", "CLIENT", "AUTH", "SELECT", "PING", "INFO", "GET", "SET", "DEL", "QUIT"}
for line in sys.stdin:
    line = line.strip()
    if not line or line == "null":
        continue
    m = json.loads(line)
    data = m.get("data") or ""
    if m.get("type") == "binary":
        data = base64.b64decode(data).decode("latin-1")
    resp = re.match(r"\*[1-9][0-9]*\r\n\$[1-9][0-9]*\r\n([A-Za-z][A-Za-z0-9._-]*)\r\n", data)
    if resp and resp.group(1).upper() in REDIS:
        kind = "redis"
    elif resp and resp.group(1) == "NOPE":
        kind = "lookalike"
    elif resp:
        kind = "other"
    elif re.match(r"(get|set) ", data):
        kind = "memcached"
    else:
        kind = "other"
    print(m["name"] + "\t" + kind)
'
}
count_kind() { classify "$1" | awk -F'\t' -v k="$2" '$2 == k' | wc -l; }
names_of() { classify "$1" | awk -F'\t' -v k="$2" '$2 == k { print $1 }'; }
# served <log>: the names of the mocks the Generic parser served.
served() { plain "$1" | grep -E 'mock found for generic request' | grep -oE '"Mock": ?"[^"]+"' | sed -E 's/.*"([^"]+)"$/\1/' | sort -u; }
# pooled <log>: the names of the mocks the Generic parser's pool held, as it
# lists them (--debug) at each request it is asked to answer.
pooled() { plain "$1" | grep -E '(Filtered|Unfiltered) Mocks' | grep -oE '"Mock\[[0-9]+\]": ?"[^"]+"' | sed -E 's/.*"([^"]+)"$/\1/' | sort -u; }

# check_clean_cut <step> <mocks.yaml> <log> [unsent]: the replay served none
# of the recording's Redis mocks, nor held one in the Generic parser's pool;
# it held and served its memcached and echo mocks (the reading works); every
# Redis connection was relayed; and the agent said once that it left every
# Redis mock out, or, with unsent (a set whose Redis mocks keploy test could
# not read, and so never sent the agent), said nothing of leaving any out.
# RELAYED_WANT is the connections the Redis servers counted (replay).
check_clean_cut() {
    local step=$1 mocks=$2 log=$3 unsent=${4:-} servedNames pooledNames redisNames n
    servedNames=$(served "$log")
    pooledNames=$(pooled "$log")
    redisNames=$(names_of "$mocks" redis)
    for n in $redisNames; do
        if grep -qxF "$n" <<<"$servedNames"; then
            fail "$step: the Generic parser served $n, a Redis mock of the recording"
        fi
        if grep -qxF "$n" <<<"$pooledNames"; then
            fail "$step: the Generic parser's pool held $n, a Redis mock of the recording the agent must leave out"
        fi
    done
    local kind want_served got_served got_pooled
    for kind in memcached lookalike; do
        want_served=2
        [ "$kind" = lookalike ] && want_served=1
        got_served=0 got_pooled=0
        for n in $(names_of "$mocks" "$kind"); do
            grep -qxF "$n" <<<"$servedNames" && got_served=$((got_served + 1))
            grep -qxF "$n" <<<"$pooledNames" && got_pooled=$((got_pooled + 1))
        done
        [ "$got_served" -ge "$want_served" ] || fail "$step: the Generic parser served $got_served $kind mock(s), want $want_served: the log does not show what Generic served (was --debug passed?)"
        [ "$got_pooled" -ge "$want_served" ] || fail "$step: the Generic parser's pool listed $got_pooled $kind mock(s), want $want_served: the log does not show Generic's pool, so it cannot show a Redis mock is not in it"
    done
    local relayed
    relayed=$(plain "$log" | grep -cF "$SKIP_LINE")
    [ "$relayed" -ge 8 ] || fail "$step: $relayed Redis connections were relayed, want at least the app's eight startup connections"
    [ "$relayed" -eq "$RELAYED_WANT" ] || fail "$step: keploy relayed $relayed Redis connection(s), but the Redis servers counted $RELAYED_WANT: a connection reached Redis without the skip, or the skip relayed one that never reached it"
    # The agent left the recording's Redis mocks out of the pool as the set's
    # mocks were stored, and said so once, with how many: all of them, or it
    # says nothing of a recording that has none.
    local want lines left
    want=$(wc -w <<<"$redisNames")
    [ -n "$unsent" ] && want=0
    lines=$(plain "$log" | grep -cF "$LEFT_OUT")
    left=$(plain "$log" | grep -F "$LEFT_OUT" | grep -oE '"mocks": ?[0-9]+' | grep -oE '[0-9]+$' | head -n1)
    if [ "$want" -gt 0 ]; then
        [ "$lines" -eq 1 ] || fail "$step: the agent said $lines times that it left the set's Redis mocks out, want once"
        [ "$left" = "$want" ] || fail "$step: the agent said it left out ${left:-no} Redis mock(s) of the set, want all $want of the recording's"
    elif [ "$lines" -ne 0 ]; then
        fail "$step: the agent said it left out Redis mocks of a recording that has none"
    fi
    echo "$step: $(wc -w <<<"$redisNames") Redis mock(s) in the recording, none pooled or served, ${left:-none} left out by the agent; $relayed Redis connection(s) relayed of $RELAYED_WANT counted"
}

# replay <name> <dir> [keploy test flags...]: keploy test of the recording in
# dir, both Redis live, memcached and the echo server down, with --debug (the
# Generic parser's and the skip's per connection lines), under the proven
# switch when REPLAY_PROVEN=1. RC is its exit code, RELAYED_WANT the
# connections the Redis servers counted meanwhile; the report it wrote is
# checked to pass all the tests.
replay() {
    local name=$1 dir=$2 report a0 b0 da db before
    shift 2
    a0=$(conns redis-a)
    b0=$(conns redis-b)
    before=$(ls -1d "$dir"/keploy/reports/test-run-* 2>/dev/null | sort -V | tail -n1)
    local proven=(-u KEPLOY_PROVEN)
    [ "${REPLAY_PROVEN:-}" = 1 ] && proven=(KEPLOY_PROVEN=1)
    (cd "$dir" && sudo -E env "${proven[@]}" PATH="$PATH" "$REPLAY_BIN" test -c "$WORK/redis-app" --delay 0 --health-path /health \
        --debug --generateGithubActions=false --disable-tele "$@") >"$WORK/$name.log" 2>&1 </dev/null
    RC=$?
    own
    # The real Redis servers were reached: at least the app's four startup
    # connections to each. Had any Redis mock answered instead, the
    # connection it answered would never have reached a server.
    da=$(since "$a0" "$(conns redis-a)")
    db=$(since "$b0" "$(conns redis-b)")
    RELAYED_WANT=$((da + db))
    echo "$name: the replay reached Redis A over $da and Redis B over $db connection(s)"
    [ "$da" -ge 4 ] && [ "$db" -ge 4 ] || fail "$name: the replay reached Redis A over $da and Redis B over $db connection(s), want at least four each: Redis was not relayed to the real servers"
    no_errors "$name" "$WORK/$name.log"
    report=$(ls -1d "$dir"/keploy/reports/test-run-*/test-set-0-report.yaml 2>/dev/null | sort -V | tail -n1)
    if [ -z "$report" ] || [ "$(dirname "$report")" = "$before" ]; then
        fail "$name: keploy test wrote no report of its own (exit $RC)"
        tail -40 "$WORK/$name.log"
        return 1
    fi
    local status total passed
    status=$(yq -N '.status' "$report")
    total=$(yq -N '.total' "$report")
    passed=$(yq -N '.success' "$report")
    echo "$name: exit $RC, status $status, $passed/$total passed"
    if [ "$RC" -ne 0 ] || [ "$status" != PASSED ] || [ "$total" != "$TESTS" ]; then
        fail "$name: keploy test must pass all $TESTS tests with Redis live (exit $RC, status $status, $passed/$total)"
        grep -nE '^status:|^    status:|test_case_id' "$report" | head -40
        plain "$WORK/$name.log" | grep -nE 'ERROR|no matching|mismatch' | head -20
    fi
}

# fresh_fixture <dir>: dir holds, as keploy reads a recording, a copy of the
# committed fixture's test set and nothing else.
fresh_fixture() {
    sudo rm -rf "$1" && mkdir -p "$1/keploy" && cp -r "$SRC/fixture/test-set-0" "$1/keploy/" || return 1
    # The wrong answer of 4: GET kv:a's mock answers omega (the set holds
    # one GET kv:a, so it is found by its request, whatever its name).
    local m="$1/keploy/test-set-0/mocks.yaml"
    GETA=$'*2\r\n$3\r\nget\r\n$4\r\nkv:a\r\n' OMEGA=$'$5\r\nomega\r\n' yq -i \
        '(select(.kind == "Generic" and .spec.genericrequests[0].message[0].data == strenv(GETA)) | .spec.genericresponses[0].message[0].data) = strenv(OMEGA)' "$m" || return 1
    if [ "$(GETA=$'*2\r\n$3\r\nget\r\n$4\r\nkv:a\r\n' OMEGA=$'$5\r\nomega\r\n' yq -N 'select(.kind == "Generic" and .spec.genericrequests[0].message[0].data == strenv(GETA) and .spec.genericresponses[0].message[0].data == strenv(OMEGA)) | .name' "$m" | grep -c .)" -ne 1 ]; then
        echo "::error::the fixture's copy in $1 does not hold one GET kv:a mock answering omega: the fixture changed, and its replays would no longer fail on a served Redis mock"
        return 1
    fi
}
# fresh_fixture_mapped <dir>: the fixture with the mappings.yaml keploy
# v3.6.159's replay of it wrote (4b).
fresh_fixture_mapped() {
    fresh_fixture "$1" && cp "$SRC/fixture/mappings-v3.6.159.yaml" "$1/keploy/test-set-0/mappings.yaml"
}
# fresh_fixture_enterprise <dir>: the mapped fixture as Keploy Enterprise
# records it (4c): each Generic Redis mock in Enterprise's kind and shape
# (kind: Redis; RequestBin, ResponseBin, reqTimestampMock, resTimestampMock),
# its mapping entries of that kind. ENTERPRISE_REDIS is then how many.
fresh_fixture_enterprise() {
    local names
    fresh_fixture_mapped "$1" || return 1
    names=$(names_of "$1/keploy/test-set-0/mocks.yaml" redis | paste -sd'|')
    [ -n "$names" ] || return 1
    ENTERPRISE_REDIS=$(names_of "$1/keploy/test-set-0/mocks.yaml" redis | wc -l)
    NAMES="^($names)\$" yq -i '(select(.kind == "Generic" and (.name | test(strenv(NAMES))))) |=
        (.kind = "Redis" | .spec.RequestBin = .spec.genericrequests | .spec.ResponseBin = .spec.genericresponses |
         .spec.reqTimestampMock = .spec.reqtimestampmock | .spec.resTimestampMock = .spec.restimestampmock |
         del(.spec.genericrequests) | del(.spec.genericresponses) | del(.spec.reqtimestampmock) | del(.spec.restimestampmock))' \
        "$1/keploy/test-set-0/mocks.yaml" || return 1
    NAMES="^($names)\$" yq -i '((.tests[].mock_entries[], .startup[]) | select(.name | test(strenv(NAMES))) | .kind) = "Redis"' \
        "$1/keploy/test-set-0/mappings.yaml" || return 1
    [ "$(yq -N 'select(.kind == "Redis") | .name' "$1/keploy/test-set-0/mocks.yaml" | grep -c .)" -eq "$ENTERPRISE_REDIS" ]
}
# check_unread <step> <log> <n>: the read of the set said once, at WARN,
# that it holds n Redis mocks this build does not read, with the URL (that
# the agent, never sent them, said nothing of leaving any out is
# check_clean_cut's, unsent).
check_unread() {
    local said
    said=$(plain "$2" | grep -E "$UNREAD")
    [ "$(grep -c . <<<"$said")" -eq 1 ] || fail "$1: the read of the set said $(grep -c . <<<"$said") times that it does not read Redis mocks, want once"
    grep -qE "holds $3 Redis mocks? this keploy does not read" <<<"$said" || fail "$1: the read of the set did not say it holds the $3 Redis mocks it does not read: $said"
    grep -qwF WARN <<<"$said" || fail "$1: what the read of the set said of its Redis mocks is not at WARN"
    grep -qF "$URL" <<<"$said" || fail "$1: what the read of the set said of its Redis mocks does not carry $URL"
    echo "$1: the read of the set said once that it does not read its $3 Redis mocks"
}

# --- 1, 2. Record -----------------------------------------------------------
# send <method> <path> <want status> [body]: one request while recording. One
# that fails stops the lane at once: its test case would never be written,
# and the wait for the test files would never end.
send() {
    local code args=(-s -o "$WORK/last-body.txt" -w '%{http_code}' -X "$1" "http://127.0.0.1:$APP$2")
    [ -n "${4:-}" ] && args+=(-d "$4")
    code=$(curl "${args[@]}") || code="no answer (curl exit $?)"
    if [ "$code" != "$3" ]; then
        echo "::error::$1 $2 answered $code while recording, want $3"
        cat "$WORK/last-body.txt" 2>/dev/null
        plain record.log | tail -40
        exit 1
    fi
}
TESTS=9
before_a=$(conns redis-a)
before_b=$(conns redis-b)
sudo rm -rf keploy keploy.yml
record_proven=(-u KEPLOY_PROVEN)
[ "${RECORD_PROVEN:-}" = 1 ] && record_proven=(KEPLOY_PROVEN=1)
sudo -E env "${record_proven[@]}" PATH="$PATH" "$RECORD_BIN" record -c "$WORK/redis-app" \
    --generateGithubActions=false --disable-tele >record.log 2>&1 </dev/null &
REC_PID=$!
wait_listen "$APP" "$REC_PID" || { echo "::error::the app did not start under keploy record"; tail -40 record.log; exit 1; }
send PUT /kv/a 200 alpha
send GET /kv/a 200
send PUT /kv/b 200 beta
send GET /kv/b 200
send GET /pipeline/p1 200
send PUT /cache/c1 200 hello
send GET /cache/c1 200
send GET /resp/r1 200
send GET /health 200
wait_tests "$TESTS" "$REC_PID" || fail "keploy record wrote $(test_files) test cases, want $TESTS"
stop_pid "$REC_PID"
REC_PID=""
own
plain record.log | tail -30
[ "$(test_files)" -eq "$TESTS" ] || fail "keploy record wrote $(test_files) test cases, want $TESTS"
MOCKS=keploy/test-set-0/mocks.yaml
[ -s "$MOCKS" ] || { fail "keploy record wrote no mocks"; MOCKS=/dev/null; }
redis_mocks=$(count_kind "$MOCKS" redis)
mem_mocks=$(count_kind "$MOCKS" memcached)
look_mocks=$(count_kind "$MOCKS" lookalike)
echo "recorded $redis_mocks Redis, $mem_mocks memcached and $look_mocks look-alike Generic mock(s)"
[ "$mem_mocks" -ge 2 ] || fail "keploy record wrote $mem_mocks memcached mock(s), want the set and the get: Generic must still record a non-Redis TCP dependency"
[ "$look_mocks" -ge 1 ] || fail "keploy record wrote $look_mocks mock(s) of the echo server, want its NOPE: the Redis skip took a RESP array that names no Redis command, and nothing of it was recorded"
conn_a=$(since "$before_a" "$(conns redis-a)")
conn_b=$(since "$before_b" "$(conns redis-b)")
echo "the app opened $conn_a connection(s) to Redis A and $conn_b to Redis B while recording"
[ "$conn_a" -ge 4 ] && [ "$conn_b" -ge 4 ] || fail "the app reached Redis A over $conn_a and Redis B over $conn_b connection(s), want at least four each"
if is_build "$RECORD_BIN"; then
    # 1. No Redis mock; 2. the notice.
    [ "$redis_mocks" -eq 0 ] || fail "this build recorded $redis_mocks Redis mock(s): $(names_of "$MOCKS" redis | tr '\n' ' ')"
    check_notices "record" record.log
    no_errors "record" record.log
    if [ "${RECORD_PROVEN:-}" = 1 ]; then
        # Under the proven switch keploy record wrote the set's mappings.yaml
        # from its test windows, owning the memcached and echo mocks, and
        # said no call went uncaptured: a Redis it relays is no loss.
        MAP=keploy/test-set-0/mappings.yaml
        if [ ! -s "$MAP" ]; then
            fail "proven record: keploy record wrote no mappings.yaml"
        else
            mapped=0
            owned=$(yq -N '.tests[].mock_entries[].name' "$MAP")
            for n in $(names_of "$MOCKS" memcached) $(names_of "$MOCKS" lookalike); do
                grep -qxF "$n" <<<"$owned" && mapped=$((mapped + 1))
            done
            [ "$mapped" -ge 3 ] || fail "proven record: mappings.yaml names $mapped of the memcached and echo mocks, want the three its tests made"
        fi
        # What keploy record says of calls it could not capture (a loss that
        # fails the recording, test cases recorded without a verdict, or
        # calls it passed through as a server-first protocol): a Redis the
        # skip relays must be none of them. Not the kernel's skipped capture
        # runs, evidence that fails nothing and is no Redis matter.
        if plain record.log | grep -E "the recording is (incomplete|not verified)|these test cases' calls may be missing from the mocks|connections keploy passes through"; then
            fail "proven record: keploy record said a call went uncaptured: a Redis the skip relays is reached for real at replay too, and is no capture loss"
        fi
        echo "proven record: mappings.yaml written, no capture loss said"
    fi
elif [ "$(plain record.log | grep -cF "$NOTICE")" -gt 0 ]; then
    # The latest release has the skip already: its recording has no Redis
    # mocks to ignore, and the committed fixture alone checks the clean cut.
    [ "$redis_mocks" -eq 0 ] || fail "a release that skips Redis recorded $redis_mocks Redis mock(s)"
    echo "the latest release skips Redis already; the clean cut is checked on ./fixture"
else
    # The replay below checks the clean cut on it.
    [ "$redis_mocks" -gt 0 ] || fail "the latest release recorded no Redis mock through Generic: the clean-cut check on its recording would prove nothing"
fi
if [ -n "${FIXTURE_OUT:-}" ]; then
    # The fixture is a release's recording that mocks Redis through Generic.
    if is_build "$RECORD_BIN" || [ "$redis_mocks" -eq 0 ]; then
        fail "FIXTURE_OUT: the recording is not a release's with Generic Redis mocks (record: $RECORD_BIN, $redis_mocks Redis mocks), so it is no fixture"
    else
        rm -rf "$FIXTURE_OUT/test-set-0" && mkdir -p "$FIXTURE_OUT" && cp -r keploy/test-set-0 "$FIXTURE_OUT/" &&
            echo "the test set copied to $FIXTURE_OUT/test-set-0" ||
            fail "FIXTURE_OUT: cannot copy the test set"
    fi
fi

# --- 3, 4. Replay, Redis live, memcached and the echo server down -----------
docker compose stop memcached >/dev/null 2>&1 || fail "cannot stop memcached for the replay"
stop_echo
listening "$ECHO_PORT" && fail "the echo server still listens: the replay could reach it for real"
# The own recording's replays read a copy of it: a replay writes the set's
# mappings.yaml when it has none, and the next would read that instead.
own_copy() { sudo rm -rf "$1" && mkdir -p "$1" && cp -r "$WORK/keploy" "$1/"; }
own_copy "$WORK/own-default" || fail "cannot copy the recording"
replay replay "$WORK/own-default"
check_notices "replay" replay.log
check_clean_cut "replay" "$MOCKS" replay.log
if [ "${RECORD_PROVEN:-}" = 1 ]; then
    # The proven record's set, its mappings.yaml its test windows', under
    # the proven switch: each test owns what that mapping says it does.
    own_copy "$WORK/own-proven" || fail "cannot copy the recording"
    REPLAY_PROVEN=1 replay replay-proven "$WORK/own-proven"
    check_notices "proven replay" replay-proven.log
    check_clean_cut "proven replay" "$MOCKS" replay-proven.log
fi

# 4. The committed fixture, a test set a release that mocked Redis through
# Generic recorded. A run that makes the fixture (FIXTURE_OUT) has none to
# replay yet. Each replay reads a fresh copy: a replay writes the set's
# mappings.yaml when it has none, and the next would read that instead of
# the recording's.
if [ -z "${FIXTURE_OUT:-}" ]; then
    FIX_MOCKS="$SRC/fixture/test-set-0/mocks.yaml"
    fixture_redis=$(count_kind "$FIX_MOCKS" redis)
    [ "$fixture_redis" -gt 0 ] || fail "the fixture holds no Generic Redis mock: it proves nothing"
    if [ -f "$SRC/fixture/test-set-0/mappings.yaml" ]; then
        echo "the fixture carries the recording's mappings.yaml: each test is held to what it recorded"
    else
        echo "the fixture carries no mappings.yaml: the proven replay derives each test's mocks from its window"
    fi

    fresh_fixture "$WORK/fixture-default" || fail "cannot copy the fixture"
    replay replay-fixture "$WORK/fixture-default"
    check_notices "replay of the fixture" replay-fixture.log
    check_clean_cut "replay of the fixture" "$FIX_MOCKS" replay-fixture.log

    # Under the proven switch (KEPLOY_PROVEN=1), whose exact serving
    # holds each test to the calls it recorded, and the set's end to its
    # worker scope (the calls the application made outside every test, its
    # boot's). The Redis mocks are no test's and no worker scope's (the call
    # each recorded is made for real), so the set passes; held to them, it
    # would fail on calls "not made".
    fresh_fixture "$WORK/fixture-proven" || fail "cannot copy the fixture"
    REPLAY_PROVEN=1 replay replay-fixture-proven "$WORK/fixture-proven"
    proven_report=$(ls -1d "$WORK"/fixture-proven/keploy/reports/test-run-*/test-set-0-report.yaml 2>/dev/null | sort -V | tail -n1)
    [ -n "$proven_report" ] && [ "$(yq -N '.proven' "$proven_report")" = true ] ||
        fail "proven replay of the fixture: its report does not say it ran under the proven switch (proven: true), so this replay proves nothing the default one did not"
    check_notices "proven replay of the fixture" replay-fixture-proven.log
    check_clean_cut "proven replay of the fixture" "$FIX_MOCKS" replay-fixture-proven.log

    # With --remove-unused-mocks, which deletes what no test consumed, and
    # keeps the Redis mocks, which went unconsumed because their calls were
    # made for real: the Generic recorder writes every mock as a config one,
    # which the prune keeps, and a keploy with a Redis parser replays them
    # (the docs say so: docs/explanation/mock-lifetimes.md).
    fresh_fixture "$WORK/fixture-prune" || fail "cannot copy the fixture"
    replay replay-fixture-prune "$WORK/fixture-prune" --remove-unused-mocks
    plain replay-fixture-prune.log | grep -qF "pruned mocks successfully" || fail "replay with --remove-unused-mocks: keploy did not prune the set (no 'pruned mocks successfully'), so the check below proves nothing"
    kept_redis=$(count_kind "$WORK/fixture-prune/keploy/test-set-0/mocks.yaml" redis)
    [ "$kept_redis" -eq "$fixture_redis" ] || fail "replay with --remove-unused-mocks kept $kept_redis of the fixture's $fixture_redis Redis mocks: it deleted recordings this build only left out"
    echo "--remove-unused-mocks kept $kept_redis of $fixture_redis Redis mocks"

    # 4b. With the mappings.yaml keploy v3.6.159's replay of it wrote: each
    # test listed with the Redis mocks it consumed then.
    fixture_owned=$(yq -N '.tests[].mock_entries[].name' "$SRC/fixture/mappings-v3.6.159.yaml")
    mapped_redis=$(for n in $(names_of "$FIX_MOCKS" redis); do grep -cxF "$n" <<<"$fixture_owned"; done | awk '{ s += $1 } END { print s + 0 }')
    [ "$mapped_redis" -gt 0 ] || fail "the fixture's v3.6.159 mappings.yaml names no Redis mock: replaying it proves nothing the plain fixture does not"
    echo "the fixture's v3.6.159 mappings.yaml names Redis mocks $mapped_redis time(s)"
    fresh_fixture_mapped "$WORK/fixture-mapped" || fail "cannot copy the mapped fixture"
    replay replay-fixture-mapped "$WORK/fixture-mapped"
    check_notices "replay of the mapped fixture" replay-fixture-mapped.log
    check_clean_cut "replay of the mapped fixture" "$FIX_MOCKS" replay-fixture-mapped.log
    fresh_fixture_mapped "$WORK/fixture-mapped-proven" || fail "cannot copy the mapped fixture"
    REPLAY_PROVEN=1 replay replay-fixture-mapped-proven "$WORK/fixture-mapped-proven"
    check_notices "proven replay of the mapped fixture" replay-fixture-mapped-proven.log
    check_clean_cut "proven replay of the mapped fixture" "$FIX_MOCKS" replay-fixture-mapped-proven.log

    # 4c. As Keploy Enterprise records it: Redis mocks this build cannot read.
    for mode in default proven; do
        dir="$WORK/fixture-enterprise-$mode"
        if ! fresh_fixture_enterprise "$dir"; then
            fail "cannot make the Enterprise-shaped fixture"
            continue
        fi
        name=replay-fixture-enterprise-$mode
        if [ "$mode" = proven ]; then
            REPLAY_PROVEN=1 replay "$name" "$dir"
        else
            replay "$name" "$dir"
        fi
        check_notices "$mode replay of the Enterprise fixture" "$name.log"
        check_unread "$mode replay of the Enterprise fixture" "$name.log" "$ENTERPRISE_REDIS"
        # The Generic Redis mocks the set was made from are checked as
        # never pooled or served; the agent, never sent one, says nothing.
        check_clean_cut "$mode replay of the Enterprise fixture" "$FIX_MOCKS" "$name.log" unsent
    done
fi

# --- 5. --------------------------------------------------------------------
if grep -l 'WARNING: DATA RACE' ./*.log; then fail "data race in a keploy log"; fi

if [ "$FAIL" -ne 0 ]; then
    echo "$FAIL check(s) failed"
    exit 1
fi
echo "all checks passed"
