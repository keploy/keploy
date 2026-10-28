#!/usr/bin/env bash
# E2E: a test is served the HTTP and MySQL calls it recorded, by the lifetime
# the recorder stated for each mock (Spec.Metadata["lifetime"]).
#
# The app (./app) is an HTTP API over MySQL that calls an auth service
# (./upstream). At startup it reads its configuration from the auth service
# and creates its tables. It fetches an auth token and a tax rate once, on
# first use, and caches them. Recorded in order:
#
#   post-orders-1       POST /orders        fetches the token and the rate;
#                                           opens the request pool's
#                                           connection, which the driver sets
#                                           up (SET sql_mode, time_zone)
#   get-profile-1       GET /profile        the cached token; GET /profile
#   get-quote-1         GET /quote          the cached rate; SELECT COUNT(*)
#   get-orders-count-1  GET /orders/count   SELECT COUNT(*)
#
# Replayed with MySQL and the auth service down. keploy record without --sync
# writes no test-mock mappings, so the first replay of a recording serves each
# test by its recorded window, and the lifetimes decide what else it may be
# served; a replay then writes the mappings of the tests it passed, and later
# replays serve a test by its mapping. Steps 1-5 replay the recording as
# recorded, each as its first replay (mappings.yaml removed):
#   1. The whole set passes, whoever recorded it and whoever replays it.
#   2. get-orders-count-1 alone passes, whoever recorded it and whoever
#      replays it: its connection is opened in its own window now, and the
#      connection's setup, recorded in post-orders-1's, is served to it; the
#      startup configuration and schema, recorded before any test, are served
#      at startup.
#   3. get-profile-1 alone (the token fetch was recorded only by
#      post-orders-1):
#      - a recording that states lifetimes, replayed by this build: the token
#        call misses, the miss is reported and names the keploy.yaml rule
#        that declares the call shared (sessionCalls), and the test fails;
#      - a recording made before: it passes, as it always did (the token
#        call recorded in post-orders-1 is served to it).
#   4. get-quote-1 alone (the rate was read only by post-orders-1): the same,
#      for MySQL, without the rule (it declares HTTP calls only).
#   5. get-profile-1 alone, with the token call declared in keploy.yaml
#      (sessionCalls), on a recording that states lifetimes: it passes.
# Then with the mappings a replay of the whole set wrote:
#   6. get-profile-1 alone misses the token call whoever recorded it: its
#      mapping names the mocks it is served. It FAILS, and keploy test exits
#      non-zero, on either recording. On one that states lifetimes its mapped
#      /profile call (per-test) also went unconsumed; keploy would demote a
#      test whose per-test mocks diverged from its mapping to OBSOLETE (not
#      judged, exit non-zero), but not one a call of which got no recorded
#      answer.
#   7. get-profile-1 alone by its mapping, with the token call declared in
#      keploy.yaml, on a recording that states lifetimes: it passes. The
#      mapping names the token call under post-orders-1, which this run does
#      not select; a call declared shared is every test's whichever test's
#      mapping names it.
#   8. A recording made by this build states a lifetime on every HTTP and
#      MySQL mock: data per-test, the connection's setup and the handshake
#      session, a prepared statement's PREPARE and CLOSE connection.
#   9. Under the proven switch (KEPLOY_PROVEN=1), a recording this build
#      made with --sync: every test passes. The connection's setup (SET) and
#      post-orders-1's transaction (START TRANSACTION, COMMIT) are stated
#      session, so no test is required to make them, and every test that uses
#      the connection is served them. This recording adds a fifth test, GET
#      /pool: one prepared statement on a pool of eight connections, run on
#      one connection for three ids in turn, then on all eight at once for
#      eight ids, then on all eight at once for one id. Each call is answered
#      with its own recorded answer: the app checks every row it reads and
#      answers 500, naming the call, for a row of another id or a call that
#      failed, so a recording served to the wrong call fails the test.
#  10. No data race in any log.
#  12. Under the proven switch, the recording of step 9 replayed by an app
#      whose GET /pool prepares another statement in place of the one it
#      recorded (SHOP_ITEM_QUERY): one of the same shape (another table),
#      then one of another (a condition more). No recording ran it: its
#      PREPARE gets the PREPARE_OK keploy makes, framed as the connection
#      negotiated (go-sql-driver reads an EOF after the parameter
#      definitions; without it the app hung in PREPARE), and each EXECUTE
#      of it is a miss, which the report names as test-5's unmatched call.
#      test-5 fails, keploy test exiting non-zero; the other tests pass. A
#      replayer that answered an EXECUTE with another statement's recording
#      (scored on the packet's header and parameters, or on the statement's
#      shape) served each call the row recorded for its id, and test-5
#      passed: a false green. Then the same recording replayed by an app
#      whose GET /pool binds another id to its own statement in its
#      sequential read (SHOP_ITEM_ID_SHIFT), while it still wants the row
#      of the id it recorded: each of those EXECUTEs is a miss too, and
#      test-5 fails. A replayer that answered them with the recording of
#      the statement made with another parameter (its next one, in recorded
#      order) served the rows the app wants, and test-5 passed.
#  11. A prepared read the app makes before any test, or between two tests,
#      is answered with a recording of its own request, and a test's with its
#      own: recorded with SHOP_SETTINGS_BY_KEY=1, the app reads two settings
#      at startup (the boot limit, then the boot retries), post-orders-1 the
#      tax rate, and between post-orders-1 and get-profile-1 the boot retries
#      again, by one prepared statement (SELECT k, v FROM settings WHERE k =
#      ?); each read checks that its row is its key's. While recording, the
#      lane signals the app for the between-tests read once post-orders-1 is
#      answered and waits for it; replayed, no signal comes, and the app
#      makes the read as get-profile-1 begins, in that test's window, which
#      holds no recording of the statement. No test window is open before
#      the first test. So neither read's own recording is the open test's,
#      and a replayer that ranks the statement's first recording with other
#      parameters above it answers the boot limit's read, and the between
#      read, with another key's row: the app logs each such read and answers
#      every request 500, and the set fails. Replayed as configured by
#      default (a strict mock window: the whole set is staged before the
#      first test, and then each test's window's recordings beside the
#      startup ones, so the between read's own recording is not staged and
#      the startup recording of the same read, with the same row, answers
#      it), with KEPLOY_STRICT_MOCK_WINDOW=0 (every recording is staged at
#      once, and the between read takes its own), and, recorded again with
#      --sync, under the proven switch. The app makes the between-tests read before it
#      checks the reads made so far, so on a replayer that answers a startup
#      read wrong the between read is still made and judged by its own log
#      line. Under the proven switch the between-tests read is recorded but
#      not made again when replayed (SHOP_BETWEEN_ON_SIGNAL_ONLY=1): keploy
#      test sends get-profile-1 as soon as post-orders-1 is answered, so,
#      with no wall-clock wait, a replayed read can only be made inside
#      get-profile-1's window, whose --sync mapping does not hold it, and
#      exact serving refuses it by design. There the startup reads are
#      judged, with the between-tests recording a second recording of the
#      boot retries' read outside every test.
#
# Which binary is this build: download-binary's build/keploy or
# build-no-race/keploy. A published release replaying a recording that states
# lifetimes may predate them, so steps 3-7 judge its outcome only for this
# build (steps 1 and 2 hold for every replayer); step 11 runs for this build's
# replayer alone, as a release before it answers the startup read with another
# request's row.
#
# Readiness is event-driven: MySQL answering SELECT 1, the auth service's and
# the app's ports, the test files keploy writes. Ports: 8096 (app), 9096 (auth
# service), 3306 (MySQL). Env: RECORD_BIN, REPLAY_BIN; MYSQL_IMAGE (default
# mysql:8.0).
set -uo pipefail

