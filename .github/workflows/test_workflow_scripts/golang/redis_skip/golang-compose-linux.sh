#!/usr/bin/env bash
# E2E, the redis_skip job's docker compose cell: OSS keploy relays Redis to
# the real server when the app runs in docker compose too, and at replay it
# reaches each Redis at the address its compose service name has then.
#
# The app (./app) runs in a container (compose/app.yml, the file keploy record
# and keploy test run) and reaches Redis A and Redis B by their compose
# service names, redis-a and redis-b (compose/deps.yml), on the lane's
# network. keploy's agent runs in a container of the app's project, the app
# in the agent's network namespace. Recorded in order: GET /health, PUT and
# GET /kv/a, PUT and GET /kv/b, GET /pipeline/p1 (SET, GET and DEL in one
# go-redis pipeline).
#
#   1. keploy record writes the six test cases and no Redis mock (nor any
#      Generic one: Redis is the app's only dependency), and the DNS mocks of
#      the app's lookups of redis-a and redis-b, which hold the addresses the
#      two had then. The notice is said once per Redis destination, at WARN,
#      with the URL.
#   2. Redis A and Redis B are recreated at other addresses. No container has
#      the addresses they had at record.
#   3. keploy test passes every test. Its DNS answers the app's lookups from
#      the recording, so the app dials the addresses the recording holds; the
#      Redis skip's relay dials the address each name has now
#      (pkg/agent/proxy/dns_live.go, liveAddr), and says so for every
#      connection it relays. Redis is relayed live, not mocked: the
#      recreated Redis servers count as many connections as keploy relayed,
#      at least the app's four startup connections to each.
#   4. No ERROR in a log, no data race in the CLI's (the agent runs from the
#      image, built without the race detector, as CI's other docker lanes
#      run it).
#   5. keploy mock, under docker compose: the client (./client, in a
#      container: compose/client.yml) is a test command that SETs and GETs on
#      Redis A and gets from memcached, by their compose service names.
#      keploy mock record, before Redis is recreated (step 2), with Redis and
#      memcached live, records memcached through Generic and no Redis mock,
#      and the DNS answers of both names. keploy mock replay, after it, with
#      memcached stopped and Redis A at its new address, passes on the
#      memcached mock. The account its agent leaves as compose stops it
#      carries what it relayed live, and the receipt
#      (keploy/<set>/last-replay.yaml) says the run is NOT isolated: Redis
#      relayed to the live server, named redis-a:6379 (the name keploy's
#      dial went by, its address now), with as many connections as Redis A
#      counted; and a DNS lookup of a name the recording never looked up
#      (the client looks up redis-b, CLIENT_LOOKUP), answered by the live
#      resolver. The terminal gives Keploy Enterprise's URL as the next step
#      and never says every dependency was answered from the recording.
#
# A liveAddr that dials the recorded address fails 3: no container has that
# address any more, so the app cannot reach its Redis, and exits.
#
# This build records and replays: its agent runs from the image
# ghcr.io/keploy/keploy:v3-dev, which the job's compose cell builds from it.
# Readiness is event-driven: Redis answering, the app and keploy's agent
# saying they listen, the test files keploy writes. Env: RECORD_BIN, REPLAY_BIN;
# REDIS_IMAGE (default redis:7.4-alpine); LANE_SUBNET (default
# 10.231.7.0/24), the lane's network, on which Redis A and Redis B have the
# addresses .11 and .12 at record, and .21 and .22 at replay, and memcached
# .13; MEMCACHED_IMAGE (default memcached:1.6-alpine).
set -uo pipefail

SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SRC/../../docker-build-retry.sh"
source "$SRC/../../go-retry.sh"
source "$SRC/../../test-iid.sh" || true

: "${RECORD_BIN:?RECORD_BIN: the keploy that records (this build)}"
: "${REPLAY_BIN:?REPLAY_BIN: the keploy that replays (this build)}"
REDIS_IMAGE="${REDIS_IMAGE:-redis:7.4-alpine}"
MEMCACHED_IMAGE="${MEMCACHED_IMAGE:-memcached:1.6-alpine}"
LANE_SUBNET="${LANE_SUBNET:-10.231.7.0/24}"
NET=redisskip-lane
AGENT_IMAGE=ghcr.io/keploy/keploy:v3-dev
APP_IMAGE=redisskip-app:lane
APP_CONTAINER=redisskip-app
CLIENT_IMAGE=redisskip-client:lane
CLIENT_CONTAINER=redisskip-client
MOCK_SET=redis-mock
APP=8098
TESTS=6
URL="https://keploy.io/docs/keploy-cloud/cloud-installation/"
NOTICE="Redis calls are skipped"
SKIP_LINE="relaying a Redis connection to the real server"
LIVE_LINE="dialling the name's address now"
# ERRORS_ALLOWED: an extended regex of ERROR lines this build may log here;
# none so far.
ERRORS_ALLOWED='^$'

