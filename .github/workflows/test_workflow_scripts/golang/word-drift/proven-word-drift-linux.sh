#!/usr/bin/env bash
# E2E: a request text one word of which the application makes fresh on
# every call, with no source keploy can follow (a UUID), is learned by its
# words under the proven switch: the changed word is left unchecked and the
# text's other words are still compared, so a replay whose other words
# changed is refused, never served.
#
# The app saves a label on GET /label with PUT /labels/1
# {"label":"Lead <uuid> for ci"}, a UUID made fresh on every call. It
# answers {"label":"ok"} only when the labels service answered as recorded
# ({"label":"unsaved"} otherwise), so a test passes only where its call was
# served from its recording; its response never shows what it sent, so only
# the call can show a change.
#
# Recorded under KEPLOY_PROVEN=1. An OSS keploy has no learn round (only
# enterprise's record with its full round learns), so what the learn step
# learns of the call is learned by the HTTP parser's own learn step and
# written onto the mock by the code that writes what record's full round
# learned (learnwords/main.go, built from the keploy module as one file; its
# header lists what of the round it does not run). It sends the recorded
# call again with its UUID made fresh, as the app makes it. So a change to
# what the learn step learns of such a text, or to how it is written,
# changes what this lane replays. The learned noise of body.label must be
# its words pattern naming word 1 alone (^keploy:words 1$), never the whole
# text. Replayed with the labels service down:
#   1. KEPLOY_PROVEN=1, a fresh UUID: the set passes (the changed word is
#      left unchecked).
#   2. KEPLOY_PROVEN=1, WORDS_OTHER=1, which sends "Evil <uuid> for
#      attacker": the same skeleton, its other words changed. The set fails,
#      its report naming body.label as what the call differs in. A learn
#      step that left the whole text unchecked serves it: a false green.
#   3. No data race in any log.
#
# Readiness is the app's listening port while recording and keploy's own
# probe of the app (--health-path) while replaying. The script stops only
# the processes it started. Ports: 8191 (app), 9191 (labels), as the app
# has them. Env: RECORD_BIN, REPLAY_BIN.
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
# shellcheck source=../../sudo_env.sh
source "$(dirname "${BASH_SOURCE[0]}")/../../sudo_env.sh" || exit 1

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-$RECORD_BIN}"
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP=8191
DEP=9191
# The keploy module's root, from which learn's own code runs.
ROOT="$(cd "$SRC/../../../../.." && pwd)"
FAIL=0
fail() {
    echo "::error::$*"
    FAIL=$((FAIL + 1))
}

WORK="$(mktemp -d)"
echo "work dir: $WORK"
(cd "$SRC" && go build -o "$WORK/label-app" ./app && go build -o "$WORK/labels-svc" ./labels) || { echo "::error::build failed"; exit 1; }
(cd "$ROOT" && go build -o "$WORK/learnwords" ./.github/workflows/test_workflow_scripts/golang/word-drift/learnwords/main.go) ||
    { echo "::error::cannot build the learn step's writer (learnwords) from the keploy module at $ROOT"; exit 1; }
cd "$WORK" || exit 1

listening() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
# pid_running PID: whether PID still runs, by its state in /proc (keploy runs
# under sudo, so kill -0 from this user fails on a sudo that is alive).
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
# wait_tests <n> <pid>: until n test cases are written; fails at once when
# keploy (pid) has exited.
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

# replay <name> [env...]: keploy test of the recording; RC is its exit code.
replay() {
    local name="$1" before
    shift
    before="$(newest_run)"
    # The switch is on only where a step asks for it, whatever the job's
    # environment holds.
    sudo env -u KEPLOY_PROVEN PATH="$PATH" "${SUDO_ENV[@]}" "$@" "$REPLAY_BIN" test -c "env LABELS_URL=http://127.0.0.1:$DEP ${WORDS_ENV:-} ./label-app" \
        --delay 1 --health-path /healthz --generateGithubActions=false --disable-tele >"$name.log" 2>&1 </dev/null
    RC=$?
    verify_ran "$(proven_of "$@")" "$name.log" || FAIL=$((FAIL + 1))
    own
    if [ "$(newest_run)" = "$before" ] || [ ! -f "$(REPORT)" ]; then
        fail "$name: keploy test wrote no report (exit $RC)"
        tail -30 "$name.log"
    fi
}
# named <field>: whether the report names field as one a call differs in,
# with a reason or without.
named() { FIELD="$1" yq -e '[.tests[].failure_info.unmatched_calls[]? | (.differs[]?, .field_reasons[]?.field)] | any_c(. == strenv(FIELD))' "$(REPORT)" >/dev/null 2>&1; }

