#!/bin/bash

# E2E for STREAMING mock-mismatch reporting. Uses the sse-redis sample, whose
# streaming endpoints (SSE / NDJSON / multipart / plain-text) are served from
# Redis. We record it, then MUTATE the recorded LRANGE read mock — the outgoing
# Redis call the streaming GETs depend on — to a non-matching key of the SAME
# byte length (so RESP framing stays valid). On replay the streaming (Phase-2)
# path must SURFACE the unmatched outgoing Redis (Generic) call on a streaming
# test case. This is the behavior added by finalizing the per-test capture
# window AFTER stream consumption; before it, the streaming path opened no
# window and carried no unmatched_calls. Success here is INVERTED: we assert the
# streaming mismatch is reported, not that every test passes.

echo "$RECORD_BIN"
echo "$REPLAY_BIN"

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh
echo "iid.sh executed"

git fetch origin

# Streaming mock-mismatch reporting only exists in the new (locally built)
# replay binary. On an older 'latest' artifact the streaming path opens no
# capture window, so skip cleanly rather than fail a cross-version cell
# (mirrors the capability gate in risk_profile / connect_tunnel).
case "${REPLAY_BIN:-}" in
    */build/keploy|*/build-no-race/keploy) ;;
    *)
        echo "REPLAY_BIN ($REPLAY_BIN) predates streaming mock-mismatch reporting; skipping suite"
        exit 0
        ;;
esac

if [ -f "./keploy.yml" ]; then
    rm ./keploy.yml
fi
rm -rf keploy/
rm -f record_logs.txt test_logs.txt

# Redis backing store for the sample's streaming endpoints.
docker compose up -d
echo "waiting for redis..."
for i in $(seq 1 30); do
    if docker compose exec -T redis redis-cli ping 2>/dev/null | grep -q PONG; then
        echo "redis ready"
        break
    fi
    sleep 2
done

build_go_app() {
  local attempt=1
  local max_attempts=4
  local sleep_sec=5
  while [ "$attempt" -le "$max_attempts" ]; do
    if GOPROXY="proxy.golang.org,direct" go build -o sse-redis-app; then
      return 0
    fi
    if [ "$attempt" -ge "$max_attempts" ]; then
      echo "::error::go build for sse-redis-app failed after ${max_attempts} attempts"
      return 1
    fi
    echo "go build attempt ${attempt} failed; retrying in ${sleep_sec}s…"
    sleep "$sleep_sec"
    sleep_sec=$((sleep_sec * 2))
    attempt=$((attempt + 1))
  done
}
build_go_app
echo "go binary built"

sudo "$RECORD_BIN" config --generate

send_request() {
    sleep 6
    app_started=false
    while [ "$app_started" = false ]; do
        if curl -sf http://localhost:8080/health >/dev/null 2>&1; then
            app_started=true
        fi
        sleep 2
    done
    echo "App started"
    # request.sh fires POST /messages (writes) then GETs every streaming
    # endpoint (SSE/NDJSON/multipart/plain), producing the streaming test cases.
    BASE_URL=http://localhost:8080 bash ./request.sh || true
    sleep 7
    pid=$(pgrep keploy)
    echo "$pid Keploy PID"
    echo "Killing Keploy"
    # Unquoted on purpose: pgrep can return BOTH the keploy CLI and the agent
    # process, so $pid may be multiple newline-separated PIDs. Quoting it passes
    # one mangled arg to kill ("failed to parse argument"), keploy survives, and
    # `wait` on the record command hangs to the job timeout. Word-split instead.
    sudo kill $pid
}

# Record one iteration of test cases + Redis mocks.
send_request &
"$RECORD_BIN" record -c "./sse-redis-app" --generateGithubActions=false 2>&1 | tee record_logs.txt
if grep "WARNING: DATA RACE" record_logs.txt; then
    echo "::error::Race condition detected in recording"
    cat record_logs.txt
    exit 1
fi
wait
echo "Recorded streaming test cases and Redis mocks"