case "$LANE_SUBNET" in
    *.0/24) net_prefix=${LANE_SUBNET%.0/24} ;;
    *) echo "::error::LANE_SUBNET ($LANE_SUBNET) must be a /24 that ends in .0"; exit 1 ;;
esac
REC_A=$net_prefix.11 REC_B=$net_prefix.12
REP_A=$net_prefix.21 REP_B=$net_prefix.22
MEM_IP=$net_prefix.13

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
for bin in "$RECORD_BIN" "$REPLAY_BIN"; do
    if ! is_build "$bin"; then
        echo "::error::$bin is not this build: the compose cell records and replays with this build, whose agent image it runs"
        exit 1
    fi
done
if ! docker image inspect "$AGENT_IMAGE" >/dev/null 2>&1; then
    echo "::error::no $AGENT_IMAGE: build keploy's agent image from this build first (the job's compose cell does)"
    exit 1
fi

WORK="${WORK:-$(mktemp -d)}"
WORK=$(realpath -m "$WORK")
mkdir -p "$WORK"
echo "work dir: $WORK"
# The app, static, in an image of its own (compose/Dockerfile: FROM scratch).
rm -rf "$WORK/app-image" && mkdir -p "$WORK/app-image" || exit 1
(cd "$SRC" && go_retry mod download && CGO_ENABLED=0 go build -o "$WORK/app-image/redis-app" ./app) || { echo "::error::build failed"; exit 1; }
cp "$SRC/compose/Dockerfile" "$WORK/app-image/" || exit 1
docker_build_retry docker build -q -t "$APP_IMAGE" "$WORK/app-image" || exit 1
# keploy mock's test command, static, in an image of its own.
rm -rf "$WORK/client-image" && mkdir -p "$WORK/client-image" || exit 1
(cd "$SRC" && CGO_ENABLED=0 go build -o "$WORK/client-image/redis-client" ./client) || { echo "::error::build failed"; exit 1; }
docker_build_retry docker build -q -t "$CLIENT_IMAGE" -f "$SRC/compose/client.Dockerfile" "$WORK/client-image" || exit 1
# The compose files are read from WORK: keploy writes the app's, as it runs
# it, beside it.
rm -rf "$WORK/compose" && mkdir -p "$WORK/compose" && cp "$SRC/compose/app.yml" "$SRC/compose/deps.yml" "$SRC/compose/client.yml" "$WORK/compose/" || exit 1
cd "$WORK" || exit 1
# A runner may reuse WORK: nothing of an earlier run is read as this one's.
sudo rm -rf keploy keploy.yml mock
rm -f ./*.log
MOCK_DIR="$WORK/mock"
mkdir -p "$MOCK_DIR" || exit 1

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
test_files() { find keploy/test-set-0/tests -maxdepth 1 -name '*.yaml' -size +0c 2>/dev/null | wc -l; }
wait_tests() {
    until [ "$(test_files)" -ge "$1" ]; do
        pid_running "$2" || return 1
        sleep 0.2
    done
}
# wait_app <log> PID: until keploy's log (which docker compose streams the
# app's output into) says both that the app listens (it says so once its
# port is bound) and that keploy's agent listens on the app's port for it
# (ingress forwarding: the agent moves the app's socket, then takes the
# port; a call before that reaches nothing and is reset), while PID runs.
# docker's published port takes connections before either.
wait_app() {
    until grep -q 'listening on' < <(plain "$1") &&
        grep -qE "Started ingress forwarding.*\"orig_port\": ?$APP[,}]" < <(plain "$1"); do
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
re() { printf '%s' "$1" | sed 's/[.]/\\./g'; }

# --- Dependencies -----------------------------------------------------------
# deps [compose args...]: the dependencies' project, Redis A at CUR_A,
# Redis B at CUR_B and memcached at MEM_IP.
CUR_A=$REC_A CUR_B=$REC_B
deps() { REDIS_IMAGE="$REDIS_IMAGE" MEMCACHED_IMAGE="$MEMCACHED_IMAGE" REDIS_A_IP=$CUR_A REDIS_B_IP=$CUR_B MEMCACHED_IP=$MEM_IP docker compose -f "$WORK/compose/deps.yml" -p redisskipdeps "$@"; }
# The app's project and keploy mock's client's, keploy's agent with each, as
# keploy left them.
app_down() {
    docker compose -f "$WORK/compose/app.yml" -p redisskipapp down -v --remove-orphans >/dev/null 2>&1
    docker compose -f "$WORK/compose/client.yml" -p redisskipclient down -v --remove-orphans >/dev/null 2>&1
}
REC_PID=""
cleanup() {
    [ -n "$REC_PID" ] && stop_pid "$REC_PID"
    app_down
    deps down -v --remove-orphans >/dev/null 2>&1
    docker network rm "$NET" >/dev/null 2>&1
    own
}
cleanup
trap cleanup EXIT
if docker network inspect "$NET" >/dev/null 2>&1; then
    echo "::error::the network $NET is still there (a container of another run holds it)"
    exit 1
fi
docker network create --subnet "$LANE_SUBNET" "$NET" >/dev/null ||
    { echo "::error::cannot create the lane's network $NET on $LANE_SUBNET (does a network of this daemon overlap it? set LANE_SUBNET)"; exit 1; }
docker_pull_retry "$REDIS_IMAGE"
docker_pull_retry "$MEMCACHED_IMAGE"
# ip_of <service>: the address the Redis service's container has on the
# lane's network.
ip_of() {
    local id
    id=$(deps ps -q "$1" 2>/dev/null)
    [ -n "$id" ] && docker inspect -f "{{(index .NetworkSettings.Networks \"$NET\").IPAddress}}" "$id" 2>/dev/null
}
# up_redis <address of A> <address of B> [compose up flags...]: Redis A and
# Redis B at those addresses, answering, and memcached with them.
up_redis() {
    local svc
    CUR_A=$1 CUR_B=$2
    shift 2
    deps up -d "$@" || { echo "::error::cannot start Redis A and Redis B"; deps logs | tail -40; exit 1; }
    for svc in redis-a redis-b; do
        until [ "$(deps exec -T "$svc" redis-cli ping 2>/dev/null | tr -d '\r')" = PONG ]; do
            [ -n "$(deps ps --status running -q "$svc" 2>/dev/null)" ] ||
                { echo "::error::$svc exited before it was ready"; deps logs "$svc" | tail -20; exit 1; }
            sleep 0.5
        done
    done
    [ "$(ip_of redis-a)" = "$CUR_A" ] && [ "$(ip_of redis-b)" = "$CUR_B" ] ||
        { echo "::error::Redis A and Redis B are at $(ip_of redis-a) and $(ip_of redis-b), want $CUR_A and $CUR_B"; exit 1; }
    # memcached, which comes up with them, answering its version.
    until [[ "$( (exec 3<>"/dev/tcp/$MEM_IP/11211" && printf 'version\r\n' >&3 && head -c 7 <&3) 2>/dev/null)" == VERSION ]]; do
        [ -n "$(deps ps --status running -q memcached 2>/dev/null)" ] ||
            { echo "::error::memcached exited before it was ready"; deps logs memcached | tail -20; exit 1; }
        sleep 0.5
    done
}
# conns <service>: the connections the Redis has accepted so far, the one
# redis-cli opens to ask among them (INFO counts it).
conns() { deps exec -T "$1" redis-cli INFO stats | tr -d '\r' | awk -F: '$1 == "total_connections_received" { print $2 }'; }
# since <before> <after>: the connections a Redis accepted between two conns
# readings, less the later reading's own.
since() { echo $(($2 - $1 - 1)); }

# --- Log readers ------------------------------------------------------------
# notices <log> <destination>: the notice lines naming the destination.
notices() { plain "$1" | grep -F "$NOTICE" | grep -cE "\"destination\": ?\"$(re "$2")\""; }
# check_notices <step> <log> <address of A> <address of B>: the notice is
# said exactly once for each Redis destination (the address the app dialled),
# at WARN, with the URL, and for nothing else.
check_notices() {
    local all dst
    all=$(plain "$2" | grep -cF "$NOTICE")
    for dst in "$3:6379" "$4:6379"; do
        [ "$(notices "$2" "$dst")" -eq 1 ] || fail "$1: the notice was said $(notices "$2" "$dst") times for $dst, want once"
    done
    [ "$all" -eq 2 ] || fail "$1: the notice was said $all times, want twice (once per Redis destination)"
    [ "$(plain "$2" | grep -F "$NOTICE" | grep -cF "$URL")" -eq "$all" ] || fail "$1: a notice does not carry $URL"
    [ "$(plain "$2" | grep -F "$NOTICE" | grep -cwF WARN)" -eq "$all" ] || fail "$1: a notice is not logged at WARN"
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
# live <log> <recorded address> <address now>: the relay's lines saying it
# dialled the address now for a connection the app made to the recorded one.
live() { plain "$1" | grep -F "$LIVE_LINE" | grep -E "\"recorded address\": ?\"$(re "$2"):6379\"" | grep -cE "\"address\": ?\"$(re "$3"):6379\""; }

# --- 1. Record --------------------------------------------------------------
up_redis "$REC_A" "$REC_B"
echo "Redis A is at $REC_A and Redis B at $REC_B"
APP_CMD="docker compose -f $WORK/compose/app.yml -p redisskipapp up"
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
sudo -E env -u KEPLOY_PROVEN PATH="$PATH" "$RECORD_BIN" record -c "$APP_CMD" --container-name "$APP_CONTAINER" \
    --generateGithubActions=false --disable-tele >record.log 2>&1 </dev/null &
REC_PID=$!
wait_app record.log "$REC_PID" || { echo "::error::the app did not start under keploy record"; plain record.log | tail -60; exit 1; }
send GET /health 200
send PUT /kv/a 200 alpha
send GET /kv/a 200
send PUT /kv/b 200 beta
send GET /kv/b 200
send GET /pipeline/p1 200
wait_tests "$TESTS" "$REC_PID" || fail "keploy record wrote $(test_files) test cases, want $TESTS"
stop_pid "$REC_PID"
REC_PID=""
own
plain record.log | tail -30
[ "$(test_files)" -eq "$TESTS" ] || fail "keploy record wrote $(test_files) test cases, want $TESTS"
MOCKS=keploy/test-set-0/mocks.yaml
[ -s "$MOCKS" ] || { fail "keploy record wrote no mocks (not even the DNS ones)"; MOCKS=/dev/null; }
recorded=$(yq -N 'select(.kind == "Generic" or .kind == "Redis") | .name' "$MOCKS" | grep -c .)
[ "$recorded" -eq 0 ] || fail "keploy record wrote $recorded Generic or Redis mock(s), want none: the app's only dependency is Redis, which this build relays"
# The DNS mocks the replay answers the app's lookups from: each name's
# answer holds the address it had at record.
for pair in "redis-a $REC_A" "redis-b $REC_B"; do
    set -- $pair
    n=$(yq -N "select(.kind == \"DNS\" and .spec.request.name == \"$1.\") | .spec.response.answers[]" "$MOCKS" | grep -cE "[[:space:]]A[[:space:]]+$(re "$2")\$")
    [ "$n" -ge 1 ] || fail "keploy record wrote no DNS mock answering $1 with $2: the replay would resolve $1 for real, and this cell would not check that the relay dials where the name points now"
done
check_notices "record" record.log "$REC_A" "$REC_B"
no_errors "record" record.log
echo "recorded $(test_files) test cases, no Redis mock, and the DNS answers of redis-a ($REC_A) and redis-b ($REC_B)"

# --- 5, first half. keploy mock record, Redis and memcached live -----------
# In a directory of its own, so its set is nobody else's; never under the
# proven switch, where this build refuses a compose keploy mock run.
CLIENT_CMD="docker compose -f $WORK/compose/client.yml -p redisskipclient up"
# mock_cmd <keploy> mock <record|replay> [env...]: keploy mock running the
# client's compose project, with env in its environment (compose reads
# CLIENT_LOOKUP from it).
mock_cmd() {
    local bin=$1 verb=$3
    shift 3
    (cd "$MOCK_DIR" && sudo -E env -u KEPLOY_PROVEN PATH="$PATH" "$@" "$bin" mock "$verb" -c "$CLIENT_CMD" \
        --container-name "$CLIENT_CONTAINER" --name "$MOCK_SET" --disable-tele --debug) </dev/null
}
mock_cmd "$RECORD_BIN" mock record >mock-record.log 2>&1
RC=$?
own
MOCK_MOCKS="$MOCK_DIR/keploy/$MOCK_SET/mocks.yaml"
if [ "$RC" -ne 0 ] || ! grep -qF "client: ok" < <(plain mock-record.log); then
    fail "keploy mock record of the client failed (exit $RC)"
    plain mock-record.log | tail -40
fi
[ -s "$MOCK_MOCKS" ] || { fail "keploy mock record wrote no mocks"; MOCK_MOCKS=/dev/null; }
mock_mem=$(yq -N 'select(.kind == "Generic") | .spec.genericrequests[0].message[0].data' "$MOCK_MOCKS" | grep -c '^get mock:none')
mock_redis=$(yq -N 'select(.kind == "Redis") | .name' "$MOCK_MOCKS" | grep -c .)
mock_generic=$(yq -N 'select(.kind == "Generic") | .name' "$MOCK_MOCKS" | grep -c .)
echo "keploy mock record wrote $mock_generic Generic mock(s), $mock_mem of them memcached's, and $mock_redis Redis mock(s)"
[ "$mock_mem" -ge 1 ] || fail "keploy mock record wrote no memcached mock: the replay below would serve nothing from the recording"
[ "$mock_redis" -eq 0 ] && [ "$mock_generic" -eq "$mock_mem" ] ||
    fail "keploy mock record wrote $mock_redis Redis and $((mock_generic - mock_mem)) other Generic mock(s), want only memcached's: this build relays Redis"
for name in redis-a memcached; do
    n=$(yq -N "select(.kind == \"DNS\" and .spec.request.name == \"$name.\") | .name" "$MOCK_MOCKS" | grep -c .)
    [ "$n" -ge 1 ] || fail "keploy mock record wrote no DNS mock of $name: the replay would resolve it for real"
done
no_errors "mock record" mock-record.log

# --- 2. Recreate Redis at other addresses -----------------------------------
up_redis "$REP_A" "$REP_B" --force-recreate
taken=$(docker network inspect "$NET" -f '{{range .Containers}}{{.IPv4Address}} {{end}}')
for a in "$REC_A" "$REC_B"; do
    grep -qE "(^| )$(re "$a")/" <<<"$taken" && fail "a container still has $a, an address Redis had at record: the replay could reach it there"
done
echo "Redis A is now at $REP_A and Redis B at $REP_B; the lane's network holds: $taken"

# --- 3. Replay ----------------------------------------------------------------
a0=$(conns redis-a)
b0=$(conns redis-b)
sudo -E env -u KEPLOY_PROVEN PATH="$PATH" "$REPLAY_BIN" test -c "$APP_CMD" --container-name "$APP_CONTAINER" \
    --delay 0 --health-path /health --debug --generateGithubActions=false --disable-tele >replay.log 2>&1 </dev/null
RC=$?
own
da=$(since "$a0" "$(conns redis-a)")
db=$(since "$b0" "$(conns redis-b)")
echo "replay: the recreated Redis A counted $da and Redis B $db connection(s)"
[ "$da" -ge 4 ] && [ "$db" -ge 4 ] || fail "replay: the recreated Redis A counted $da and Redis B $db connection(s), want at least the app's four startup connections each: Redis was not reached where its name points now"
report=$(ls -1d keploy/reports/test-run-*/test-set-0-report.yaml 2>/dev/null | sort -V | tail -n1)
if [ -z "$report" ]; then
    fail "replay: keploy test wrote no report (exit $RC)"
    plain replay.log | tail -60
