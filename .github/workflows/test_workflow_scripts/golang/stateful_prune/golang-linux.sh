#!/usr/bin/env bash
# E2E (decision 27): keploy test --remove-unused-mocks keeps a stateful
# dependency's recordings whole.
#
# The app (./app, role tick) answers GET /tick with what a counter (role
# counter, outside keploy) answers GET /next: the same request every time, a
# different answer each time (n = 1, 2, ...). Eight GET /tick are recorded.
#
#   1. Test 6 is made to fail (its recorded answer edited), and keploy test
#      --remove-unused-mocks runs: the failed test's mocks are not kept.
#   2. Test 6 is put back, and keploy test runs again: every test passes, each
#      answered with its own recording.
#
# A recording that states no lifetimes (a released keploy's) replays its
# recordings of GET /next as one sequence, across tests, in record order: the
# prune used to drop the failed test's, and every later test was answered with
# the recording after its own (test 6 got n=7, test 7 n=8). The prune now
# keeps such a sequence whole. A recording that states lifetimes (this
# build's) serves each test its own recordings: the failed test's are pruned,
# and the later tests are still answered with their own.
#
# Readiness is event-driven: the counter's port, the app's port, the test
# files keploy writes, the report each keploy test writes. Ports: 8097 (app),
# 9097 (counter). Env: RECORD_BIN, REPLAY_BIN; STRIP_LIFETIMES=true removes the
# recording's stated lifetimes, so it states none whatever the recorder (as
# one made before lifetimes were stated); WORK (default: a new temporary
# directory).
set -uo pipefail
# This lane guards the switch-off prune (--remove-unused-mocks), which the
# proven switch ignores (design v14 U18). So it runs keploy with the switch
# off (KEPLOY_PROVEN=0), where that still works: the open-source build's
# default, and keploy enterprise's on Windows and macOS. Unset, keploy
# enterprise runs the proven model on Linux.
export KEPLOY_PROVEN=0

RECORD_BIN="${RECORD_BIN:-keploy}"
REPLAY_BIN="${REPLAY_BIN:-$RECORD_BIN}"
SRC="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
APP=8097
DEP=9097
FAIL=0
fail() {
    echo "::error::$*"
    FAIL=$((FAIL + 1))
}

WORK="${WORK:-$(mktemp -d)}"
mkdir -p "$WORK"
echo "work dir: $WORK"
(cd "$SRC" && go build -o "$WORK/app" ./app) || { echo "::error::build failed"; exit 1; }
cd "$WORK" || exit 1

listening() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
wait_listen() {
    until listening "$1"; do
        kill -0 "$2" 2>/dev/null || return 1
        sleep 0.2
    done
}
test_files() { find keploy/test-set-0/tests -maxdepth 1 -name '*.yaml' -size +0c 2>/dev/null | wc -l; }
own() { sudo chown -R "$(id -u):$(id -g)" . 2>/dev/null || true; }
stop_pid() {
    local sudo_pid="$1" child
    child="$(pgrep -P "$sudo_pid" 2>/dev/null | head -n1)"
    [ -n "$child" ] && sudo kill -INT "$child" 2>/dev/null
    wait "$sudo_pid" 2>/dev/null
}
newest_run() { ls -1d keploy/reports/test-run-* 2>/dev/null | sort -t- -k3 -n | tail -n1; }
REPORT() { echo "$(newest_run)/test-set-0-report.yaml"; }

DEP_PID=""
REC_PID=""
cleanup() {
    [ -n "$REC_PID" ] && stop_pid "$REC_PID"
    [ -n "$DEP_PID" ] && kill "$DEP_PID" 2>/dev/null
}
trap cleanup EXIT

for p in "$APP" "$DEP"; do
    if listening "$p"; then echo "::error::port $p is in use"; exit 1; fi
done