for p in "$APP" "$DEP"; do
    if listening "$p"; then echo "::error::port $p is in use"; exit 1; fi
done
REC_PID=""
DEP_PID=""
cleanup() {
    [ -n "$REC_PID" ] && stop_pid "$REC_PID"
    [ -n "$DEP_PID" ] && kill "$DEP_PID" 2>/dev/null
    return 0
}
trap cleanup EXIT

echo "== record (KEPLOY_PROVEN=1) =="
./labels-svc >labels.log 2>&1 &
DEP_PID=$!
wait_listen "$DEP" "$DEP_PID" || { echo "::error::the labels service did not start"; exit 1; }
sudo env PATH="$PATH" "${SUDO_ENV[@]}" KEPLOY_PROVEN=1 "$RECORD_BIN" record -c "env LABELS_URL=http://127.0.0.1:$DEP ./label-app" \
    --generateGithubActions=false --disable-tele >record.log 2>&1 </dev/null &
REC_PID=$!
wait_listen "$APP" "$REC_PID" || { echo "::error::the app did not start under keploy record"; tail -30 record.log; exit 1; }
out="$(curl -s "http://127.0.0.1:$APP/label")"
[ "$out" = '{"label":"ok"}' ] || fail "GET /label answered '$out' while recording"
wait_tests 1 "$REC_PID" || fail "keploy record wrote $(test_files) test cases, want 1"
stop_pid "$REC_PID"
REC_PID=""
kill "$DEP_PID" 2>/dev/null
wait "$DEP_PID" 2>/dev/null
DEP_PID=""
own
verify_ran 1 record.log || FAIL=$((FAIL + 1))
MOCKS=keploy/test-set-0/mocks.yaml
[ "$(grep -c '^kind: Http$' "$MOCKS" 2>/dev/null)" -ge 1 ] || { echo "::error::the labels call was not recorded"; exit 1; }
grep -q 'Lead [0-9a-f-]\{36\} for ci' "$MOCKS" || { echo "::error::the recorded call holds no label 'Lead <uuid> for ci'"; exit 1; }

# OSS learns nothing at record: noise record wrote at body.label would be
# met with what learnwords writes below, hiding what it learned.
grep -q 'body.label' "$MOCKS" && fail "keploy record wrote noise at body.label, which OSS never learns: what learnwords writes below would be met with it"
echo "== learn (the HTTP parser's learn step, the call's UUID made fresh) =="
KEPLOY_PROVEN=1 ./learnwords -path keploy -set test-set-0 -place body.label -word 1 ||
    { echo "::error::learnwords learned nothing (its reason above)"; exit 1; }
# What the writer put on the mock: body.label under the mock's noise.words,
# naming the UUID's word alone, and never under noise.req, which leaves the
# whole text unchecked.
yq 'select(.kind == "Http") | .noise' "$MOCKS"
words="$(yq -N 'select(.kind == "Http") | .noise.words["body.label"] // [] | join(",")' "$MOCKS")"
whole="$(yq -N 'select(.kind == "Http") | .noise.req // [] | map(select(. == "body.label")) | length' "$MOCKS")"
[ "$whole" = 0 ] || fail "the learn step wrote body.label as noise whole (noise.req): the text's other words are left unchecked"
[ "$words" = 1 ] || fail "the learn step wrote body.label's unchecked words as '$words', want word 1 alone (the UUID)"

echo "== 1. KEPLOY_PROVEN=1, a fresh UUID: passes =="
WORDS_ENV="" replay fresh KEPLOY_PROVEN=1
if [ "$RC" -ne 0 ] || [ "$(set_status)" != PASSED ]; then
    fail "step 1: the set must pass (exit $RC, status $(set_status))"
    grep -nE 'status:|field:|why:|differs|path:' "$(REPORT)" | head -30
fi

echo "== 2. KEPLOY_PROVEN=1, WORDS_OTHER=1 (Evil <uuid> for attacker): fails naming body.label =="
WORDS_ENV="WORDS_OTHER=1" replay other KEPLOY_PROVEN=1
if [ "$RC" -eq 0 ] || [ "$(set_status)" != FAILED ]; then
    fail "step 2: the set must fail (exit $RC, status $(set_status)): a label whose other words changed was served"
fi
named body.label || fail "step 2: the report does not name body.label as what the call differs in"

echo "== 3. no data race =="
for f in record.log fresh.log other.log; do
    grep -q 'WARNING: DATA RACE' "$f" 2>/dev/null && fail "data race in $f"
done

if [ "$FAIL" -ne 0 ]; then
    echo "word-drift e2e: $FAIL check(s) failed"
    exit 1
fi
echo "word-drift e2e: ok"
