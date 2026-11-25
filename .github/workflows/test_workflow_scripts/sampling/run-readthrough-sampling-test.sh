#!/usr/bin/env bash
# End-to-end test for uncaptured cache-warming read-through rescue & cold replay:
#
#   1. Start a local upstream HTTP dependency (rt-upstream) and build the
#      in-process caching sample app (rt-app).
#   2. Start `keploy record --sync` with `record.filters` excluding `/warm`
#      (and `/health`) so windowed SyncMockManager recording is active while
#      `/warm` is uncaptured (identical to a sampled-out or filtered request).
#   3. Prime StartupMockTestCaseWindow (6 sequential /prime requests) so later
#      calls are strictly past the startup window.
#   4. Issue uncaptured GET /warm?entity=warm-target -> calls upstream
#      GET /config?entity=warm-target outside every kept test window and
#      populates rt-app's in-process cache.
#   5. Issue kept GET /lookup?entity=warm-target -> hits the warm in-process
#      cache (zero outgoing HTTP calls in its own window) and is recorded.
#   6. Stop keploy record and STOP rt-upstream (no live upstream during replay).
#   7. Verify YAML artifacts:
#        - zero *warm*.yaml test files exist (/warm was uncaptured)
#        - at least one *lookup*.yaml test file exists
#        - mocks.yaml has a mock-rt-* entry with readThrough: "true" for
#          entity=warm-target and zero non-read-through mocks for warm-target.
#   8. Run `keploy test` on a cold-started rt-app in BOTH KEPLOY_PROVEN="" and
#      KEPLOY_PROVEN=1 (with KEPLOY_EXACT_VIEW_VERIFY=1 verified via verify_ran):
#        - Positive: cold /lookup?entity=warm-target hits mock-rt-* -> PASSED.
#        - Negative: OVERRIDE_ENTITY=other-entity misses cleanly (must NOT
#          receive owner-warm-target) -> FAILED.
#
# Expects (env): KEPLOY_BIN (executable path to built keploy binary).
# Optional   :  RT_APP_PORT (default 18082), RT_UPSTREAM_PORT (default 19091).

# Enable exact-view verify mode for KEPLOY_PROVEN=1 runs unless explicitly
# overridden, before sourcing sudo_env.sh so SUDO_ENV forwards it to sudo.
export KEPLOY_EXACT_VIEW_VERIFY="${KEPLOY_EXACT_VIEW_VERIFY:-1}"

# shellcheck source=../sudo_env.sh
source "$(dirname "${BASH_SOURCE[0]}")/../sudo_env.sh" || exit 1
source "$(dirname "${BASH_SOURCE[0]}")/../go-retry.sh"

set -Eeuo pipefail

: "${KEPLOY_BIN:?KEPLOY_BIN must point to the keploy binary}"
if [ ! -x "$KEPLOY_BIN" ]; then
    echo "::error::keploy binary $KEPLOY_BIN is not executable"
    exit 1
fi

RT_APP_PORT="${RT_APP_PORT:-18082}"
RT_UPSTREAM_PORT="${RT_UPSTREAM_PORT:-19091}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FIXTURE_DIR="${SCRIPT_DIR}/readthrough"
WORK="$(mktemp -d)"

UPSTREAM_PID=""
REC_PID=""

section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

pid_running() {
    local st
    case "${1:-}" in "" | *[!0-9]*) return 1 ;; esac
    { read -r st <"/proc/$1/stat"; } 2>/dev/null || return 1
    st=${st##*) }
    [ "${st%% *}" != Z ]
}

stop_keploy_pid() {
    local p="$1" child
    [ -n "$p" ] || return 0
    child="$(pgrep -P "$p" 2>/dev/null | head -n1 || true)"
    if [ -n "$child" ]; then
        sudo kill -INT "$child" 2>/dev/null || true
    else
        sudo kill -INT "$p" 2>/dev/null || true
    fi
    for _ in $(seq 1 30); do
        pid_running "$p" || break
        sleep 0.5
    done
    if pid_running "$p"; then
        sudo kill -KILL "$p" 2>/dev/null || true
    fi
    wait "$p" 2>/dev/null || true
}

