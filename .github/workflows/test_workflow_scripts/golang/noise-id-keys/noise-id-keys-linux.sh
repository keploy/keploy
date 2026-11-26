#!/usr/bin/env bash
# E2E: a time field under a map keyed by ids is written into its test case
# once, naming every id key ("{id}"), and keploy test honours that entry; a
# field that is not a time stays asserted under every id.
#
# The app (app/main.go) answers GET /options with forty options keyed by UUIDs
# derived from the query, each with a departAt read from its clock and a fixed
# label; GET /holds with two options: one with a hold read from its clock, the
# other with a fixed holdReason; GET /slots with three options, two holding a
# hold read from its clock and one a price, which also gets a hold under
# another LABEL; and GET /seen with a time per id, for a third id too under
# another LABEL. Recorded with the proven switch off, where keploy record
# writes a value that reads as a time as noise, by its shape. Then:
#   1. The /options case's noise names body.options.{id}.departAt and no
#      option id. The /holds case's noise names the one option's hold and no
#      {id} entry: "options.{id}.hold" would also silence the other option's
#      holdReason, which is not a time. The /slots case's noise is
#      body.options.{id}.hold, the /seen case's body.lastSeen.{id}.
#   2. keploy test, the same LABEL: the set passes; every time moved.
#   3. keploy test, another LABEL: the /options and /holds tests fail; the
#      label under every id and the holdReason are still compared. A key the
#      recording lacks (the hold the third option gained, the third id) is a
#      difference too: /slots and /seen fail, or pass only by keploy test's
#      default pass of a response that gained fields alone, which names
#      them. An {id} entry excusing the key would pass them with neither.
# Before {id} entries, step 1 found forty per-id departAt entries in the
# /options case.
#
# Readiness is the app's port while recording, and keploy's own probe of the
# app (--health-path) while replaying. The script stops only the processes it
# started. Port: 8093 (app). Env: RECORD_BIN, REPLAY_BIN.
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true
# shellcheck source=../../sudo_env.sh
source "$(dirname "${BASH_SOURCE[0]}")/../../sudo_env.sh" || exit 1

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-$RECORD_BIN}"
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP=8093
FAIL=0
fail() {
    echo "::error::$*"
    FAIL=$((FAIL + 1))
}

WORK="$(mktemp -d)"
echo "work dir: $WORK"
(cd "$SRC" && go build -o "$WORK/idkeys-app" ./app) || { echo "::error::build failed"; exit 1; }
cd "$WORK" || exit 1

listening() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
# pid_running PID: whether PID still runs, by its state in /proc (keploy runs
# under sudo, and kill -0 from this user fails on a sudo that is alive).
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
newest_run() { ls -1d keploy/reports/test-run-* 2>/dev/null | sort -t- -k3 -n | tail -n1; }
REPORT() { echo "$(newest_run)/test-set-0-report.yaml"; }
set_status() { grep -m1 '^status:' "$(REPORT)" 2>/dev/null | awk '{print $2}'; }
# case_test <path> <field>: the field of the report's test of GET <path>.
case_test() { yq -r "[.tests[] | select(.req.url | test(\"$1\"))][0] | $2" "$(REPORT)"; }

if listening "$APP"; then echo "::error::port $APP is in use"; exit 1; fi

REC_PID=""
cleanup() { [ -n "$REC_PID" ] && stop_pid "$REC_PID"; }
trap cleanup EXIT

sudo env -u KEPLOY_PROVEN PATH="$PATH" "${SUDO_ENV[@]}" "$RECORD_BIN" record -c "env LABEL=fixed ./idkeys-app" \
    --generateGithubActions=false --disable-tele >record.log 2>&1 </dev/null &
