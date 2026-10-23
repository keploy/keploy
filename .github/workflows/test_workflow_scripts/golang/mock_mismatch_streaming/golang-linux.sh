#!/usr/bin/env bash
# E2E for STREAMING mock-mismatch reporting.
#
# The app (./app) serves streaming responses (SSE, NDJSON, multipart, plain
# text), each built from one read of its store (./store), an HTTP service it
# calls; the SSE stream also carries a banner the app reads from the store's
# line-protocol cache over plain TCP (a memcached text get), which keploy
# records and mocks through its Generic parser, keploy's parser of every
# protocol it has no parser for. SSE, NDJSON and multipart are streams by
# their Content-Type; the text/plain one is a stream only when its recording
# kept its body as frames (a keploy recorder older than that rule), and is
# otherwise replayed whole in the main loop. We record it, then MUTATE the
# recorded read the streaming GETs depend on (GET /store/messages/list) to a
# path no request makes (the writes are left intact, so the mismatch is
# isolated to the streaming reads). On replay the streaming (Phase-2) path
# must SURFACE the unmatched outgoing call on each streaming test case (SSE,
# NDJSON, multipart), the plain-text one must report it too, every test but
# those four must pass, and keploy test must exit 1: this is the behaviour
# added by finalizing the per-test capture window AFTER stream consumption;
# before it, the streaming path opened no window and carried no
# unmatched_calls. Success here is INVERTED: we assert the streaming mismatch
# is reported, not that every test passes. The recorded banner get is
# rewritten too, to a request the app never sends, and the SSE test must
# report that Generic miss as well: a Generic miss on a streaming test is
# reported like an HTTP one. Then the same replay under the proven switch
# (KEPLOY_PROVEN=1).
#
# The lane ran on the samples-go sse-redis sample until Redis became a Keploy
# Enterprise dependency (OSS keploy passes Redis through and mocks none of
# it); the store keeps the feature, which is OSS code, covered without Redis.
#
# Readiness is event-driven: the store's and the app's ports, the test files
# keploy writes. Ports: 8097 (app), 9097 (store), 9098 (the store's
# line-protocol cache). Env: RECORD_BIN (either keploy), REPLAY_BIN (this
# build: a release replayer predates the feature).
set -uo pipefail

SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SRC/../../test-iid.sh" || true

: "${RECORD_BIN:?RECORD_BIN: the keploy that records}"
: "${REPLAY_BIN:?REPLAY_BIN: the keploy that replays (this build)}"
APP=8097
STORE=9097
LINES=9098
echo "record: $RECORD_BIN"
echo "replay: $REPLAY_BIN"
case "$REPLAY_BIN" in
    */build/keploy | */build-no-race/keploy) ;;
    *)
        echo "::error::REPLAY_BIN ($REPLAY_BIN) is not this build: streaming mock-mismatch reporting exists only in the build"
        exit 1
        ;;
esac