# docker_pull_retry: a registry hiccup is retried and named, not taken for a
# failed lane.
source "$(dirname "${BASH_SOURCE[0]}")/../../docker-build-retry.sh"
source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-$RECORD_BIN}"
MYSQL_IMAGE="${MYSQL_IMAGE:-mysql:8.0}"
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP=8096
UP=9096
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

WORK="$(mktemp -d)"
echo "work dir: $WORK"
(cd "$SRC" && go build -o "$WORK/shop" ./app && go build -o "$WORK/upstream" ./upstream) || { echo "::error::build failed"; exit 1; }
cd "$WORK" || exit 1

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

# wait_listen <port> <pid>: until something listens on port; fails at once
# when pid (the process that should start it) has exited.
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
newest_run() { ls -1d keploy/reports/test-run-* 2>/dev/null | sort -t- -k3 -n | tail -n1; }
REPORT() { echo "$(newest_run)/test-set-0-report.yaml"; }
set_status() { grep -m1 '^status:' "$(REPORT)" 2>/dev/null | awk '{print $2}'; }
# test_status <test id>: the test's status in the newest report.
test_status() { T="$1" yq -N '.tests[] | select(.test_case_id == strenv(T)) | .status' "$(REPORT)" 2>/dev/null; }
ran_tests() { yq -N '[.tests[].test_case_id] | join(" ")' "$(REPORT)" 2>/dev/null; }