else
    status=$(yq -N '.status' "$report")
    total=$(yq -N '.total' "$report")
    passed=$(yq -N '.success' "$report")
    echo "replay: exit $RC, status $status, $passed/$total passed"
    if [ "$RC" -ne 0 ] || [ "$status" != PASSED ] || [ "$total" != "$TESTS" ]; then
        fail "replay: keploy test must pass all $TESTS tests with Redis recreated (exit $RC, status $status, $passed/$total)"
        plain replay.log | grep -nE 'ERROR|could not connect|no matching|mismatch' | head -20
    fi
fi
relayed=$(plain replay.log | grep -cF "$SKIP_LINE")
la=$(live replay.log "$REC_A" "$REP_A")
lb=$(live replay.log "$REC_B" "$REP_B")
echo "replay: keploy relayed $relayed Redis connection(s), $la of them dialled at $REP_A for $REC_A and $lb at $REP_B for $REC_B"
[ "$relayed" -eq $((da + db)) ] || fail "replay: keploy relayed $relayed Redis connection(s), but the Redis servers counted $((da + db)): a connection reached Redis without the skip, or the skip relayed one that never reached it"
[ "$la" -eq "$da" ] && [ "$lb" -eq "$db" ] || fail "replay: the relay said it dialled Redis A's address now for $la connection(s) and Redis B's for $lb, but they counted $da and $db: a connection was dialled at the address the recording holds"
check_notices "replay" replay.log "$REC_A" "$REC_B"
no_errors "replay" replay.log