REC_PID=$!
wait_listen "$APP" "$REC_PID" || { echo "::error::the app did not start under keploy record"; tail -30 record.log; exit 1; }
n="$(curl -s "http://127.0.0.1:$APP/options?q=lisbon" | yq -p=json -r '.options | length')"
[ "$n" = 40 ] || fail "GET /options answered $n options while recording, want 40"
n="$(curl -s "http://127.0.0.1:$APP/holds?q=lisbon" | yq -p=json -r '.options | length')"
[ "$n" = 2 ] || fail "GET /holds answered $n options while recording, want 2"
n="$(curl -s "http://127.0.0.1:$APP/slots?q=lisbon" | yq -p=json -r '.options | length')"
[ "$n" = 3 ] || fail "GET /slots answered $n options while recording, want 3"
n="$(curl -s "http://127.0.0.1:$APP/seen?q=lisbon" | yq -p=json -r '.lastSeen | length')"
[ "$n" = 2 ] || fail "GET /seen answered $n ids while recording, want 2"
for p in /version /about /apps/7/version; do
    [ "$(curl -s "http://127.0.0.1:$APP$p" | yq -p=json -r '.version')" = fixed ] || fail "GET $p did not answer the label while recording"
done
wait_tests 7 "$REC_PID" || fail "keploy record wrote $(test_files) test cases, want 7"
stop_pid "$REC_PID"
REC_PID=""
own

# 1. The noise each case was written with.
opt_case="$(grep -l 'url: .*/options' keploy/test-set-0/tests/*.yaml | head -n1)"
holds_case="$(grep -l 'url: .*/holds' keploy/test-set-0/tests/*.yaml | head -n1)"
slots_case="$(grep -l 'url: .*/slots' keploy/test-set-0/tests/*.yaml | head -n1)"
seen_case="$(grep -l 'url: .*/seen' keploy/test-set-0/tests/*.yaml | head -n1)"
[ -n "$opt_case" ] && [ -n "$holds_case" ] && [ -n "$slots_case" ] && [ -n "$seen_case" ] ||
    { echo "::error::no recorded test case of GET /options, /holds, /slots or /seen"; exit 1; }
noise() { yq -r '.spec.assertions.noise // {} | keys | .[]' "$1"; }
per_id='^body\.options\.[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.'
echo "== /options noise: $(noise "$opt_case" | tr '\n' ' ')"
echo "== /holds noise: $(noise "$holds_case" | tr '\n' ' ')"
echo "== /slots noise: $(noise "$slots_case" | tr '\n' ' ')"
echo "== /seen noise: $(noise "$seen_case" | tr '\n' ' ')"
noise "$opt_case" | grep -qxF 'body.options.{id}.departAt' ||
    fail "1: the /options case does not name body.options.{id}.departAt"
n="$(noise "$opt_case" | grep -cE "$per_id")"
[ "$n" -eq 0 ] || fail "1: the /options case names $n option ids, want none"
n="$(noise "$holds_case" | grep -cE "${per_id}hold\$")"
[ "$n" -eq 1 ] || fail "1: the /holds case names $n per-id hold entries, want 1"
n="$(noise "$holds_case" | grep -cF '{id}')"
[ "$n" -eq 0 ] || fail "1: the /holds case has $n {id} entries, which would silence the other option's holdReason"
body_noise() { noise "$1" | grep '^body\.' | tr '\n' ' '; }
[ "$(body_noise "$slots_case")" = 'body.options.{id}.hold ' ] ||
    fail "1: the /slots case's body noise is not body.options.{id}.hold alone"
[ "$(body_noise "$seen_case")" = 'body.lastSeen.{id} ' ] ||
    fail "1: the /seen case's body noise is not body.lastSeen.{id} alone"

# replay <name> <label>: keploy test of the recording, the app answering
# label; RC is its exit code.
replay() {
    local before
    before="$(newest_run)"
    sudo env -u KEPLOY_PROVEN PATH="$PATH" "${SUDO_ENV[@]}" "$REPLAY_BIN" test -c "env LABEL=$2 ./idkeys-app" \
        --delay 1 --health-path /healthz --generateGithubActions=false --disable-tele >"$1.log" 2>&1 </dev/null
    RC=$?
    own
    if [ "$(newest_run)" = "$before" ] || [ ! -f "$(REPORT)" ]; then
        fail "$1: keploy test wrote no report (exit $RC)"
        tail -30 "$1.log"
    fi
}

cat >keploy.yml <<'CFG'
test:
    globalNoise:
        endpoints:
            - method: GET
              path: /version
              body: {"version": []}
              header: {"X-Build": []}
            - path: /apps/{id}/version
              body: {"version": []}
              header: {"X-Build": []}
CFG

# 2. Every time moved, nothing else did.
sleep 1
replay same fixed
if [ "$RC" -ne 0 ] || [ "$(set_status)" != PASSED ]; then
    fail "2: the set must pass with the same label (exit $RC, status $(set_status))"
    yq -o=json -I=0 '[.tests[] | {"url": .req.url, "status": .status, "body": [.result.body_result[]? | select(.normal == false)]}]' "$(REPORT)" | head -c 4000
    echo
fi

# 3. Another label: the fields that are not times are still compared.
replay changed other
[ "$RC" -ne 0 ] && [ "$(set_status)" = FAILED ] || fail "3: the set must fail with another label (exit $RC, status $(set_status))"
[ "$(case_test '/options' .status)" = FAILED ] ||
    fail "3: GET /options passed with another label: the label under an id key is no longer compared"
[ "$(case_test '/holds' .status)" = FAILED ] ||
    fail "3: GET /holds passed with another holdReason: an entry for the hold silences it"
[ "$(case_test ':8093/version$' .status)" = PASSED ] ||
    fail "3: GET /version failed with another label: its rule in test.globalNoise.endpoints did not ignore version and X-Build"
[ "$(case_test '/apps/7/version$' .status)" = PASSED ] ||
    fail "3: GET /apps/7/version failed with another label: the rule for /apps/{id}/version did not apply"
[ "$(case_test '/about$' .status)" = FAILED ] ||
    fail "3: GET /about passed with another label: the rule for GET /version ignored version on another endpoint"
echo "== /version: $(case_test ':8093/version$' .status), /apps/7/version: $(case_test '/apps/7/version$' .status), /about: $(case_test '/about$' .status)"
# added_reported <path> <case file>: the test of GET <path> failed, or keploy
# test passed it as a response that only gained fields, naming it.
added_reported() {
    local name
    [ "$(case_test "$1" .status)" = FAILED ] && return 0
    name="$(yq -r '.name' "$2")"
    grep -a 'Additive response schema change detected' changed.log | grep -qw -- "$name"
}
added_reported '/slots' "$slots_case" ||
    fail "3: GET /slots passed when an option gained a hold, and not as gained fields: an {id} entry excused a key the recording lacks"
added_reported '/seen' "$seen_case" ||
    fail "3: GET /seen passed with a new id, and not as gained fields: an {id} entry excused a key the recording lacks"
echo "== /slots: $(case_test '/slots' .status), /seen: $(case_test '/seen' .status); passed as gained fields: $(grep -ac 'Additive response schema change detected' changed.log)"

if [ "$FAIL" -eq 0 ]; then
    echo "noise-id-keys e2e passed"
else
    echo "noise-id-keys e2e FAILED ($FAIL)"
    for f in record same changed; do echo "== $f"; grep -aE 'status|PASSED|FAILED|error' "$f.log" 2>/dev/null | tail -5; done
fi
exit "$FAIL"
