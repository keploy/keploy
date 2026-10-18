#!/usr/bin/env bash
# E2E: a response body of another kind than the recorded one fails its test
# (O-K, P005), with the proven switch on or off.
#
# The app's GET /n answers its dependency's body as JSON with a 200 whatever
# the dependency answered. Recorded with the dependency answering {"n":7};
# replayed with the dependency down:
#   1. Switch off, the recording as it is: the set passes (control).
#   2. Switch off, its mocks emptied: every dependency call gets keploy's own
#      plain-text "no matching mock" answer, which the app passes on with its
#      200, in place of {"n":7}. The set fails, and the report says the body
#      differs. Before O-K the body was skipped as non-JSON, its
#      Content-Length (7 against 53) excused, and the set PASSED.
#   3. KEPLOY_PROVEN=1, the same: the set fails the same way.
# GET /t answers its dependency's text ("count: 7") as text: a body of the
# recorded kind. With the switch off it is not compared and its
# Content-Length (8 against keploy's 53) is excused, as keploy documents:
# its test passes in 2. Under the switch it is compared byte for byte: its
# test fails in 3, its length not excused, and the recording as it is
# passes (4).
#
# GET /upgrade answers a 101 (Switching Protocols) and then speaks the
# protocol it switched to (an h2c preface), as a WebSocket or tunnel endpoint
# does. Its test, a 101 with no body as Keploy Enterprise's capture records one,
# passes with the switch off (1) and on (4): a 101's response ends at its
# headers. keploy test once read the next protocol's bytes as its body, and so
# failed every replayed upgrade whose app sent anything after its 101 once a
# body of another kind was compared (v3.6.129).
# keploy record does not make a test of an upgrade (its connection becomes a
# raw byte pump), so the case is the recorded GET /t's, rewritten.
#
# Readiness is the dependency's own "ready" line, the app's port while
# recording, and keploy's own probe of the app (--health-path) while
# replaying. The script stops only the processes it started. Ports: 8092
# (app), 9097 (dependency). Env: RECORD_BIN, REPLAY_BIN.
set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-$RECORD_BIN}"
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP=8092
DEP=9097
FAIL=0
fail() {
    echo "::error::$*"
    FAIL=$((FAIL + 1))
}

WORK="$(mktemp -d)"
echo "work dir: $WORK"
(cd "$SRC" && go build -o "$WORK/body-app" ./app) || { echo "::error::build failed"; exit 1; }
cd "$WORK" || exit 1

listening() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
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

for p in "$APP" "$DEP"; do
    if listening "$p"; then echo "::error::port $p is in use"; exit 1; fi
done

# The dependency: answers GET /n with {"n":7} and GET /t with "count: 7",
# says "ready" once it listens.
cat >dep.py <<'PY'
import http.server
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        b, t = (b'count: 7', "text/plain") if self.path == "/t" else (b'{"n":7}', "application/json")
        self.send_response(200); self.send_header("Content-Type", t)
        self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    def log_message(self, *a): pass
s = http.server.HTTPServer(("127.0.0.1", 9097), H)
print("ready", flush=True)
s.serve_forever()
PY
DEP_PID=""
REC_PID=""
cleanup() {
    [ -n "$REC_PID" ] && stop_pid "$REC_PID"
    [ -n "$DEP_PID" ] && kill "$DEP_PID" 2>/dev/null
}
trap cleanup EXIT
coproc DEPPROC { exec python3 -u dep.py; }
DEP_PID=$DEPPROC_PID
read -r line <&"${DEPPROC[0]}"
[ "$line" = ready ] || { echo "::error::the dependency did not start"; exit 1; }

# Record two GET /n and one GET /t, with the dependency up.
sudo -E env -u KEPLOY_PROVEN PATH="$PATH" "$RECORD_BIN" record -c "env DEP_URL=http://127.0.0.1:$DEP ./body-app" \
    --generateGithubActions=false --disable-tele >record.log 2>&1 </dev/null &
REC_PID=$!
wait_listen "$APP" "$REC_PID" || { echo "::error::the app did not start under keploy record"; tail -30 record.log; exit 1; }
for _ in 1 2; do
    out="$(curl -s "http://127.0.0.1:$APP/n")"
    [ "$out" = '{"n":7}' ] || fail "GET /n answered '$out' while recording"
done
out="$(curl -s "http://127.0.0.1:$APP/t")"
[ "$out" = 'count: 7' ] || fail "GET /t answered '$out' while recording"
wait_tests 3 "$REC_PID" || fail "keploy record wrote $(test_files) test cases, want 3"
stop_pid "$REC_PID"
REC_PID=""
own
grep -q '"n":7' keploy/test-set-0/mocks.yaml || { echo "::error::the dependency call was not recorded"; exit 1; }
t_case="$(grep -l 'url: .*/t$' keploy/test-set-0/tests/*.yaml | head -n1)"
[ -n "$t_case" ] || { echo "::error::no recorded test case of GET /t to make the upgrade case from"; exit 1; }
# Its window comes after every recorded call (the GET /t case's answer, the
# last recorded, plus 10 s): under the switch a test owns the calls its
# window holds, and the upgrade makes none.
up_at="$(date -u -d "$(yq -r '.spec.resp.timestamp' "$t_case") + 10 seconds" +%Y-%m-%dT%H:%M:%S.%NZ)" ||
    { echo "::error::could not read the GET /t case's answer time"; exit 1; }
