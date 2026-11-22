#!/bin/bash
# A request held in flight past keploy's 7 s stale cutoff, beside a steady
# load of fast requests, under `keploy record --sync`: every request is
# recorded, and every test passes keploy test with the upstream gone.
#
# heldload/main.go is the upstream, the application and the load. Each fast
# request makes two upstream calls. Each held request makes CALLS upstream
# calls before its answer's head, then streams one line a second for HOLD
# seconds: its window yields at the head, so the fast requests run beside it,
# and each of their resolves passes over the held request's calls, buffered
# for it, as the window index keeps that pass short. The fast requests go on,
# RATE a second, until the last held request has answered (at least FAST of
# them), so resolves run after a held request's first calls are past the
# cutoff, before its own resolve: they move those calls to the hold for its
# window, which is still open, and its resolve takes them from there. How
# many fast requests go depends on how long the held ones take, not on how
# fast the machine sends them, so the replay's length does not either.
# keploy records with --debug: the buffer-transition line of each resolve
# says how many it took from the hold (held_taken), and a test case left out
# for a window given up is logged. So the checks are:
#   1. keploy record wrote one test case per request sent (none left out);
#   2. its log says no test case was left out, and no window given up;
#   3. the resolve of each held request took calls of its from the hold
#      (held_taken > 0): the cutoff's path to the hold ran for every one;
#   4. keploy test, the upstream gone, passes every one: each has its mocks.
# It also prints how long each resolve held the manager's lock (the buffer
# transition's mu_held) and how many buffered mocks it decided: a
# measurement, never a check.

# keploy runs under sudo, which hands it only the variables its command line
# names (sudo-rs, Ubuntu 26.04's sudo, ignores -E): SUDO_ENV names the
# switches a workflow sets for keploy, each only while it is set.
# shellcheck source=../../sudo_env.sh
source "$(dirname "${BASH_SOURCE[0]}")/../../sudo_env.sh" || exit 1

set -uo pipefail

source "${GITHUB_WORKSPACE:-.}/.github/workflows/test_workflow_scripts/test-iid.sh" 2>/dev/null || true

RECORD_BIN="${RECORD_BIN:?RECORD_BIN must name the keploy binary under test}"
REPLAY_BIN="${REPLAY_BIN:-$RECORD_BIN}"
FAST="${FAST:-3000}"
WORKERS="${WORKERS:-6}"
HELD="${HELD:-3}"
CALLS="${CALLS:-2500}"
HOLD="${HOLD:-9}"
RATE="${RATE:-100}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT="${LANE_OUT:-$HERE}"
APP=127.0.0.1:18092
UP=127.0.0.1:18093
WORK="$(mktemp -d)"

FAIL=0
fail() {
    echo "::error::$*"
    FAIL=$((FAIL + 1))
}