# --- 5, second half. keploy mock replay, Redis A recreated, memcached down -
deps stop memcached >/dev/null 2>&1 || fail "cannot stop memcached for keploy mock replay"
a0=$(conns redis-a)
mock_cmd "$REPLAY_BIN" mock replay CLIENT_LOOKUP=redis-b >mock-replay.log 2>&1
RC=$?
own
mock_da=$(since "$a0" "$(conns redis-a)")
RECEIPT="$MOCK_DIR/keploy/$MOCK_SET/last-replay.yaml"
echo "keploy mock replay: exit $RC; the recreated Redis A accepted $mock_da connection(s)"
if [ "$RC" -ne 0 ] || ! grep -qF "client: ok" < <(plain mock-replay.log); then
    fail "keploy mock replay of the client failed (exit $RC): with Redis live and memcached mocked it must pass"
    plain mock-replay.log | tail -40
fi
grep -qE "client: redis-b has (.*, )?$(re "$REP_B")(,|\$)" < <(plain mock-replay.log) ||
    fail "keploy mock replay: the client did not look redis-b up, or got another address than Redis B's ($REP_B)"
no_errors "mock replay" mock-replay.log
if [ ! -s "$RECEIPT" ]; then
    fail "keploy mock replay wrote no receipt ($RECEIPT)"