MYSQL_CID=""
start_mysql() {
    docker_pull_retry "$MYSQL_IMAGE"
    MYSQL_CID=$(docker run -d --init -p 127.0.0.1:3306:3306 -e MYSQL_ROOT_PASSWORD=password \
        -e MYSQL_DATABASE=shop "$MYSQL_IMAGE") || { echo "::error::cannot start $MYSQL_IMAGE"; exit 1; }
    # Ready when the server answers a query over TCP (the image's first,
    # socket-only server answers the socket alone, and exits).
    until docker exec "$MYSQL_CID" mysql -h127.0.0.1 -uroot -ppassword -e 'SELECT 1' shop >/dev/null 2>&1; do
        [ "$(docker inspect -f '{{.State.Running}}' "$MYSQL_CID" 2>/dev/null)" = true ] ||
            { echo "::error::MySQL exited before it was ready"; docker logs "$MYSQL_CID" | tail -30; exit 1; }
        sleep 0.5
    done
}
stop_mysql() {
    [ -n "$MYSQL_CID" ] && docker rm -f "$MYSQL_CID" >/dev/null 2>&1
    MYSQL_CID=""
}
UP_PID=""
start_upstream() {
    ./upstream >upstream.log 2>&1 &
    UP_PID=$!
    wait_listen "$UP" "$UP_PID" || { echo "::error::the auth service did not start"; cat upstream.log; exit 1; }
}
stop_upstream() {
    [ -n "$UP_PID" ] && kill "$UP_PID" 2>/dev/null && wait "$UP_PID" 2>/dev/null
    UP_PID=""
}

REC_PID=""
cleanup() {
    [ -n "$REC_PID" ] && stop_pid "$REC_PID"
    stop_upstream
    stop_mysql
}
trap cleanup EXIT

# send <method> <path> <want status> [body]: one request while recording.
send() {
    local code
    if [ -n "${4:-}" ]; then
        code=$(curl -s -o /dev/null -w '%{http_code}' -X "$1" "http://127.0.0.1:$APP$2" -H 'Content-Type: application/json' -d "$4")
    else
        code=$(curl -s -o /dev/null -w '%{http_code}' -X "$1" "http://127.0.0.1:$APP$2")
    fi
    [ "$code" = "$3" ] || fail "$1 $2 answered $code while recording, want $3"
}

