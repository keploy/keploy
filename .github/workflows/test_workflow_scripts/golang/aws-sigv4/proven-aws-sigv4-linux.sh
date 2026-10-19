#!/usr/bin/env bash
# E2E: an AWS SDK's SigV4-signed calls under the proven switch (design v14
# row S1, founder decision 25), on an aws-sdk-go-v2 S3 app against LocalStack.
#
# Every SigV4 call carries a signature over the request and the time it was
# made, so a replay's signature is never the recording's. Under the switch
# keploy compares the Authorization header by its parts (the founder's
# ruling on SF-3): the scheme, the region, the service, the terminator and
# the signed headers exactly, the session token's header aside; the access
# key id, the date and the signature not at all, nor whether the credentials
# were temporary. A replay runs with the credentials of where it runs.
# x-amz-content-sha256 must be the hash of the body sent (or the recorded
# literal, such as UNSIGNED-PAYLOAD), and a presigned URL's query is read the
# same way (X-Amz-Credential, X-Amz-Signature, X-Amz-Security-Token).
#
# The app's GET /run puts an object, reads it back, and reads it through a
# presigned URL. Recorded once with LocalStack up; replayed with it stopped:
#   1. KEPLOY_PROVEN=1, unchanged: the set passes. (Before decision 25 every
#      call missed: the Authorization header was compared whole.)
#   2. KEPLOY_PROVEN=1, another region: the set fails, and the report names
#      header.authorization.region.
#   3. KEPLOY_PROVEN=1, another access key id (a CI's credentials): the set
#      passes. (Before the ruling it failed, naming
#      header.authorization.access_key_id.)
#   4. KEPLOY_PROVEN=1, temporary credentials (a session token, signed and in
#      the presigned URL) where the recording had static ones: the set
#      passes.
#   5. Switch off: the set passes, as on main.
#
# Readiness is LocalStack's "Ready." line, the app's port while recording, and
# keploy's own probe of the app (--health-path) while replaying. The script
# stops only the processes and containers it started. Ports: 8091 (app),
# 4566 (LocalStack). Env: RECORD_BIN, REPLAY_BIN; LOCALSTACK_IMAGE.
set -uo pipefail
# The proven switch, stated for every keploy this lane runs: the steps that
# say KEPLOY_PROVEN=1 run under it, and every other one with it off
# (KEPLOY_PROVEN=0, the default on Windows and macOS), as this lane always ran
# them. Unset, a Linux keploy runs the proven model.
export KEPLOY_PROVEN=0

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-$RECORD_BIN}"
IMAGE="${LOCALSTACK_IMAGE:-localstack/localstack:4.0.3}"
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP=8091
LS=4566
BUCKET=keploy-e2e
FAIL=0
fail() {
    echo "::error::$*"
    FAIL=$((FAIL + 1))
}

WORK="$(mktemp -d)"
echo "work dir: $WORK"
(cd "$SRC" && go build -o "$WORK/aws-app" ./app) || { echo "::error::build failed"; exit 1; }
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

LSID=""
# wait_ready: until LocalStack says "Ready."; fails at once when it exits.
wait_ready() {
    local line
    while IFS= read -r line; do
        case "$line" in *Ready.*) return 0 ;; esac
    done < <(docker logs -f "$LSID" 2>&1)
    return 1
}

ENVS="AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_REGION=us-east-1 S3_ENDPOINT=http://127.0.0.1:$LS S3_BUCKET=$BUCKET"

for p in "$APP" "$LS"; do
    if listening "$p"; then echo "::error::port $p is in use"; exit 1; fi
done
REC_PID=""
cleanup() {
    [ -n "$REC_PID" ] && stop_pid "$REC_PID"
    [ -n "$LSID" ] && docker rm -f "$LSID" >/dev/null 2>&1
}
trap cleanup EXIT