UP_AT="$up_at" yq '.name = "test-upgrade" | .spec.req.url |= sub("/t$"; "/upgrade")
    | .spec.req.timestamp = strenv(UP_AT) | .spec.resp.timestamp = strenv(UP_AT)
    | .spec.req.header.Connection = "Upgrade" | .spec.req.header.Upgrade = "keploy-e2e"
    | .spec.resp.status_code = 101 | .spec.resp.status_message = "Switching Protocols"
    | .spec.resp.header = {"Connection": "Upgrade", "Upgrade": "keploy-e2e"} | .spec.resp.body = "" | del(.curl)' \
    "$t_case" >keploy/test-set-0/tests/test-upgrade.yaml || { echo "::error::could not write the upgrade case"; exit 1; }
kill "$DEP_PID" 2>/dev/null
wait "$DEP_PID" 2>/dev/null
DEP_PID=""

# replay <name>: keploy test of the recording; RC is its exit code. PROVEN=1
# turns the switch on.
replay() {
    local name="$1" before
    before="$(newest_run)"
    # shellcheck disable=SC2086
    sudo -E env -u KEPLOY_PROVEN PATH="$PATH" ${PROVEN:+KEPLOY_PROVEN=1} "$REPLAY_BIN" test -c "env DEP_URL=http://127.0.0.1:$DEP ./body-app" \
        --delay 1 --health-path /healthz --generateGithubActions=false --disable-tele >"$name.log" 2>&1 </dev/null
    RC=$?
    own
    if [ "$(newest_run)" = "$before" ] || [ ! -f "$(REPORT)" ]; then
        fail "$name: keploy test wrote no report (exit $RC)"
        tail -30 "$name.log"
    fi
}
expect_pass() {
    if [ "$RC" -ne 0 ] || [ "$(set_status)" != PASSED ]; then
        fail "$1: the set must pass (exit $RC, status $(set_status))"
    fi
}
# upgrade_test <field>: GET /upgrade's test's field in the report.
upgrade_test() { yq -r "[.tests[] | select(.req.url | test(\"/upgrade\$\"))][0] | $1" "$(REPORT)"; }
expect_upgrade_pass() {
    [ "$(upgrade_test .status)" = PASSED ] && return 0
    fail "$1: GET /upgrade's 101 must pass, its response ending at its headers (status $(upgrade_test .status))"
    yq -o=json -I=0 "[.tests[] | select(.req.url | test(\"/upgrade\$\"))][0] | {\"result\": .result, \"reason\": (.failure_info // .failureInfo // \"\")}" "$(REPORT)" | head -c 4000
    echo
}
# text_test <field>: GET /t's test's field in the report.
text_test() { yq -r "[.tests[] | select(.req.url | test(\"/t\$\"))][0] | $1" "$(REPORT)"; }
# expect_body_fail <step>: the set failed, and on a body the report says
# differs: keploy's "no matching mock" where {"n":7} was recorded.
expect_body_fail() {
    if [ "$RC" -eq 0 ] || [ "$(set_status)" != FAILED ]; then
        fail "$1: the set must fail (exit $RC, status $(set_status))"
    fi
    yq -e '[.tests[].result.body_result[]? | select(.normal == false)] | length > 0' "$(REPORT)" >/dev/null ||
        fail "$1: the report does not say the body differs"
    yq -e '[.tests[].result.body_result[]?.actual] | any_c(test("no matching mock"))' "$(REPORT)" >/dev/null ||
        fail "$1: the report does not show keploy's no-mock answer as the actual body"
}

PROVEN="" replay off-recorded
expect_pass "1 (switch off, the recording as it is)"
expect_upgrade_pass "1"

cp keploy/test-set-0/mocks.yaml mocks.yaml.saved
: >keploy/test-set-0/mocks.yaml
PROVEN="" replay off-no-mocks
expect_body_fail "2 (switch off, no mocks: keploy's plain-text answer in place of the JSON)"
[ "$(text_test .status)" = PASSED ] ||
    fail "2: GET /t's text body of the recorded kind must stay uncompared with the switch off, as documented (status $(text_test .status))"
PROVEN=1 replay proven-no-mocks
expect_body_fail "3 (KEPLOY_PROVEN=1, no mocks)"
[ "$(text_test .status)" = FAILED ] && [ "$(text_test '.result.body_result[0].normal')" = false ] ||
    fail "3: under the switch GET /t's text body that changed must fail on its body (status $(text_test .status))"
[ "$(text_test '[.result.headers_result[] | select(.expected.key == "Content-Length")][0].normal')" = false ] ||
    fail "3: under the switch GET /t's Content-Length (keploy's text in place of the recorded text) was excused"
cp mocks.yaml.saved keploy/test-set-0/mocks.yaml
PROVEN=1 replay proven-recorded
expect_pass "4 (KEPLOY_PROVEN=1, the recording as it is: a text body compared byte for byte)"
expect_upgrade_pass "4"

if [ "$FAIL" -eq 0 ]; then
    echo "body-kind e2e passed"
else
    echo "body-kind e2e FAILED ($FAIL)"
    for f in off-recorded off-no-mocks proven-no-mocks proven-recorded; do echo "== $f"; grep -aE 'status|no matching|PASSED|FAILED' "$f.log" | tail -5; done
fi
exit "$FAIL"