TESTS=4
# record [proven]: keploy record of the four requests; proven records under
# the proven switch with --sync (KEPLOY_PROVEN=1), which maps each test to the
# mocks it made as it records, and a fifth request, GET /pool.
record() {
    local proven="${1:-}" flags=() want="$TESTS"
    [ -n "$proven" ] && flags=(--sync) && want=$((TESTS + 1))
    rm -rf keploy keploy.yml
    start_mysql
    start_upstream
    sudo -E env -u KEPLOY_PROVEN PATH="$PATH" ${proven:+KEPLOY_PROVEN=1} "$RECORD_BIN" record -c "./shop" "${flags[@]}" \
        --generateGithubActions=false --disable-tele >record.log 2>&1 </dev/null &
    REC_PID=$!
    wait_listen "$APP" "$REC_PID" || { echo "::error::the app did not start under keploy record"; tail -30 record.log; exit 1; }
    send POST /orders 201 '{"qty":2}'
    [ "${SHOP_SETTINGS_BY_KEY:-}" = 1 ] && between_read
    send GET /profile 200
    send GET '/quote?qty=4' 200
    send GET /orders/count 200
    [ -n "$proven" ] && send GET /pool 200
    wait_tests "$want" "$REC_PID" || fail "keploy record wrote $(test_files) test cases, want $want"
    stop_pid "$REC_PID"
    REC_PID=""
    own
    stop_upstream
    stop_mysql
    # Under the proven switch keploy names the tests test-1..test-4; the
    # steps that select a test alone run on the recording made without it.
    [ -n "$proven" ] && return
    for t in post-orders-1 get-profile-1 get-quote-1 get-orders-count-1; do
        [ -s "keploy/test-set-0/tests/$t.yaml" ] || { echo "::error::keploy record wrote no $t"; ls keploy/test-set-0/tests; exit 1; }
    done
}

# between_read: step 11's read between two tests, while recording: signal the
# app (the pid it wrote to SHOP_PIDFILE) and wait for its log line saying the
# read is done, so the read is recorded between post-orders-1 and the next
# request.
between_read() {
    until [ -s "$SHOP_PIDFILE" ]; do
        pid_running "$REC_PID" || { fail "the app wrote no pid to $SHOP_PIDFILE"; return; }
        sleep 0.2
    done
    sudo kill -USR1 "$(cat "$SHOP_PIDFILE")" || fail "could not signal the app for the between-tests read"
    until grep -qE 'between-tests setting boot_retries = |between tests: ' record.log; do
        pid_running "$REC_PID" || { fail "keploy record exited before the app's between-tests read"; return; }
        sleep 0.2
    done
}

# replay <name> <fresh|mapped> [only <test id>] [declare]: keploy test of the
# recording, with MySQL and the auth service down. fresh replays it as
# recorded, its first replay (no mappings.yaml); mapped keeps the mappings an
# earlier replay wrote. only runs one test; declare declares the token call in
# keploy.yaml. RC is its exit code.
replay() {
    local name="$1" mapping="$2" only="${3:-}" declare="${4:-}" before
    local switch=()
    [ -n "${PROVEN_REPLAY:-}" ] && switch=(KEPLOY_PROVEN=1)
    [ "$mapping" = fresh ] && rm -f keploy/test-set-0/mappings.yaml
    rm -f keploy.yml
    {
        if [ -n "$only" ]; then
            printf 'test:\n    selectedTests:\n        test-set-0: ["%s"]\n' "$only"
        fi
        if [ -n "$declare" ]; then
            printf 'sessionCalls:\n    - host: "127.0.0.1:%s"\n      method: POST\n      pathPrefix: /oauth/token\n' "$UP"
        fi
    } >keploy.yml
    before="$(newest_run)"
    sudo -E env -u KEPLOY_PROVEN PATH="$PATH" "${switch[@]}" "$REPLAY_BIN" test -c "./shop" --delay 1 --health-path /healthz \
        --generateGithubActions=false --disable-tele >"$name.log" 2>&1 </dev/null
    RC=$?
    own
    rm -f keploy.yml
    if [ "$(newest_run)" = "$before" ] || [ ! -f "$(REPORT)" ]; then
        fail "$name: keploy test wrote no report (exit $RC)"
        tail -40 "$name.log"
        return 1
    fi
    if [ -n "$only" ] && [ "$(ran_tests)" != "$only" ]; then
        fail "$name: keploy test ran '$(ran_tests)', want $only alone"
    fi
}
expect_pass() {
    if [ "$RC" -ne 0 ] || [ "$(set_status)" != PASSED ]; then
        fail "$1: the set must pass (exit $RC, status $(set_status))"
        grep -nE '^status:|^    status:|test_case_id|body: ' "$(REPORT)" | head -40
        grep -nE 'no matching mock|ERROR' "$2" | head -20
    fi
}
# expect_miss <step> <test id> <status> <log> <pattern> [hint]: the test got
# status, and its log reports the call that got no recorded answer (pattern),
# and, when hint is set, names the keploy.yaml rule that declares it.
expect_miss() {
    if [ "$(test_status "$2")" != "$3" ]; then
        fail "$1: $2 must be $3 (exit $RC, status $(test_status "$2"))"
    fi
    if [ "$3" = FAILED ] && [ "$RC" -eq 0 ]; then
        fail "$1: keploy test exited 0 with $2 FAILED"
    fi
    grep -qE "$5" "$4" || fail "$1: the log reports no miss matching '$5'"
    if [ -n "${6:-}" ]; then
        grep -q 'sessionCalls' "$4" || fail "$1: the miss does not name keploy.yaml's sessionCalls"
    fi
}