listening() { (exec 3<>"/dev/tcp/${1%:*}/${1##*:}") 2>/dev/null; }
wait_listen() {
    local _i
    for _i in $(seq 1 600); do
        listening "$1" && return 0
        kill -0 "$2" 2>/dev/null || return 1
        sleep 0.1
    done
    return 1
}
# stop_pid PID: SIGINT to the keploy that sudo PID runs, then wait for both.
stop_pid() {
    local child
    child="$(pgrep -P "$1" 2>/dev/null | head -n1)"
    [ -n "$child" ] && sudo kill -INT "$child" 2>/dev/null
    wait "$1" 2>/dev/null
}
UP_PID=""
REC_PID=""
cleanup() {
    [ -n "$REC_PID" ] && stop_pid "$REC_PID"
    [ -n "$UP_PID" ] && kill "$UP_PID" 2>/dev/null
    mkdir -p "$OUT" 2>/dev/null && cp "$WORK"/*.log "$OUT/" 2>/dev/null
    sudo rm -rf "$WORK"
}
trap cleanup EXIT

(cd "$HERE/heldload" && CGO_ENABLED=0 go build -o "$WORK/heldload" .) || { echo "::error::building heldload failed"; exit 1; }
cd "$WORK" || exit 1
for a in "$APP" "$UP"; do
    listening "$a" && { echo "::error::$a is already in use"; exit 1; }
done

./heldload -upstream "$UP" > upstream.log 2>&1 &
UP_PID=$!
wait_listen "$UP" "$UP_PID" || { echo "::error::the upstream did not start"; exit 1; }

echo "== keploy record --sync --debug: fast requests ($RATE a second, at least $FAST) from $WORKERS workers until the last of $HELD held ones of $CALLS calls each, streamed for ${HOLD}s, has answered =="
# The logs are the runner user's: sudo runs keploy, not the redirect.
# shellcheck disable=SC2024
sudo env PATH="$PATH" "${SUDO_ENV[@]}" "$RECORD_BIN" record -c "./heldload -app $APP $UP" --sync --debug \
    --generateGithubActions=false --disable-tele > record.log 2>&1 < /dev/null &
REC_PID=$!
wait_listen "$APP" "$REC_PID" || { echo "::error::the app did not start under keploy record"; tail -30 record.log; exit 1; }
began=$(date +%s)
./heldload -drive "$APP" "$FAST" "$WORKERS" "$HELD" "$CALLS" "$HOLD" "$RATE" > drive.log 2>&1
drive_rc=$?
echo "the load took $(( $(date +%s) - began ))s: $(tail -n1 drive.log)"
[ "$drive_rc" -eq 0 ] || { fail "an answer under keploy record was not the application's (drive.log)"; tail -20 drive.log; }
# One test case per request the drive sent: its last line says how many.
fast_sent='' held_sent=''
read -r fast_sent held_sent < <(sed -nE 's/^sent fast=([0-9]+) held=([0-9]+) bad=[0-9]+$/\1 \2/p' drive.log | tail -n1)
if [ -z "${fast_sent:-}" ] || [ "$held_sent" -ne "$HELD" ] || [ "$fast_sent" -lt "$FAST" ]; then
    echo "::error::the drive did not say it sent at least $FAST fast requests and $HELD held ones (drive.log)"
    tail -5 drive.log
    exit 1
fi
WANT=$((fast_sent + held_sent))
# Every test case is written as its request is decided; wait for them all, or
# for keploy to stop writing them.
n=0 last=-1 still=0
while [ "$still" -lt 300 ]; do
    n=$(find keploy/test-set-0/tests -maxdepth 1 -name '*.yaml' -size +0c 2>/dev/null | wc -l)
    [ "$n" -ge "$WANT" ] && break
    if [ "$n" -eq "$last" ]; then still=$((still + 1)); else still=0; last=$n; fi
    sleep 0.1
done
stop_pid "$REC_PID"; REC_PID=""
sudo chown -R "$(id -u):$(id -g)" . 2>/dev/null
[ "$n" -eq "$WANT" ] || fail "1: keploy record wrote $n test cases, want $WANT (one per request)"
mapfile -t held_tests < <(grep -l 'url: .*/held?id=' keploy/test-set-0/tests/*.yaml 2>/dev/null)
[ "${#held_tests[@]}" -eq "$HELD" ] || fail "1: keploy record wrote ${#held_tests[@]} test cases of a held request, want $HELD"
left=$(grep -cE 'test case was left out|left a kept request out|window was given up' record.log)
[ "$left" -eq 0 ] || { fail "2: keploy record's log says $left times that a test case was left out or a window given up"; grep -nE 'test case was left out|left a kept request out|window was given up' record.log | head -5; }
if grep -q 'WARNING: DATA RACE' record.log; then
    fail "2: a data race under keploy record"
fi

# 3: the buffer-transition line of each held request's own resolve (named by
# its test case) says it took calls of its from the hold.
for f in "${held_tests[@]}"; do
    name=$(awk '/^name:/ {print $2; exit}' "$f")
    taken=$(grep -F 'diag/ResolveRange: buffer transition' record.log | grep -F "\"test_name\": \"$name\"" |
        sed -nE 's/.*"held_taken": ([0-9]+).*/\1/p' | sort -n | tail -n1)
    if [ -z "$name" ] || [ -z "$taken" ]; then
        fail "3: no buffer-transition line of the resolve of held test case ${name:-$f} in keploy record's log"
    elif [ "$taken" -eq 0 ]; then
        fail "3: the resolve of held test case $name took nothing from the hold (held_taken 0): no resolve beside it moved its calls there past the cutoff"
    else
        echo "held test case $name: its resolve took $taken of its calls from the hold"
    fi
done

echo "== how long each resolve held the manager's lock (mu_held), and the buffered mocks it decided =="
python3 - record.log <<'PY'
import re, sys
units = {"ns": 1e-6, "µs": 1e-3, "us": 1e-3, "ms": 1.0, "s": 1e3, "m": 6e4, "h": 3.6e6}
def ms(d):
    total = 0.0
    for v, u in re.findall(r"([0-9.]+)(ns|µs|us|ms|s|m|h)", d):
        total += float(v) * units[u]
    return total
held, decided, buffered = [], [], []
for line in open(sys.argv[1], errors="replace"):
    if "diag/ResolveRange: buffer transition" not in line:
        continue
    m = re.search(r'"mu_held": "([^"]+)"', line)
    if m:
        held.append(ms(m.group(1)))
    m = re.search(r'"decided": (\d+)', line)
    if m:
        decided.append(int(m.group(1)))
    m = re.search(r'"buffer_len_before": (\d+)', line)
    if m:
        buffered.append(int(m.group(1)))
def q(xs, p):
    xs = sorted(xs)
    return xs[min(len(xs) - 1, int(len(xs) * p))] if xs else 0
if not held:
    print("HOLD no resolve said mu_held (a keploy without the field)")
else:
    print("HOLD resolves=%d mu_held_ms p50=%.3f p99=%.3f max=%.3f total=%.1f" % (len(held), q(held, .5), q(held, .99), max(held), sum(held)))
if decided:
    print("HOLD decided p50=%d p99=%d max=%d" % (q(decided, .5), q(decided, .99), max(decided)))
if buffered:
    print("HOLD buffer_len_before p50=%d p99=%d max=%d" % (q(buffered, .5), q(buffered, .99), max(buffered)))
PY

kill "$UP_PID" 2>/dev/null; wait "$UP_PID" 2>/dev/null; UP_PID=""

echo "== keploy test, the upstream gone =="
# shellcheck disable=SC2024
sudo env PATH="$PATH" "${SUDO_ENV[@]}" "$REPLAY_BIN" test -c "./heldload -app $APP $UP" --delay 3 --api-timeout $((HOLD + 60)) \
    --generateGithubActions=false --disable-tele > test.log 2>&1 < /dev/null
sudo chown -R "$(id -u):$(id -g)" . 2>/dev/null
report=$(ls -1d keploy/reports/test-run-* 2>/dev/null | sort -V | tail -n1)/test-set-0-report.yaml
if [ ! -f "$report" ]; then
    fail "4: keploy test wrote no report"
    tail -30 test.log
else
    read -r status passed failed total < <(awk '/^status:/{s=$2} /^success:/{p=$2} /^failure:/{f=$2} /^total:/{t=$2} END{print s, p, f, t}' "$report")
    echo "keploy test: $status, $passed passed, $failed failed of $total"
    [ "$total" = "$WANT" ] || fail "4: keploy test ran $total tests, want $WANT"
    [ "$passed" = "$WANT" ] && [ "$failed" = 0 ] || {
        fail "4: keploy test passed $passed of $WANT ($failed failed): a test without the mocks it was recorded with"
        grep -nE "no matching mock|mock miss|Testrun failed" test.log | head -20
    }
fi

[ "$FAIL" -eq 0 ] || exit 1
echo "PASS: $WANT requests recorded under --sync, $HELD of them held past the stale cutoff beside the load, each taking its calls from the hold, and every test passed with its mocks"