# tick_tests: the recorded GET /tick test cases, in record order (by the
# request's timestamp), one file per line.
tick_tests() {
    for f in keploy/test-set-0/tests/*.yaml; do
        [ "$(yq -N '.spec.req.url' "$f")" = "http://127.0.0.1:$APP/tick" ] || continue
        echo "$(yq -N '.spec.req.timestamp' "$f") $f"
    done | sort | cut -d' ' -f2
}
# results: test id, status and answered body of each test of the newest run,
# tab-separated as they are (@tsv would quote a body holding quotes).
results() {
    local tab=$'\t'
    yq -N '.tests[] | [.test_case_id, .status, (.result.body_result[0].actual // "" | sub("\n"; ""))] | join("'"$tab"'")' "$(REPORT)" 2>/dev/null | sort
}
# run_test <log> [flags...]: keploy test of the recording; it must write a
# new report, or the checks would read the previous run's.
run_test() {
    local log="$1" before
    shift
    before="$(newest_run)"
    sudo -E env PATH="$PATH" "$REPLAY_BIN" test -c "$WORK/app tick" --delay 1 --health-path /healthz --api-timeout 30 \
        "$@" --generateGithubActions=false --disable-tele >"$log" 2>&1 </dev/null
    own
    if [ "$(newest_run)" = "$before" ] || [ ! -f "$(REPORT)" ]; then
        echo "::error::keploy test ($log) wrote no report"
        tail -40 "$log"
        exit 1
    fi
}

"$WORK/app" counter >counter.log 2>&1 &
DEP_PID=$!
wait_listen "$DEP" "$DEP_PID" || { echo "::error::the counter did not start"; exit 1; }
sudo -E env PATH="$PATH" "$RECORD_BIN" record -c "$WORK/app tick" --generateGithubActions=false --disable-tele >record.log 2>&1 </dev/null &
REC_PID=$!
wait_listen "$APP" "$REC_PID" || { echo "::error::the app did not start under keploy record"; tail -30 record.log; exit 1; }
for i in 1 2 3 4 5 6 7 8; do
    got=$(curl -s "http://127.0.0.1:$APP/tick")
    [ "$got" = "{\"n\":$i}" ] || fail "GET /tick $i answered $got while recording, want n=$i"
done
until [ "$(test_files)" -ge 8 ]; do
    kill -0 "$REC_PID" 2>/dev/null || break
    sleep 0.2
done
stop_pid "$REC_PID"
REC_PID=""
kill "$DEP_PID" 2>/dev/null
wait "$DEP_PID" 2>/dev/null
DEP_PID=""
own
mapfile -t TICKS < <(tick_tests)
[ "${#TICKS[@]}" -eq 8 ] || { echo "::error::recorded ${#TICKS[@]} GET /tick test cases, want 8"; exit 1; }
RECORDED=$(grep -c '^kind: Http' keploy/test-set-0/mocks.yaml)
[ "$RECORDED" -eq 8 ] || { echo "::error::recorded $RECORDED mocks of GET /next, want 8"; exit 1; }
if [ "${STRIP_LIFETIMES:-}" = true ]; then
    yq -i 'del(.spec.metadata.lifetime)' keploy/test-set-0/mocks.yaml
fi
STATES=$(grep -c "lifetime: perTest" keploy/test-set-0/mocks.yaml)
echo "recorded: 8 test cases; mocks stating a per-test lifetime: $STATES"
if [ "${STRIP_LIFETIMES:-}" = true ] && [ "$STATES" -ne 0 ]; then
    echo "::error::$STATES mocks still state a per-test lifetime after their lifetimes were removed: this cell would test the per-test path, not a recording that states none"
    exit 1
fi

# 1. Test 6 fails; the prune runs.
T6="${TICKS[5]}"
cp "$T6" t6.orig
sed -i 's/\\"n\\":6/\\"n\\":999/; s/"n":6/"n":999/' "$T6"
grep -qE 'n(\\)?":999' "$T6" || { echo "::error::could not make test 6 fail: $T6"; exit 1; }
run_test prune.log --remove-unused-mocks --debug
echo "the pruning run:"; results | sed 's/^/  /'
t6=$(basename "$T6" .yaml)
[ "$(results | awk -F'\t' -v t="$t6" '$1 == t {print $2}')" = FAILED ] || fail "test 6 ($t6) did not fail in the pruning run: nothing was pruned for it"
KEPT=$(grep -c '^kind: Http' keploy/test-set-0/mocks.yaml)
echo "mocks after the prune: $KEPT of 8"
case "$STATES" in
0)
    [ "$KEPT" -eq 8 ] || fail "the prune kept $KEPT of the 8 recordings of the reusable sequence, want all 8"
    grep -qE '"keptAsDependency": ?1\b' prune.log || fail "the prune did not keep the failed test's recording as a member of its sequence"
    ;;
*) [ "$KEPT" -eq 7 ] || fail "the prune kept $KEPT of the 8 per-test recordings, want 7: did it run?" ;;
esac

# 2. Test 6 back; every later test is answered with its own recording.
cp t6.orig "$T6"
run_test test.log
echo "the replay after the prune:"; results | sed 's/^/  /'
for i in 1 2 3 4 5 7 8; do
    t=$(basename "${TICKS[$((i - 1))]}" .yaml)
    line=$(results | awk -F'\t' -v t="$t" '$1 == t')
    [ "$(echo "$line" | cut -f2)" = PASSED ] || fail "test $i ($t) did not pass after the prune: $(echo "$line" | cut -f2-)"
done
# Test 6: a recording that states no lifetimes kept its sequence whole, so it
# passes; one that states them pruned the failed test's own recording, so it
# misses (it is never answered with another test's).
line=$(results | awk -F'\t' -v t="$t6" '$1 == t')
case "$STATES" in
0) [ "$(echo "$line" | cut -f2)" = PASSED ] || fail "test 6 ($t6) did not pass after the prune, on a recording that states no lifetimes: $(echo "$line" | cut -f2-)" ;;
*) echo "$line" | cut -f3 | grep -qE '"n":(7|8)' && fail "test 6 ($t6) was answered with another test's recording: $(echo "$line" | cut -f3)" ;;
esac
if grep -q 'WARNING: DATA RACE' ./*.log; then fail "data race in a keploy log"; fi

if [ "$FAIL" -ne 0 ]; then
    echo "$FAIL check(s) failed"
    exit 1
fi
echo "all checks passed"