else
    yq -N '{"isolated": .isolated, "isolationNote": .isolationNote, "relayed": .relayed, "loaded": .loaded, "consumed": .consumed}' "$RECEIPT"
    [ "$(yq -N '.isolated' "$RECEIPT")" = false ] ||
        fail "mock replay: the receipt says the run is isolated, but its Redis answered live"
    note=$(yq -N '.isolationNote' "$RECEIPT")
    grep -qF "Redis relayed to the live server at redis-a:6379" <<<"$note" && grep -qF "open-source keploy does not mock Redis" <<<"$note" ||
        fail "mock replay: the receipt's isolation note does not say Redis was relayed to the live server at redis-a:6379, and why: $note"
    relayed=$(yq -N '[.relayed[] | select(.reason == "redis_enterprise_only" and .destination == "redis-a:6379") | .connections] | .[0] // 0' "$RECEIPT")
    [ "$relayed" -ge 1 ] && [ "$relayed" -eq "$mock_da" ] ||
        fail "mock replay: the receipt lists $relayed Redis connection(s) relayed to redis-a:6379, want every connection the recreated Redis A accepted ($mock_da): the account the stopped agent left did not carry them"
    lookups=$(yq -N '[.relayed[] | select(.reason == "dns_forwarded" and .query == "redis-b A") | .connections] | .[0] // 0' "$RECEIPT")
    [ "$lookups" -ge 1 ] ||
        fail "mock replay: the receipt lists no lookup of redis-b answered by the live resolver: the recording has no answer for it"
    others=$(yq -N '.relayed[] | select(((.reason == "redis_enterprise_only" and .destination == "redis-a:6379") or (.reason == "dns_forwarded" and (.query // "" | test("^redis-b ")))) | not) | .reason + " " + .destination + " " + (.query // "")' "$RECEIPT")
    [ -z "$others" ] || fail "mock replay: the receipt lists relays this run did not make: $others"
    [ "$(yq -N '.consumed' "$RECEIPT")" -ge 1 ] || fail "mock replay: the receipt says no recorded call was served: memcached's mock should have answered"
fi
if grep -qF "every dependency answered from the recording" < <(plain mock-replay.log); then
    fail "mock replay: keploy said the run had every dependency answered from the recording, but its Redis answered live"
fi
grep -F "this replay did not prove the tests run with the dependencies off" < <(plain mock-replay.log) | grep -qF "$URL" ||
    fail "mock replay: keploy did not say the run is not isolated, with Keploy Enterprise's URL as the next step"

# --- 4. ---------------------------------------------------------------------
if grep -l 'WARNING: DATA RACE' ./*.log; then fail "data race in a keploy log"; fi

if [ "$FAIL" -ne 0 ]; then
    echo "$FAIL check(s) failed"
    exit 1
fi
echo "all checks passed"