LSID=$(docker run -d --init -e SERVICES=s3 -p "127.0.0.1:$LS:4566" "$IMAGE") || { echo "::error::cannot start $IMAGE"; exit 1; }
wait_ready || { echo "::error::LocalStack exited before it was ready"; docker logs "$LSID" | tail -30; exit 1; }
curl -sf -X PUT "http://127.0.0.1:$LS/$BUCKET" >/dev/null || { echo "::error::cannot create the bucket"; exit 1; }

# Record two GET /run, with LocalStack up.
rm -rf keploy
# shellcheck disable=SC2086
sudo -E env PATH="$PATH" KEPLOY_PROVEN=0 "$RECORD_BIN" record -c "env $ENVS ./aws-app" \
    --generateGithubActions=false --disable-tele >record.log 2>&1 </dev/null &
REC_PID=$!
wait_listen "$APP" "$REC_PID" || { echo "::error::the app did not start under keploy record"; tail -30 record.log; exit 1; }
for k in a b; do
    out="$(curl -s "http://127.0.0.1:$APP/run?key=$k")"
    [ "$out" = "{\"get\":\"hello $k\",\"presigned\":\"hello $k\",\"status\":200}" ] || fail "GET /run?key=$k answered '$out' while recording"
done
wait_tests 2 "$REC_PID" || fail "keploy record wrote $(test_files) test cases, want 2"
stop_pid "$REC_PID"
REC_PID=""
own
grep -q 'AWS4-HMAC-SHA256 Credential=test/' keploy/test-set-0/mocks.yaml || { echo "::error::no SigV4-signed call was recorded"; exit 1; }
grep -q 'X-Amz-Signature=' keploy/test-set-0/mocks.yaml || { echo "::error::the presigned call was not recorded"; exit 1; }
docker rm -f "$LSID" >/dev/null 2>&1
LSID=""

# replay <name> <env...>: keploy test of the recording, LocalStack stopped,
# with the app's environment overridden by env; RC is its exit code.
replay() {
    local name="$1" before
    shift
    before="$(newest_run)"
    # shellcheck disable=SC2086
    sudo -E env PATH="$PATH" KEPLOY_PROVEN="${PROVEN:-0}" "$REPLAY_BIN" test -c "env $ENVS $* ./aws-app" \
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
        grep -nE 'status:|match_phase|path:|differs' "$(REPORT)" | head -40
    fi
}
# expect_miss <step> <field>: the set failed on a call that got no recorded
# answer, and the report names field as what differs.
expect_miss() {
    if [ "$RC" -eq 0 ] || [ "$(set_status)" != FAILED ]; then
        fail "$1: the set must fail (exit $RC, status $(set_status))"
    fi
    FIELD="$2" yq -e '[.tests[].failure_info.unmatched_calls[]?.field_diffs[]?.path] | any_c(. == strenv(FIELD))' "$(REPORT)" >/dev/null ||
        { fail "$1: the report's field diffs do not name $2"; grep -nE 'path:|differs' "$(REPORT)" | head -20; }
    FIELD="$2" yq -e '[.tests[].failure_info.unmatched_calls[]?.differs[]?] | any_c(. == strenv(FIELD))' "$(REPORT)" >/dev/null ||
        fail "$1: the report's differs do not name $2"
}

PROVEN=1 replay proven-same
expect_pass "1 (KEPLOY_PROVEN=1, unchanged)"
PROVEN=1 replay proven-region AWS_REGION=eu-west-1
expect_miss "2 (KEPLOY_PROVEN=1, another region)" header.authorization.region
PROVEN=1 replay proven-keyid AWS_ACCESS_KEY_ID=other
expect_pass "3 (KEPLOY_PROVEN=1, another access key id)"
PROVEN=1 replay proven-session AWS_ACCESS_KEY_ID=ASIATEMPORARY AWS_SESSION_TOKEN=session-token-1
expect_pass "4 (KEPLOY_PROVEN=1, temporary credentials)"
PROVEN=0 replay off-same
expect_pass "5 (switch off)"

if [ "$FAIL" -eq 0 ]; then
    echo "proven AWS SigV4 e2e passed"
else
    echo "proven AWS SigV4 e2e FAILED ($FAIL)"
fi
exit "$FAIL"