WORK="${WORK:-$(mktemp -d)}"
mkdir -p "$WORK"
echo "work dir: $WORK"
(cd "$SRC" && go build -o "$WORK/stream-app" ./app && go build -o "$WORK/store" ./store) || { echo "::error::build failed"; exit 1; }
cd "$WORK" || exit 1
sudo rm -rf keploy keploy.yml
rm -f ./*.log ./*.txt

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
STORE_PID=""
stop_store() {
    [ -n "$STORE_PID" ] && kill "$STORE_PID" 2>/dev/null && wait "$STORE_PID" 2>/dev/null
    STORE_PID=""
}
REC_PID=""
cleanup() {
    [ -n "$REC_PID" ] && stop_pid "$REC_PID"
    stop_store
    own
}
trap cleanup EXIT

for p in "$APP" "$STORE" "$LINES"; do
    if listening "$p"; then echo "::error::port $p is in use"; exit 1; fi
done

# --- Record -----------------------------------------------------------------
./store >store.log 2>&1 &
STORE_PID=$!
wait_listen "$STORE" "$STORE_PID" || { echo "::error::the store did not start"; cat store.log; exit 1; }
wait_listen "$LINES" "$STORE_PID" || { echo "::error::the store's line-protocol cache did not start"; cat store.log; exit 1; }

sudo -E env -u KEPLOY_PROVEN PATH="$PATH" "$RECORD_BIN" record -c "./stream-app" \
    --generateGithubActions=false --disable-tele >record_logs.txt 2>&1 </dev/null &
REC_PID=$!
wait_listen "$APP" "$REC_PID" || { echo "::error::the app did not start under keploy record"; tail -30 record_logs.txt; exit 1; }

BASE="http://127.0.0.1:$APP"
REQUESTS=0
# req <want status> <name> <curl args...>: one request while recording, its
# body kept in body-<name>.txt. A request that fails stops the lane at once:
# its test case would never be written, and the wait below would never end.
req() {
    local want=$1 name=$2 code
    shift 2
    code=$(curl -sS -N -o "body-$name.txt" -w '%{http_code}' "$@") || code="no answer (curl exit $?)"
    REQUESTS=$((REQUESTS + 1))
    if [ "$code" != "$want" ]; then
        echo "::error::$name answered $code while recording, want $want"
        cat "body-$name.txt" 2>/dev/null
        tail -40 record_logs.txt
        exit 1
    fi
}
MESSAGES=("hello world" "streaming is awesome" "keploy rocks")
req 200 clear -X DELETE "$BASE/messages"
req 201 post-1 -X POST "$BASE/messages" -H 'Content-Type: application/json' -d '{"text":"hello world","category":"greeting"}'
req 201 post-2 -X POST "$BASE/messages" -H 'Content-Type: application/json' -d '{"text":"streaming is awesome","category":"demo"}'
req 201 post-3 -X POST "$BASE/messages" -H 'Content-Type: application/json' -d '{"text":"keploy rocks","category":"test"}'
req 200 sse -H 'Accept: text/event-stream' "$BASE/events/sse"
req 200 ndjson "$BASE/events/ndjson"
req 200 multipart "$BASE/events/multipart"
req 200 plain "$BASE/events/plain"
req 200 health "$BASE/health"
# Every stream the app sent carried every message, one frame each: the app
# streamed several frames, what the streaming path is about. (Whether the
# recorder kept them as frames is the replay's to show, below.)
for name in sse ndjson multipart plain; do
    for m in "${MESSAGES[@]}"; do
        grep -qF "$m" "body-$name.txt" || { echo "::error::the $name stream recorded lacks \"$m\""; cat "body-$name.txt"; exit 1; }
    done
done
[ "$(grep -c . body-ndjson.txt)" -eq 3 ] || { echo "::error::the NDJSON stream recorded has $(grep -c . body-ndjson.txt) lines, want 3"; exit 1; }
grep -qF '"banner":"keploy streams"' body-sse.txt || { echo "::error::the SSE stream recorded lacks the banner the app reads from the cache"; cat body-sse.txt; exit 1; }
# The wait ends when keploy has written every test case, or when keploy
# ends: a recorder that drops one while it keeps running leaves the wait to
# the job's timeout, no event telling a test case it will never write from
# one it has not written yet.
wait_tests "$REQUESTS" "$REC_PID" || { echo "::error::keploy record wrote $(test_files) test cases, want $REQUESTS"; tail -40 record_logs.txt; exit 1; }
stop_pid "$REC_PID"
REC_PID=""
own
stop_store
cat record_logs.txt
if grep -q "WARNING: DATA RACE" record_logs.txt; then
    echo "::error::Race condition detected in recording"
    exit 1
fi
echo "Recorded streaming test cases and the store's mocks"

# --- Force a streaming mock mismatch ----------------------------------------
# Rewrite the recorded read of the message list only: the URL of every HTTP
# GET mock of /store/messages/list, one per streaming GET. Rewritten to a path
# no request makes, the live read matches no recorded mock (the HTTP matcher
# compares the path; a static segment is never taken for a dynamic one). The
# writes (POST and DELETE /store/messages) are left as recorded, so the
# mismatch is isolated to the streaming reads.
MOCKS=keploy/test-set-0/mocks.yaml
[ -s "$MOCKS" ] || { echo "::error::keploy record wrote no mocks"; exit 1; }
LIST_READ='select(.kind == "Http" and .spec.req.method == "GET" and ((.spec.req.url // "") | test("/store/messages/list$")))'
WRITES='select(.kind == "Http" and (.spec.req.method == "POST" or .spec.req.method == "DELETE") and ((.spec.req.url // "") | test("/store/messages$")))'
count() { yq -N "$1 | .name" "$MOCKS" | grep -c . ; }
reads=$(count "$LIST_READ")
writes=$(count "$WRITES")
echo "recorded $reads list read(s) and $writes write(s) of the store"
[ "$reads" -eq 4 ] || { echo "::error::recorded $reads list reads of the store, want 4 (one per streaming GET)"; yq -N '.kind + " " + .spec.req.method + " " + .spec.req.url' "$MOCKS"; exit 1; }
[ "$writes" -eq 4 ] || { echo "::error::recorded $writes writes of the store, want 4 (DELETE and three POSTs)"; exit 1; }
yq -i "($LIST_READ | .spec.req.url) |= sub(\"/store/messages/list$\"; \"/store/messages/MISS\")" "$MOCKS"
[ "$(count "$LIST_READ")" -eq 0 ] || { echo "::error::a list read survived the rewrite"; exit 1; }
[ "$(count 'select(.kind == "Http" and ((.spec.req.url // "") | test("/store/messages/MISS$")))')" -eq 4 ] || { echo "::error::the rewrite did not leave four MISS reads"; exit 1; }
[ "$(count "$WRITES")" -eq 4 ] || { echo "::error::the rewrite touched the store's writes"; exit 1; }
# The banner get, recorded by the Generic parser, is rewritten to a request
# the app never sends (another command, another key), so the replay's get
# matches no recorded exchange, nor comes close to one: Generic matches a
# session exchange at a Jaccard similarity of 0.4.
BANNER_GET='select(.kind == "Generic" and ((.spec.genericrequests[0].message[0].data // "") | test("^get banner")))'
banners=$(count "$BANNER_GET")
[ "$banners" -eq 1 ] || { echo "::error::recorded $banners Generic mock(s) of the banner get, want 1"; yq -N 'select(.kind == "Generic") | .name + " " + .spec.genericrequests[0].message[0].data' "$MOCKS"; exit 1; }
TOUCH=$'touch zzzzzzzzzzzzzzzz 0\r\n' yq -i "($BANNER_GET | .spec.genericrequests[0].message[0].data) = strenv(TOUCH)" "$MOCKS"
[ "$(count "$BANNER_GET")" -eq 0 ] || { echo "::error::the banner get survived the rewrite"; exit 1; }
[ "$(count 'select(.kind == "Generic" and ((.spec.genericrequests[0].message[0].data // "") | test("^touch zzzz")))')" -eq 1 ] || { echo "::error::the rewrite did not leave one touch request"; exit 1; }

# --- Replay -----------------------------------------------------------------
# The store is down: keploy answers for it, and the mutated reads match no
# recorded mock, so the streaming path must report the unmatched outgoing call
# on a streaming test. Replay is EXPECTED to not all-pass here.
sudo -E env -u KEPLOY_PROVEN PATH="$PATH" "$REPLAY_BIN" test -c "./stream-app" --delay 0 --health-path /health \
    --generateGithubActions=false --disable-tele >test_logs.txt 2>&1 </dev/null
REPLAY_RC=$?
own
cat test_logs.txt
if grep -q "WARNING: DATA RACE" test_logs.txt; then
    echo "::error::Race condition detected in test"
    exit 1
fi

# Assert the mismatch surfaced on a STREAMING test case (events-sse / -ndjson /
# -multipart): a test routed through the Phase-2 streaming path carries an
# unmatched_calls entry. Before the fix the streaming path opened no capture
# window, so these tests carried no unmatched_calls regardless of the forced
# mismatch. events-plain is checked on its own: whether it is a stream depends
# on which keploy recorded it (frames from an older one, whole from this one,
# which runs it in the main loop), and its miss must be reported whichever
# path ran it.
shopt -s nullglob
reports=(./keploy/reports/test-run-*/test-set-*-report.yaml)
shopt -u nullglob
if [ ${#reports[@]} -eq 0 ]; then
    echo "::error::no test-set report produced"
    tail -40 test_logs.txt
    exit 1
fi
# report_has_streaming_miss says whether a report holds a streaming test one
# of whose unmatched_calls is the forced miss, the read of the message list
# (its actual_summary names the path); with a second argument, a test whose id
# matches that pattern instead.
report_has_streaming_miss() {
    local hits
    hits=$(P="${2:-events-(sse|ndjson|multipart)}" yq -N '.tests[] | select(.test_case_id | test(strenv(P))) |
        select([.failure_info.unmatched_calls[] | (.actual_summary // "") | select(test("/store/messages/list"))] | length > 0) |
        .test_case_id' "$1")
    [ -n "$hits" ] || return 1
    echo "the forced miss was reported on: $(tr '\n' ' ' <<<"$hits")"
}
# report_has_generic_miss <report>: the SSE test carries an unmatched call
# the Generic parser reported (the rewritten banner get).
report_has_generic_miss() {
    local hits
    hits=$(yq -N '.tests[] | select(.test_case_id | test("events-sse")) |
        select([.failure_info.unmatched_calls[] | select(.protocol == "Generic")] | length > 0) |
        .test_case_id' "$1")
    [ -n "$hits" ] || return 1
    echo "the forced Generic miss was reported on: $(tr '\n' ' ' <<<"$hits")"
}
# require_replay <log> <exit code> <report>...: the replay's verdict on the
# forced misses. Each streaming test (SSE, NDJSON and multipart, each read
# through its own framing) reports the miss of its read of the store, and
# the SSE test the Generic miss of its banner get; every
# other test (the writes, which the rewrite left alone, and the health check)
# passes, so the miss stayed where it was forced; and keploy test exits 1,
# its code for failed tests, not a crash's or a signal's.
require_replay() {
    local log=$1 rc=$2 kind rf ok
    shift 2
    for kind in sse ndjson multipart; do
        ok=false
        for rf in "$@"; do
            if report_has_streaming_miss "$rf" "events-$kind"; then
                ok=true
                break
            fi
        done
        if [ "$ok" != true ]; then
            echo "::error::replay did NOT report the forced mock mismatch on the $kind streaming test ($log)"
            echo "--- mismatch/unmatched lines in $log ---"
            grep -iE "mismatch|unmatched|no matching" "$log" | head -20
            echo "--- tail of $log ---"
            tail -40 "$log"
            exit 1
        fi
    done
    ok=false
    for rf in "$@"; do
        if report_has_generic_miss "$rf"; then
            ok=true
            break
        fi
    done
    if [ "$ok" != true ]; then
        echo "::error::replay did NOT report the forced Generic miss (the banner get) on the SSE streaming test ($log)"
        grep -n -B2 -A12 'protocol: Generic' "$@" | head -40
        grep -iE "generic|banner" "$log" | head -20
        exit 1
    fi
    local others
    others=$(yq -N '.tests[] | select((.test_case_id | test("events-")) | not) | select(.status != "PASSED") | .test_case_id + " " + .status' "$@")
    if [ -n "$others" ]; then
        echo "::error::a test the forced miss does not touch did not pass ($log): $(tr '\n' ';' <<<"$others")"
        grep -n -A20 'failure_info' "$@" | head -60
        exit 1
    fi
    local n=0 c
    for rf in "$@"; do
        c=$(yq -N '[.tests[] | select((.test_case_id | test("events-")) | not)] | length' "$rf")
        n=$((n + ${c:-0}))
    done
    [ "$n" -ge 5 ] || { echo "::error::the reports hold $n of the five tests the forced miss does not touch ($log)"; exit 1; }
    if [ "$rc" -ne 1 ]; then
        echo "::error::keploy test exited $rc, want 1 (tests failed) ($log)"
        tail -40 "$log"
        exit 1
    fi
}
# require_plain_miss <log> <report>...: the text/plain events test reported
# its forced miss, whichever path ran it.
require_plain_miss() {
    local log=$1 rf
    shift
    for rf in "$@"; do
        if report_has_streaming_miss "$rf" 'events-plain'; then
            return 0
        fi
    done
    echo "::error::replay did NOT report the forced mock mismatch on the text/plain events test ($log)"
    echo "--- events-plain in the reports ---"
    grep -n -A30 'test_case_id: .*events-plain' "$@" | head -60
    echo "--- events-plain, mismatch and unmatched lines in $log ---"
    grep -iE "events-plain|events/plain|deferring streaming|mismatch|unmatched|no matching" "$log" | head -30
    echo "--- tail of $log ---"
    tail -40 "$log"
    exit 1
}
require_replay test_logs.txt "$REPLAY_RC" "${reports[@]}"
echo "✓ every streaming test reported its forced miss, every other test passed, keploy test exited 1"
require_plain_miss test_logs.txt "${reports[@]}"

# The same replay under the proven switch (KEPLOY_PROVEN=1, the proven model's
# staging gate). The agent then says when a read of a test's misses is
# incomplete (models.ErrUnmatchedCallsUnread), which leaves the test's set
# unjudged. Here nothing is lost: the forced misses must still reach their
# streaming tests (require_replay, as above), no test may be marked
# unmatched_calls_unread, and no set INTERNAL_ERR. The agent's own debug line
# is the proof the switch reached it as --proven.
rm -rf ./keploy/reports
sudo -E env PATH="$PATH" KEPLOY_PROVEN=1 "$REPLAY_BIN" test -c "./stream-app" --delay 0 --health-path /health \
    --debug --generateGithubActions=false --disable-tele >test_logs_proven.txt 2>&1 </dev/null
PROVEN_RC=$?
own
if grep -q "WARNING: DATA RACE" test_logs_proven.txt; then
    echo "::error::Race condition detected in the proven-switch replay"
    exit 1
fi
if ! grep -q "staging switch is on in this agent" test_logs_proven.txt; then
    echo "::error::the agent never said the proven switch is on: KEPLOY_PROVEN=1 did not reach it as --proven"
    grep -n "agent" test_logs_proven.txt | head -20
    exit 1
fi
shopt -s nullglob
proven_reports=(./keploy/reports/test-run-*/test-set-*-report.yaml)
shopt -u nullglob
if [ ${#proven_reports[@]} -eq 0 ]; then
    echo "::error::the proven-switch replay produced no test-set report"
    tail -40 test_logs_proven.txt
    exit 1
fi
for rf in "${proven_reports[@]}"; do
    if grep -q 'unmatched_calls_unread: true' "$rf" || grep -q '^status: INTERNAL_ERR' "$rf"; then
        echo "::error::$(basename "$rf"): a read of a test's misses was incomplete under the proven switch, though nothing was lost"
        grep -n 'unmatched_calls_unread\|^status\|failure_reason' "$rf" | head -20
        exit 1
    fi
done
require_replay test_logs_proven.txt "$PROVEN_RC" "${proven_reports[@]}"
require_plain_miss test_logs_proven.txt "${proven_reports[@]}"
echo "✓ under KEPLOY_PROVEN=1 the agent got the switch, every forced miss reached its test, and no read was incomplete"

echo "streaming mock-mismatch e2e passed: every streaming test reported the forced unmatched read of the store"
exit 0