# lifetimes <mocks.yaml>: one line per HTTP and MySQL mock: kind, its first
# request (method and URL, or command and query), its stated lifetime, its
# type tag.
lifetimes() {
    yq -N 'select(.kind == "Http" or .kind == "MySQL") |
        [.kind,
         (.spec.req.method // .spec.requests[0].header.packet_type // "-"),
         (.spec.req.url // .spec.requests[0].message.query // "-"),
         (.spec.metadata.lifetime // "none"),
         (.spec.metadata.type // "-")] | @tsv' "$1"
}
# stated <kind> <first> <url or query regex> <want>: every such mock states want.
stated() {
    local rows bad
    rows=$(lifetimes keploy/test-set-0/mocks.yaml | awk -F'\t' -v k="$1" -v f="$2" -v q="$3" '$1 == k && $2 == f && $3 ~ q')
    [ -n "$rows" ] || { fail "the recording holds no $1 $2 matching '$3'"; return; }
    bad=$(awk -F'\t' -v w="$4" '$4 != w' <<<"$rows")
    [ -z "$bad" ] || fail "the recording states another lifetime than $4 for: $(tr '\n' ';' <<<"$bad")"
}

for p in "$APP" "$UP" 3306; do
    if listening "$p"; then echo "::error::port $p is in use"; exit 1; fi
done

record
cp keploy/test-set-0/mocks.yaml mocks.recorded.yaml
total=$(lifetimes mocks.recorded.yaml | wc -l)
with=$(lifetimes mocks.recorded.yaml | awk -F'\t' '$4 != "none"' | wc -l)
echo "recorded $total HTTP and MySQL mocks, $with stating a lifetime"
states=false
[ "$with" -gt 0 ] && states=true
if is_build "$RECORD_BIN"; then
    # 8. This build's recorder states every lifetime.
    [ "$with" -eq "$total" ] || fail "this build recorded $((total - with)) of $total HTTP and MySQL mocks without a lifetime"
    stated Http GET 'config/boot' perTest
    stated Http POST 'oauth/token' perTest
    stated Http GET 'profile' perTest
    # The handshake and auth (type config).
    hs=$(lifetimes mocks.recorded.yaml | awk -F'\t' '$1 == "MySQL" && $5 == "config"')
    [ -n "$hs" ] || fail "the recording holds no MySQL handshake"
    [ -z "$(awk -F'\t' '$4 != "session"' <<<"$hs")" ] || fail "a MySQL handshake states another lifetime than session: $(tr '\n' ';' <<<"$hs")"
    stated MySQL COM_QUERY '^(SET|set) ' session
    stated MySQL COM_QUERY 'FROM settings' perTest
    stated MySQL COM_QUERY 'COUNT' perTest
    stated MySQL COM_QUERY 'CREATE TABLE' perTest
    stated MySQL COM_STMT_PREPARE '.*' connection
    stated MySQL COM_STMT_EXECUTE '.*' perTest
    stated MySQL COM_STMT_CLOSE '.*' connection
fi

TOKEN_MISS='no matching mock for outgoing call.*POST /oauth/token'
RATE_MISS="no matching mock for outgoing call.*SELECT v FROM settings WHERE k = 'tax_rate'"
judged=true
is_build "$REPLAY_BIN" || judged=false

# 1. The whole set.
replay all fresh && expect_pass "the whole set" all.log
# 2. get-orders-count-1 alone: the connection's setup and the startup traffic
#    are shared.
replay only-count fresh get-orders-count-1 && expect_pass "get-orders-count-1 alone" only-count.log
# 3. get-profile-1 alone: the token call only post-orders-1 recorded.
if replay only-profile fresh get-profile-1; then
    if [ "$judged" = false ]; then
        echo "get-profile-1 alone, replayed by a release: exit $RC, status $(set_status) (not judged)"
    elif [ "$states" = true ]; then
        expect_miss "get-profile-1 alone" get-profile-1 FAILED only-profile.log "$TOKEN_MISS" hint
    else
        expect_pass "get-profile-1 alone, on a recording made before" only-profile.log
    fi
fi
# 4. get-quote-1 alone: the rate only post-orders-1 read.
if replay only-quote fresh get-quote-1; then
    if [ "$judged" = false ]; then
        echo "get-quote-1 alone, replayed by a release: exit $RC, status $(set_status) (not judged)"
    elif [ "$states" = true ]; then
        expect_miss "get-quote-1 alone" get-quote-1 FAILED only-quote.log "$RATE_MISS"
    else
        expect_pass "get-quote-1 alone, on a recording made before" only-quote.log
    fi
fi
# 5. get-profile-1 alone, the token call declared shared.
if [ "$judged" = true ] && [ "$states" = true ]; then
    replay only-profile-declared fresh get-profile-1 declare &&
        expect_pass "get-profile-1 alone, with the token call declared in keploy.yaml" only-profile-declared.log
fi
# 6. With the mappings a replay of the whole set writes.
replay all-mapping fresh && expect_pass "the whole set, writing its mappings" all-mapping.log
[ -s keploy/test-set-0/mappings.yaml ] || fail "the replay of the whole set wrote no mappings.yaml"
if replay mapped-profile mapped get-profile-1; then
    if [ "$judged" = false ]; then
        echo "get-profile-1 alone by its mapping, replayed by a release: exit $RC, status $(set_status) (not judged)"
    elif [ "$states" = true ]; then
        expect_miss "get-profile-1 alone by its mapping" get-profile-1 FAILED mapped-profile.log "$TOKEN_MISS" hint
    else
        expect_miss "get-profile-1 alone by its mapping, on a recording made before" get-profile-1 FAILED mapped-profile.log "$TOKEN_MISS"
    fi
fi
# 7. By its mapping, the token call declared shared.
if [ "$judged" = true ] && [ "$states" = true ]; then
    replay mapped-profile-declared mapped get-profile-1 declare &&
        expect_pass "get-profile-1 alone by its mapping, with the token call declared in keploy.yaml" mapped-profile-declared.log
fi

# 9. Under the proven switch, a recording made with --sync: every test is
#    served exactly the mocks its mapping names, and passes. The connection's
#    setup (SET) and post-orders-1's transaction (START TRANSACTION, COMMIT)
#    are stated session, so they are protocol-phase, not calls a test must
#    make: in post-orders-1's window or not, they are served to whichever
#    test opens or uses the connection.
if is_build "$RECORD_BIN" && [ "$judged" = true ]; then
    mkdir -p "$WORK/proven" && cd "$WORK/proven" || exit 1
    cp "$WORK/shop" "$WORK/upstream" .
    record proven
    [ -s keploy/test-set-0/mappings.yaml ] || fail "keploy record --sync wrote no mappings.yaml"
    for q in 'START TRANSACTION' COMMIT; do
        [ -n "$(lifetimes keploy/test-set-0/mocks.yaml | awk -F'\t' -v q="$q" '$1 == "MySQL" && $3 == q && $4 == "session"')" ] ||
            fail "the --sync recording holds no $q stated session"
    done
    PROVEN_REPLAY=1 replay proven-all mapped && expect_pass "the whole set under the proven switch" proven-all.log
    # The pool test, by name: keploy names the fifth request test-5.
    pool_url=$(yq -N '.spec.req.url' keploy/test-set-0/tests/test-5.yaml 2>/dev/null)
    case "$pool_url" in
        */pool) ;;
        *) fail "the --sync recording's test-5 is '$pool_url', want GET /pool" ;;
    esac
    if [ "$(test_status test-5)" != PASSED ]; then
        fail "under the proven switch GET /pool (test-5) must pass: each call of the pool its own recorded answer (status $(test_status test-5))"
        # What the app answered: the calls that read another id's row or failed.
        T=test-5 yq -N '.tests[] | select(.test_case_id == strenv(T)) | .result.body_result[].actual' "$(REPORT)" 2>/dev/null | head -c 4000
        echo
    fi
    refused=$(grep -ac 'failed to update matched mock' proven-all.log)
    [ "$refused" -eq 0 ] || fail "under the proven switch the MySQL matcher failed $refused command(s) with a consume the store refused (failed to update matched mock)"

    # 12. The pool's prepared read changed since it was recorded. Each replay
    #     starts from the mappings step 9 replayed with. changed_read NAME
    #     WHAT STMT replays as NAME, with the app's change (WHAT) in the
    #     environment, and checks test-5 failed on an unmatched EXECUTE of
    #     STMT while the other tests passed.
    cp keploy/test-set-0/mappings.yaml mappings.step9
    changed_read() {
        cp mappings.step9 keploy/test-set-0/mappings.yaml
        PROVEN_REPLAY=1 replay "$1" mapped
        # Under the proven switch a miss is logged at debug only: the report
        # names it, as test-5's unmatched call.
        [ "$(test_status test-5)" = FAILED ] ||
            fail "under the proven switch, GET /pool $2: test-5 must be FAILED (exit $RC, status $(test_status test-5))"
        [ "$RC" -ne 0 ] || fail "under the proven switch, GET /pool $2: keploy test exited 0"
        T=test-5 S="COM_STMT_EXECUTE $3" yq -e '[.tests[] | select(.test_case_id == strenv(T)) | .failure_info.unmatched_calls[]?.actual_summary] | any_c(contains(strenv(S)))' "$(REPORT)" >/dev/null ||
            fail "under the proven switch, GET /pool $2: the report names no unmatched call of test-5 running '$3' ($(T=test-5 yq -N -o=json -I=0 '[.tests[] | select(.test_case_id == strenv(T)) | .failure_info.unmatched_calls[]?.actual_summary]' "$(REPORT)" 2>/dev/null))"
        for t in test-1 test-2 test-3 test-4; do
            [ "$(test_status "$t")" = PASSED ] ||
                fail "under the proven switch, with GET /pool $2, $t must still pass (status $(test_status "$t"))"
        done
    }
    n=0
    for changed in 'SELECT id, name, price FROM items_v2 WHERE id = ?' \
        'SELECT id, name, price FROM items WHERE id = ? AND price > 0'; do
        n=$((n + 1))
        export SHOP_ITEM_QUERY="$changed"
        changed_read "proven-changed-$n" "running '$changed'" "$changed"
        unset SHOP_ITEM_QUERY
    done
    export SHOP_ITEM_ID_SHIFT=100
    changed_read proven-changed-param "binding another id to its statement" 'SELECT id, name, price FROM items WHERE id = ?'
    unset SHOP_ITEM_ID_SHIFT
    cd "$WORK" || exit 1
fi

# 11. A prepared read before any test, the same statement read in a test with
#     other parameters.
if [ "$judged" = true ]; then
    mkdir -p "$WORK/by-key" && cd "$WORK/by-key" || exit 1
    cp "$WORK/shop" "$WORK/upstream" .
    export SHOP_SETTINGS_BY_KEY=1 SHOP_PIDFILE="$WORK/by-key/shop.pid"
    rm -f "$SHOP_PIDFILE"
    record
    grep -q 'between-tests setting boot_retries = 5' record.log ||
        fail "while recording, the app's between-tests read did not read the boot retries"
    # The recording holds each read once: the startup ones, and
    # post-orders-1's.
    for kn in boot_limit:1 boot_retries:2 tax_rate:1; do
        [ "$(K="${kn%%:*}" yq -N 'select(.kind == "MySQL" and .spec.requests[0].header.packet_type == "COM_STMT_EXECUTE" and
            .spec.requests[0].message.parameters[0].value == strenv(K)) | .name' keploy/test-set-0/mocks.yaml | grep -c .)" -eq "${kn##*:}" ] ||
            fail "the recording with settings read by key holds not ${kn##*:} prepared read(s) of ${kn%%:*}"
    done
    replay by-key fresh && expect_pass "the whole set, its startup reads prepared" by-key.log
    if grep -n 'got the row of' by-key.log; then
        fail "a prepared read was answered with another request's row"
    fi
    export KEPLOY_STRICT_MOCK_WINDOW=0
    replay by-key-lax fresh && expect_pass "the whole set, its startup reads prepared, every recording staged at once" by-key-lax.log
    unset KEPLOY_STRICT_MOCK_WINDOW
    if grep -n 'got the row of' by-key-lax.log; then
        fail "with every recording staged at once, a prepared read was answered with another request's row"
    fi
    for l in by-key.log by-key-lax.log; do
        grep -q 'between-tests setting boot_retries = 5' "$l" || fail "$l: the app's between-tests read did not read the boot retries"
    done
    # Under the proven switch, a recording made with --sync.
    mkdir -p "$WORK/by-key-proven" && cd "$WORK/by-key-proven" || exit 1
    cp "$WORK/shop" "$WORK/upstream" .
    SHOP_PIDFILE="$WORK/by-key-proven/shop.pid"
    export SHOP_BETWEEN_ON_SIGNAL_ONLY=1
    rm -f "$SHOP_PIDFILE"
    record proven
    grep -q 'between-tests setting boot_retries = 5' record.log ||
        fail "while recording with --sync, the app's between-tests read did not read the boot retries"
    PROVEN_REPLAY=1 replay by-key-proven mapped && expect_pass "the whole set, its startup reads prepared and a between-tests read recorded, under the proven switch" by-key-proven.log
    if grep -n 'got the row of' by-key-proven.log; then
        fail "under the proven switch, a prepared read was answered with another request's row"
    fi
    for kv in 'boot_limit = 7' 'boot_retries = 5'; do
        grep -q "startup setting $kv" by-key-proven.log || fail "by-key-proven.log: the app's startup read did not log $kv"
    done
    unset SHOP_SETTINGS_BY_KEY SHOP_PIDFILE SHOP_BETWEEN_ON_SIGNAL_ONLY
    cd "$WORK" || exit 1
fi

# 10.
if grep -rl 'WARNING: DATA RACE' --include='*.log' .; then fail "data race in a keploy log"; fi

if [ "$FAIL" -ne 0 ]; then
    echo "$FAIL check(s) failed"
    exit 1
fi
echo "all checks passed"
