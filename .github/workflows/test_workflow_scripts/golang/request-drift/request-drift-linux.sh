#!/bin/bash
# Request drift on served mocks, and strict request matching.
#
# The app sends {"amount": AMOUNT} to a payments service and answers
# {"order":"ok"} whatever the charge returns. Recorded with AMOUNT=100,
# replayed with AMOUNT=250:
#   - lenient (default): the recorded mock is still served, so the test
#     passes — and the report must say the request changed
#     (failure_info.drifted_calls: body.amount 100 -> 250);
#   - --mock-noise-strict: the mock is refused, and because the app ignores
#     its dependency the response still matches — the test must FAIL anyway.
# Before the fix the lenient run left no trace of the change and the strict
# run passed.

set -uo pipefail
set +e

cleanup() {
    # Exact process names (-x), never -f: a full-command-line match also hits
    # this script and its sudo parent, and a substring match would take down
    # any other keploy-named process on the machine.
    sudo pkill -9 -x keploy 2>/dev/null || true
    pkill -9 -x payments-svc 2>/dev/null || true
    pkill -9 -x order-app 2>/dev/null || true
    sleep 1
}
trap cleanup EXIT

fail() { echo "::error::$*"; exit 1; }

cd "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/golang/request-drift" || fail "no sample dir"
go build -o order-app ./app && go build -o payments-svc ./payments || fail "build failed"
sudo rm -rf keploy keploy.yml

./payments-svc &
for _ in $(seq 1 30); do curl -s -o /dev/null -X POST localhost:9099/charge && break; sleep 1; done

echo "=== record (AMOUNT=100) ==="
sudo -E env PATH="$PATH" "$RECORD_BIN" record -c "env AMOUNT=100 PAYMENTS_URL=http://127.0.0.1:9099 ./order-app" --disable-ansi > record.log 2>&1 &
for _ in $(seq 1 60); do curl -s -o /dev/null localhost:8090/order && break; sleep 1; done
for _ in 1 2; do curl -s localhost:8090/order; echo; sleep 1; done
sleep 5
sudo pkill -INT -x keploy
for _ in $(seq 1 30); do pgrep -x keploy >/dev/null || break; sleep 1; done
pkill -x payments-svc  # replay must be served from mocks
ls keploy/test-set-0/tests/*.yaml >/dev/null 2>&1 || { cat record.log; fail "nothing was recorded"; }

report_count() { sudo ls -d keploy/reports/test-run-* 2>/dev/null | wc -l; }
latest_report() { sudo ls -td keploy/reports/test-run-* | head -1; }
# A replay that writes no report must fail here, not pass on an older report.
expect_new_report() { [ "$(report_count)" -gt "$1" ] || fail "the replay wrote no new report"; }

echo "=== replay, lenient (AMOUNT=250) ==="
before=$(report_count)
sudo -E env PATH="$PATH" "$RECORD_BIN" test -c "env AMOUNT=250 PAYMENTS_URL=http://127.0.0.1:9099 ./order-app" --delay 5 --disable-ansi > test-lenient.log 2>&1
lenient=$?
expect_new_report "$before"
report="$(latest_report)/test-set-0-report.yaml"
sudo cat "$report" > lenient-report.yaml
[ "$lenient" -eq 0 ] || { tail -50 test-lenient.log; fail "lenient replay should pass (the mock is still served), exit $lenient"; }
grep -q 'drifted_calls:' lenient-report.yaml || fail "lenient report has no drifted_calls: the changed request left no trace"
grep -A12 'drifted_calls:' lenient-report.yaml | grep -q 'path: body.amount' || fail "drifted_calls should name body.amount"
grep -A12 'drifted_calls:' lenient-report.yaml | grep -q 'expected: "100"' || fail "drifted_calls should carry the recorded value 100"
grep -A12 'drifted_calls:' lenient-report.yaml | grep -q 'actual: "250"' || fail "drifted_calls should carry the live value 250"

echo "=== replay, strict (AMOUNT=250) ==="
before=$(report_count)
sudo -E env PATH="$PATH" "$RECORD_BIN" test -c "env AMOUNT=250 PAYMENTS_URL=http://127.0.0.1:9099 ./order-app" --delay 5 --disable-ansi --mock-noise-strict > test-strict.log 2>&1
strict=$?
expect_new_report "$before"
report="$(latest_report)/test-set-0-report.yaml"
sudo cat "$report" > strict-report.yaml
[ "$strict" -ne 0 ] || { tail -50 test-strict.log; fail "strict replay passed: a refused (changed) dependency request must fail the test even when the response matches"; }
grep -q '^status: FAILED' strict-report.yaml || fail "strict report should be FAILED"
grep -q 'match_phase: strict_noise_reject' strict-report.yaml || fail "strict report should carry the strict rejection"

echo "PASS: drift reported on the lenient replay, strict replay failed"