# Force a streaming mock mismatch: rewrite the recorded LRANGE read key only.
# The streaming GETs read the message list with `lrange sse-demo:messages 0 -1`;
# rewriting that key (same length, so the RESP $17 prefix stays valid) means the
# live read no longer matches any recorded mock. The rpush/del writes are left
# intact, so the mismatch is isolated to the streaming reads.
shopt -s nullglob
mock_files=( ./keploy/test-set-*/mocks.yaml )
shopt -u nullglob
if [ ${#mock_files[@]} -eq 0 ]; then
    echo "::error::No recorded mocks found to mutate under ./keploy/test-set-*/"
    cat record_logs.txt
    exit 1
fi
mutated_any=false
for mf in "${mock_files[@]}"; do
    python3 - "$mf" <<'PY'
import sys
p = sys.argv[1]
s = open(p).read()
old = "lrange\\r\\n$17\\r\\nsse-demo:messages\\r\\n"
new = "lrange\\r\\n$17\\r\\nsse-demo:MISMATCH\\r\\n"
n = s.count(old)
open(p, "w").write(s.replace(old, new))
print("mutated %d lrange read mock(s) in %s" % (n, p))
PY
    if grep -q 'sse-demo:MISMATCH' "$mf"; then
        echo "mutated LRANGE read key in: $mf"
        mutated_any=true
    fi
done
if [ "$mutated_any" != true ]; then
    echo "::error::mutation changed no LRANGE read mock — sample/mock layout changed; e2e can't force a streaming mismatch"
    head -60 "${mock_files[0]}"
    exit 1
fi

# Replay. The mutated reads no longer match → the streaming path must report the
# unmatched outgoing Redis call on a streaming test. Replay is EXPECTED to not
# all-pass here.
"$REPLAY_BIN" test -c "./sse-redis-app" --delay 7 --generateGithubActions=false 2>&1 | tee test_logs.txt
if grep "WARNING: DATA RACE" test_logs.txt; then
    echo "::error::Race condition detected in test"
    cat test_logs.txt
    exit 1
fi

# Assert the mismatch surfaced on a STREAMING test case (events-sse / -ndjson /
# -multipart / -plain) — i.e. a test routed through the Phase-2 streaming path
# carries an unmatched_calls entry. This is the streaming-specific behavior:
# before the fix the streaming path opened no capture window, so these tests
# carried no unmatched_calls regardless of the forced mismatch.
shopt -s nullglob
reports=( ./keploy/reports/test-run-*/test-set-*-report.yaml )
shopt -u nullglob
if [ ${#reports[@]} -eq 0 ]; then
    echo "::error::no test-set report produced"
    tail -40 test_logs.txt
    exit 1
fi
streaming_reported=false
for rf in "${reports[@]}"; do
    if python3 - "$rf" <<'PY'
import sys, re
txt = open(sys.argv[1]).read()
# Each per-test block starts with "    - " at the test-results list level.
blocks = re.split(r'\n    - ', txt)
ok = False
for b in blocks:
    m = re.search(r'test_case_id:\s*(\S+)', b)
    if not m:
        continue
    tid = m.group(1)
    # streaming test cases are the get-events-* endpoints
    if 'events-' in tid and 'unmatched_calls:' in b:
        ok = True
        print("streaming test reported a mismatch:", tid)
sys.exit(0 if ok else 1)
PY
    then
        echo "✓ $(basename "$rf") has a streaming test with an unmatched_calls entry"
        streaming_reported=true
    fi
done

if [ "$streaming_reported" != true ]; then
    echo "::error::replay did NOT report the forced streaming mock mismatch on any streaming test"
    echo "--- mismatch/unmatched lines in test_logs.txt ---"
    grep -iE "mismatch|unmatched|no matching" test_logs.txt | head -20
    echo "--- tail of test_logs.txt ---"
    tail -40 test_logs.txt
    docker compose down 2>/dev/null || true
    exit 1
fi

echo "streaming mock-mismatch e2e passed: a streaming test reported the forced unmatched Redis read"
docker compose down 2>/dev/null || true
exit 0