cleanup() {
    local rc=$?
    section "Cleanup read-through sampling e2e"
    if [ -n "$REC_PID" ]; then
        stop_keploy_pid "$REC_PID"
        REC_PID=""
    fi
    if [ -n "$UPSTREAM_PID" ] && kill -0 "$UPSTREAM_PID" 2>/dev/null; then
        kill "$UPSTREAM_PID" 2>/dev/null || true
        wait "$UPSTREAM_PID" 2>/dev/null || true
    fi
    if [ "$rc" -ne 0 ]; then
        echo "=== record.log tail ==="
        tail -n 120 "$WORK/record.log" 2>/dev/null || true
        for f in "$WORK"/replay-*.log; do
            [ -f "$f" ] || continue
            echo "=== $(basename "$f") tail ==="
            tail -n 120 "$f" 2>/dev/null || true
        done
    fi
    sudo rm -rf "$WORK" 2>/dev/null || rm -rf "$WORK" 2>/dev/null || true
    endsec
    exit "$rc"
}
trap cleanup EXIT

# Fake installation-id so keploy does not phone home.
sudo mkdir -p /root/.keploy "$HOME/.keploy" 2>/dev/null || true
echo "ObjectID('123456789')" | sudo tee /root/.keploy/installation-id.yaml "$HOME/.keploy/installation-id.yaml" >/dev/null 2>&1 || true

section "Build read-through sample app and upstream"
(cd "$FIXTURE_DIR" && go_retry build -o "$WORK/rt-upstream" ./upstream && go_retry build -o "$WORK/rt-app" ./app)
ls -lh "$WORK/rt-upstream" "$WORK/rt-app"
endsec

cd "$WORK"

section "Start local upstream on 127.0.0.1:$RT_UPSTREAM_PORT"
UPSTREAM_PORT="$RT_UPSTREAM_PORT" ./rt-upstream >upstream.log 2>&1 &
UPSTREAM_PID=$!
ready=0
for _ in $(seq 1 30); do
    if curl -sf --max-time 2 "http://127.0.0.1:${RT_UPSTREAM_PORT}/health" >/dev/null 2>&1; then
        ready=1
        break
    fi
    sleep 0.2
done
if [ "$ready" -ne 1 ]; then
    echo "::error::rt-upstream failed to start on :$RT_UPSTREAM_PORT"
    cat upstream.log || true
    exit 1
fi
echo "rt-upstream ready (pid=$UPSTREAM_PID)"
endsec

section "Start keploy record (--sync with /warm filtered out)"
# Write keploy.yml filtering /health and /warm so /warm warms the app's
# in-process cache without being recorded as a test case (just like a
# sampled-out or filtered request in windowed synchronous recording).
cat >keploy.yml <<'YAML'
record:
  sync: true
  filters:
    - path: /health
    - path: /warm
YAML

sudo env -u KEPLOY_PROVEN PATH="$PATH" "${SUDO_ENV[@]}" \
    PORT="$RT_APP_PORT" UPSTREAM_URL="http://127.0.0.1:${RT_UPSTREAM_PORT}" \
    "$KEPLOY_BIN" record \
        -c "env PORT=${RT_APP_PORT} UPSTREAM_URL=http://127.0.0.1:${RT_UPSTREAM_PORT} ./rt-app" \
        --sync \
        --generate-github-actions=false \
        --disable-tele \
        >record.log 2>&1 </dev/null &
REC_PID=$!

ready=0
for i in $(seq 1 60); do
    if curl -sf --max-time 2 "http://127.0.0.1:${RT_APP_PORT}/health" >/dev/null 2>&1; then
        echo "rt-app ready after ${i}s"
        ready=1
        break
    fi
    if ! pid_running "$REC_PID"; then
        echo "::error::keploy record exited before rt-app became ready"
        exit 1
    fi
    sleep 1
done
if [ "$ready" -ne 1 ]; then
    echo "::error::rt-app failed to come up within 60s"
    exit 1
fi
endsec

section "Prime past StartupMockTestCaseWindow (6 requests)"
for i in 1 2 3 4 5 6; do
    resp="$(curl -sf --max-time 5 "http://127.0.0.1:${RT_APP_PORT}/prime?i=${i}")"
    echo "prime ${i}: ${resp}"
    sleep 0.2
done
endsec

section "Uncaptured cache warm followed by kept warm-cache lookup"
# /warm is filtered out (no test case recorded), calls upstream
# GET /config?entity=warm-target outside any kept test window, and populates
# rt-app's in-process cache.
warm_out="$(curl -sf --max-time 10 "http://127.0.0.1:${RT_APP_PORT}/warm?entity=warm-target")"
echo "warm response: ${warm_out}"
grep -q '"owner":"owner-warm-target"' <<<"$warm_out" || {
    echo "::error::/warm did not fetch expected upstream payload: ${warm_out}"
    exit 1
}

# Wait briefly, then issue the kept /lookup request: warm-target is already hot
# in rt-app's cache, so this kept test makes zero outgoing HTTP calls.
sleep 0.5
lookup_out="$(curl -sf --max-time 5 "http://127.0.0.1:${RT_APP_PORT}/lookup?entity=warm-target")"
echo "kept lookup response: ${lookup_out}"
grep -q '"owner":"owner-warm-target"' <<<"$lookup_out" || {
    echo "::error::kept /lookup did not return warm-target payload: ${lookup_out}"
    exit 1
}
sleep 2
endsec

section "Stop keploy record and upstream"
stop_keploy_pid "$REC_PID"
REC_PID=""
sudo chown -R "$(id -u):$(id -g)" "$WORK" 2>/dev/null || true

kill "$UPSTREAM_PID" 2>/dev/null || true
wait "$UPSTREAM_PID" 2>/dev/null || true
UPSTREAM_PID=""
echo "rt-upstream stopped (replay runs strictly offline)"
endsec

section "Verify recorded tests and rescued mock-rt-* artifact"
if grep -E "panic:|WARNING: DATA RACE" record.log >/dev/null 2>&1; then
    echo "::error::panic or data race in record.log"
    exit 1
fi
echo "recorded test files:"
ls -1 keploy/test-set-0/tests | sed 's/^/  /'

# Verify /warm was uncaptured (zero test cases mention /warm), /lookup was
# recorded (at least 1 test case), mocks.yaml has a mock-rt-* entry with
# readThrough="true" and zero non-read-through mocks for entity=warm-target,
# and mappings.yaml maps zero mocks to the /lookup test case.
python3 - <<'PY'
import glob, os, sys

test_files = sorted(glob.glob("keploy/test-set-0/tests/*.yaml"))
warm_tests = []
lookup_tests = []
for path in test_files:
    with open(path, "r", encoding="utf-8") as f:
        body = f.read()
    name = os.path.splitext(os.path.basename(path))[0]
    if "/warm?" in body or "/warm\n" in body:
        warm_tests.append(name)
    if "/lookup?entity=warm-target" in body:
        lookup_tests.append(name)

print(f"test cases: total={len(test_files)}, warm={warm_tests}, lookup={lookup_tests}")
if warm_tests:
    print(f"::error::/warm was NOT excluded (recorded in {warm_tests})", file=sys.stderr)
    sys.exit(1)
if not lookup_tests:
    print("::error::expected at least 1 kept /lookup?entity=warm-target test case", file=sys.stderr)
    sys.exit(1)

with open("keploy/test-set-0/mocks.yaml", "r", encoding="utf-8") as f:
    raw = f.read()

docs = [d.strip() for d in raw.split("\n---") if d.strip()]
rt_warm = 0
non_rt_warm = 0
rt_names = []
for d in docs:
    if "entity=warm-target" not in d:
        continue
    is_rt_name = "name: mock-rt-" in d
    is_rt_meta = 'readThrough: "true"' in d or "readThrough: 'true'" in d or "readThrough: true" in d
    if is_rt_name and is_rt_meta:
        rt_warm += 1
        for line in d.splitlines():
            if line.startswith("name: "):
                rt_names.append(line.split("name: ", 1)[1].strip())
    else:
        non_rt_warm += 1

print(f"mocks.yaml entity=warm-target: readThrough={rt_warm} ({rt_names}), nonReadThrough={non_rt_warm}")
if rt_warm < 1:
    print("::error::expected rescued mock-rt-* with readThrough=true for entity=warm-target", file=sys.stderr)
    sys.exit(1)
if non_rt_warm != 0:
    print("::error::unexpected non-read-through mock for entity=warm-target (must come solely from read-through rescue)", file=sys.stderr)
    sys.exit(1)

if not os.path.exists("keploy/test-set-0/mappings.yaml"):
    print("::error::expected keploy/test-set-0/mappings.yaml under --sync", file=sys.stderr)
    sys.exit(1)
with open("keploy/test-set-0/mappings.yaml", "r", encoding="utf-8") as f:
    mp = f.read()
for rt_name in rt_names:
    if rt_name in mp:
        print(f"::error::{rt_name} must not be mapped as a per-test mock in mappings.yaml", file=sys.stderr)
        sys.exit(1)
# Also verify each /lookup test entry in mappings.yaml owns zero mocks.
lines = mp.splitlines()
for idx, line in enumerate(lines):
    stripped = line.strip()
    for lt in lookup_tests:
        if stripped == f"id: {lt}" or stripped == f"- id: {lt}":
            # Scan until the next test block or next top-level YAML key.
            for nxt in lines[idx + 1:]:
                if nxt and not nxt[0].isspace() and not nxt.startswith("-"):
                    break
                ns = nxt.strip()
                if ns.startswith("- id:") or ns.startswith("id:"):
                    break
                if ns.startswith("- name:"):
                    print(f"::error::{lt} unexpectedly owns mock ({ns}) in mappings.yaml", file=sys.stderr)
                    sys.exit(1)
PY
endsec

newest_report() {
    local run_dir
    run_dir="$(ls -1d keploy/reports/test-run-* 2>/dev/null | sort -t- -k3 -n | tail -n1 || true)"
    if [ -n "$run_dir" ]; then
        echo "${run_dir}/test-set-0-report.yaml"
    fi
}

report_status() {
    local rpt="$1"
    grep -m1 '^status:' "$rpt" 2>/dev/null | awk '{print $2}'
}

run_cold_replay() {
    local label="$1" proven="$2" override="$3" expect="$4"
    local log="replay-${label}.log"
    local before after status rc cmd
    before="$(newest_report)"
    cmd="env PORT=${RT_APP_PORT} UPSTREAM_URL=http://127.0.0.1:${RT_UPSTREAM_PORT}"
    if [ -n "$override" ]; then
        cmd="${cmd} OVERRIDE_ENTITY=${override}"
    fi
    cmd="${cmd} ./rt-app"

    set +e
    if [ -n "$proven" ]; then
        sudo env -u KEPLOY_PROVEN PATH="$PATH" "${SUDO_ENV[@]}" KEPLOY_PROVEN="$proven" \
            "$KEPLOY_BIN" test -c "$cmd" \
            --test-sets test-set-0 \
            --delay 1 \
            --health-path /health \
            --generate-github-actions=false \
            --disable-tele >"$log" 2>&1 </dev/null
    else
        sudo env -u KEPLOY_PROVEN PATH="$PATH" "${SUDO_ENV[@]}" \
            "$KEPLOY_BIN" test -c "$cmd" \
            --test-sets test-set-0 \
            --delay 1 \
            --health-path /health \
            --generate-github-actions=false \
            --disable-tele >"$log" 2>&1 </dev/null
    fi
    rc=$?
    set -e
    sudo chown -R "$(id -u):$(id -g)" "$WORK" 2>/dev/null || true

    verify_ran "$proven" "$log" || exit 1

    if grep -E "panic:|WARNING: DATA RACE" "$log" >/dev/null 2>&1; then
        echo "::error::${label}: panic or data race detected in ${log}"
        exit 1
    fi
    after="$(newest_report)"
    if [ -z "$after" ] || [ "$after" = "$before" ] || [ ! -f "$after" ]; then
        echo "::error::${label}: keploy test produced no report (rc=${rc})"
        exit 1
    fi
    status="$(report_status "$after")"
    echo "${label}: rc=${rc} status=${status} report=${after}"

    if [ "$expect" = "PASS" ]; then
        if [ "$rc" -ne 0 ] || [ "$status" != "PASSED" ]; then
            echo "::error::${label}: expected PASSED (rc=0), got status=${status} rc=${rc}"
            exit 1
        fi
    else
        if [ "$rc" -eq 0 ] || [ "$status" != "FAILED" ]; then
            echo "::error::${label}: expected FAILED (rc!=0) for different entity ${override}, got status=${status} rc=${rc}"
            exit 1
        fi
        # Ensure the different-entity call actually missed cleanly (returned 502
        # with lookup upstream miss) rather than receiving warm-target's cached body.
        if ! grep -q "lookup upstream miss" "$after"; then
            echo "::error::${label}: expected different-entity miss response in report ${after}"
            exit 1
        fi
    fi
}

section "Cold replay positive (exact match) and negative (different entity miss) in both modes"
run_cold_replay "positive-proven-unset" "" "" "PASS"
run_cold_replay "positive-proven-1" "1" "" "PASS"
run_cold_replay "negative-other-entity-proven-unset" "" "other-entity" "FAIL"
run_cold_replay "negative-other-entity-proven-1" "1" "other-entity" "FAIL"
endsec

echo "✅ sampled cache-warming read-through e2e passed (both KEPLOY_PROVEN='' and KEPLOY_PROVEN=1)"
