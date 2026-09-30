#!/usr/bin/env bash
source "$(dirname "${BASH_SOURCE[0]}")/../../go-retry.sh"
#
# Schema-based request-body noise detection — end-to-end CI test using the real
# samples-go/mux-elasticsearch app.
#
# The app stamps a server-side `created_at` on every document it indexes, so the
# OUTGOING Elasticsearch request body (POST /documents/_doc) carries a volatile
# field that drifts between a keploy recording and each replay. Five scenarios:
#
#   Phase A — control (flag off): replay writes NO noise.req.
#   Phase B — detect+persist (--schema-noise-detection): the body.created_at
#             drift is detected and persisted under noise.req on the ES mock.
#   Phase C — strict (test.schemaNoiseStrict): a drift on a NON-noise field
#             (content, induced by tampering the recorded mock) is rejected.
#   Phase D — --remove-unused-mocks deletes nothing, and creates no
#             mappings.yaml, after a request that got no answer (client
#             timeout); a clean replay deletes exactly the one unused mock.
#   Phase E — a --remove-unused-mocks replay stopped mid-set (the keploy
#             agent stopped, the replay's context left live) leaves mocks.yaml
#             byte-identical and creates no mappings.yaml; a full replay
#             afterwards passes every test and maps them all.
#
# Runs inside samples-go/mux-elasticsearch. Expects: $KEPLOY_BIN (named
# "keploy"), an Elasticsearch reachable at 127.0.0.1:9200, passwordless sudo, Go.
# GitHub runs the `run:` step as `bash -e` and this file is sourced into it.
# This script does its own error handling (a FAILURES counter + explicit exit)
# and deliberately runs commands that return non-zero by design (pkill with no
# match, grep with no match, startup curl retries), so disable errexit here.
set +e
set -o pipefail

source "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/test-iid.sh"

KEPLOY_BIN="${KEPLOY_BIN:-/usr/local/bin/keploy}"
APP_BIN=mux-elasticsearch
# The app's command line, as keploy starts it (sh -c ./mux-elasticsearch). A
# process name is cut to 15 characters, so `pkill -x mux-elasticsearch`
# matches nothing.
APP_PROC="^\./$APP_BIN( |\$)"
APP_DIR="$PWD"
SND_PATH="$APP_DIR/keploy-snd"          # dedicated path; leaves committed keploy/ untouched
export ELASTICSEARCH_URL="http://127.0.0.1:9200"
# Enable the app's schema-noise demo mode (server-stamped created_at + no
# keep-alive on the ES client). Off by default so the app's committed
# recordings stay valid; this pipeline opts in.
export STAMP_CREATED_AT=1

FAILURES=0
step()  { echo "== $* =="; }
pass()  { echo "  PASS: $*"; }
fail()  { echo "  FAIL: $*"; FAILURES=$((FAILURES + 1)); }
strip_ansi() { sed -E 's/\x1b\[[0-9;]*m//g'; }
mock_file() { find "$SND_PATH" -name mocks.yaml 2>/dev/null | head -1; }

cleanup() {
  sudo pkill -x keploy 2>/dev/null
  sudo pkill -f "$APP_PROC" 2>/dev/null
  # Wait for them to go, so no agent or app outlives its phase into the next.
  local i
  for i in $(seq 1 50); do
    pgrep -x keploy >/dev/null || pgrep -f "$APP_PROC" >/dev/null || break
    sleep 0.2
  done
  return 0
}
# Listeners the phases start. cleanup() cannot stop them (replay() calls
# cleanup() mid-phase); the EXIT trap does.
listener_pids=""
trap 'cleanup; [ -n "$listener_pids" ] && kill $listener_pids 2>/dev/null' EXIT

# ---------------------------------------------------------------------------
# Build app + wait for Elasticsearch
# ---------------------------------------------------------------------------
step "Building mux-elasticsearch"
go_retry build -o "$APP_BIN" . || exit 1

step "Waiting for Elasticsearch at $ELASTICSEARCH_URL"
for i in $(seq 1 30); do
  if curl -fs "$ELASTICSEARCH_URL/_cluster/health" >/dev/null 2>&1; then echo "  ES ready"; break; fi
  sleep 4
done
curl -fs "$ELASTICSEARCH_URL/_cluster/health" >/dev/null 2>&1 || { echo "::error::Elasticsearch not reachable"; exit 1; }

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
# record_fresh [n]: records n POST /documents (default 1). n=1 keeps the exact
# body Phase C tampers; n>1 gives each document its own title, so each test's ES
# mock is distinct.
record_fresh() {
  local n="${1:-1}"
  cleanup
  sudo rm -rf "$SND_PATH"
  sudo -E env "PATH=$PATH" "ELASTICSEARCH_URL=$ELASTICSEARCH_URL" \
    "$KEPLOY_BIN" record -c "./$APP_BIN" --path "$SND_PATH" >"$APP_DIR/record.log" 2>&1 &
  local rec_pid=$!
  local i
  for i in $(seq 1 60); do curl -fs http://localhost:8000/ >/dev/null 2>&1 && break; sleep 1; done
  sleep 2
  if [ "$n" -eq 1 ]; then
    curl -s -X POST http://localhost:8000/documents \
      -H 'Content-Type: application/json' \
      -d '{"title":"hello","content":"world"}' >/dev/null
  else
    for i in $(seq 1 "$n"); do
      curl -s -X POST http://localhost:8000/documents \
        -H 'Content-Type: application/json' \
        -d "{\"title\":\"doc-$i\",\"content\":\"world\"}" >/dev/null
    done
  fi
  sleep 6
  sudo kill -INT "$rec_pid" 2>/dev/null
  for i in $(seq 1 25); do sudo kill -0 "$rec_pid" 2>/dev/null || break; sleep 1; done
  cleanup
  if [ -z "$(mock_file)" ]; then
    echo "  no mock recorded; record.log tail:"; tail -20 "$APP_DIR/record.log" | strip_ansi
    fail "recording produced no ES mock"; return 1
  fi
}

# replay <logfile> <extra keploy test args...>
replay() {
  local log="$1"; shift
  cleanup
  sudo -E env "PATH=$PATH" "ELASTICSEARCH_URL=$ELASTICSEARCH_URL" \
    "$KEPLOY_BIN" test -c "./$APP_BIN" --path "$SND_PATH" --delay 10 "$@" >"$log" 2>&1
  local rc=$?
  cleanup
  return $rc
}

checkout_passed() {  # post-documents testcase passed?
  grep -oE '"testcase id": "[^"]*document[^"]*".*"passed": "[^"]+"' "$1" \
    | strip_ansi | grep -q '"passed": "true"'
}
# Request-body schema noise is written under the unified `noise:` block as a
# `req:` list of field paths (this replaced the legacy top-level `req_body_noise:`
# map). A learned path therefore appears as a `- body.<path>` list item, which is
# unique to noise.req: the HTTP spec's own `req:` is a mapping (no body-path list
# items) and obfuscator value-regexes live under noise.value.
mock_has_created_at_noise() { grep -qE '^[[:space:]]*-[[:space:]]+body\.created_at([[:space:]]|$)' "$(mock_file)" 2>/dev/null; }
mock_has_any_noise()        { grep -qE '^[[:space:]]*-[[:space:]]+body\.' "$(mock_file)" 2>/dev/null; }
mock_count()                { grep -cE '^name:[[:space:]]' "$(mock_file)" 2>/dev/null; }

# start_hanging_listener <dir>: a TCP listener on an ephemeral 127.0.0.1 port
# that accepts and does not answer. Sets hang_port and hang_pid, and appends a
# line to <dir>/accepted per connection. <dir>/port appears only once the
# socket listens, which is the bind check. Once <dir>/release exists, every
# connection, held or new, is answered with a 500 and closed; without it none
# ever is. It exits by itself after 10 minutes.
start_hanging_listener() {
  local dir="$1" i
  rm -rf "$dir" && mkdir -p "$dir"
  python3 - "$dir" <<'PY' &
import os, signal, socket, sys
signal.alarm(600)
d = sys.argv[1]
s = socket.socket()
s.bind(("127.0.0.1", 0))
s.listen(64)
s.settimeout(0.1)
open(os.path.join(d, "port.tmp"), "w").write(str(s.getsockname()[1]))
os.rename(os.path.join(d, "port.tmp"), os.path.join(d, "port"))
held = []
while True:
    try:
        conn, _ = s.accept()
        held.append(conn)
        with open(os.path.join(d, "accepted"), "a") as f:
            f.write("accepted\n")
    except socket.timeout:
        pass
    if held and os.path.exists(os.path.join(d, "release")):
        for conn in held:
            try:
                # Read the request first: closing over unread bytes sends a
                # RST, which can reach the client as a reset, not the answer.
                conn.settimeout(1)
                conn.recv(65536)
                conn.sendall(b"HTTP/1.1 500 Internal Server Error\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
                conn.shutdown(socket.SHUT_WR)
                conn.close()
            except OSError:
                pass
        held = []
PY
  hang_pid=$!
  listener_pids="$listener_pids $hang_pid"
  hang_port=""
  for i in $(seq 1 50); do
    [ -s "$dir/port" ] && { hang_port=$(cat "$dir/port"); break; }
    sleep 0.1
  done
  [ -n "$hang_port" ] && kill -0 "$hang_pid" 2>/dev/null
}

# point_test_at <test.yaml> <port>: the test dials 127.0.0.1:<port> instead of the
# app. Replay dials the recorded app_port in preference to the URL's port, so
# both move.
point_test_at() {
  sudo sed -i -E "s#(localhost|127\.0\.0\.1):8000#\1:$2#; s#^([[:space:]]*app_port:[[:space:]]*)8000[[:space:]]*\$#\1$2#" "$1"
}

# ---------------------------------------------------------------------------
# Phase A — control
# ---------------------------------------------------------------------------
step "Phase A — control (no --schema-noise-detection)"
record_fresh
mock_has_any_noise && fail "fresh recording already has noise.req" \
                   || pass "fresh recording has no noise.req"
replay "$APP_DIR/test_control.log" --remove-unused-mocks
checkout_passed "$APP_DIR/test_control.log" && pass "replay passed with flag off" \
                                            || fail "replay should pass with flag off"
mock_has_any_noise && fail "flag off must NOT write noise.req" \
                   || pass "flag off wrote no noise.req (inert when disabled)"

# ---------------------------------------------------------------------------
# Phase B — detect + persist
# ---------------------------------------------------------------------------
step "Phase B — detect + persist (--schema-noise-detection)"
record_fresh
replay "$APP_DIR/test_detect.log" --schema-noise-detection --remove-unused-mocks
checkout_passed "$APP_DIR/test_detect.log" && pass "replay passed with detection on" \
                                           || fail "replay should pass with detection on"
if mock_has_created_at_noise; then
  pass "ES mock persisted noise.req: body.created_at"
  grep -A3 '^noise:' "$(mock_file)" | sed 's/^/    /'
else
  fail "expected noise.req: body.created_at in the ES mock"
  echo "  --- mocks.yaml ---"; cat "$(mock_file)" 2>/dev/null | sed 's/^/    /'
fi

# ---------------------------------------------------------------------------
# Phase C — strict field-specificity
# Reuses Phase B's noise-bearing mock. Only created_at drifts naturally (and it
# is now noise), so to exercise rejection we tamper a NON-noise field (content)
# in the recorded ES request body: on replay the app re-sends "world", which
# now differs from the mock -> strict matching must reject it.
# ---------------------------------------------------------------------------
step "Phase C — strict matching (test.schemaNoiseStrict: true)"
sed -i 's/"content":"world"/"content":"TAMPERED"/' "$(mock_file)"
STRICT_CFG="$APP_DIR/.strict-config"; mkdir -p "$STRICT_CFG"
cat >"$STRICT_CFG/keploy.yml" <<'EOF'
path: ""
appName: mux-elasticsearch
test:
    schemaNoiseStrict: true
EOF
replay "$APP_DIR/test_strict.log" --schema-noise-detection --config-path "$STRICT_CFG" --debug
if grep 'strict req-body match rejected mock' "$APP_DIR/test_strict.log" | grep -q 'body.content'; then
  pass "strict matching rejected the mock on non-noise drift (body.content)"
else
  fail "expected strict rejection on body.content"
  grep -i 'strict\|reject\|drift' "$APP_DIR/test_strict.log" | strip_ansi | tail -10 | sed 's/^/    /'
fi

# ---------------------------------------------------------------------------
# Phase D — --remove-unused-mocks: no prune after a request got no answer, and
# a clean run still prunes.
# Seven documents, so the 7th test's ES mock lies outside the 5-test startup
# window UpdateMocks always keeps (the readiness GET / is recorded too). There is
# no mappings.yaml, as in a DaemonSet recording.
#   D1: the doc-7 test is pointed at a listener that accepts and never answers,
#       so it hits the client timeout while every other test passes. Tests
#       passed, none was refused and nothing else failed: the shape the prune
#       used to accept, deleting the unanswered test's mock. The run must delete
#       nothing, and must not create mappings.yaml, which would lack doc-7 for good.
#   D2 (control): doc-7 points at the app again and a mock no test sends is
#       planted beside its mock. A clean run must delete exactly that one and
#       create mappings.yaml with doc-7. Without D2, a prune that never runs
#       at all would pass D1.
# The schema-noise demo mode is off here and in Phase E. With it on, the seven
# near-identical ES request bodies drift and match each other's mocks, failing
# tests for a reason these phases are not about.
# ---------------------------------------------------------------------------
step "Phase D — --remove-unused-mocks: no prune after a request got no answer; a clean run still prunes"
unset STAMP_CREATED_AT
record_fresh 7
# Tests are named after the route (post-documents-N), so find doc-7 by its body.
tc7="$(find "$SND_PATH" -path '*/tests/*.yaml' -exec grep -l 'doc-7' {} + 2>/dev/null | head -1)"
set_dir="$(dirname "$(mock_file)")"
sudo rm -f "$set_dir/mappings.yaml"
mocks_before=$(mock_count)
if [ -z "$tc7" ] || [ "${mocks_before:-0}" -lt 7 ]; then
  fail "precondition: want the doc-7 test and >= 7 mocks, got test='$tc7' mocks=${mocks_before:-0}"
else
  sudo cp "$tc7" "$APP_DIR/tc7.orig.yaml"
  if ! start_hanging_listener "$APP_DIR/noanswer-listener"; then
    fail "precondition: the no-answer listener did not start on an ephemeral 127.0.0.1 port"
  else
    point_test_at "$tc7" "$hang_port"
    replay "$APP_DIR/test_noanswer.log" --remove-unused-mocks --api-timeout 3
    mocks_after=$(mock_count)
    # Grep a stripped copy, not `strip_ansi <log | grep -q`: under pipefail that
    # pipeline fails whenever grep -q exits on its first match and sed then takes
    # SIGPIPE writing the rest of a debug-sized log.
    noanswer_log="$APP_DIR/test_noanswer.plain.log"
    strip_ansi <"$APP_DIR/test_noanswer.log" >"$noanswer_log"
    passed_tests=$(awk '/Total test passed:/ { print $NF; exit }' "$noanswer_log")
    failed_tests=$(awk '/Total test failed:/ { print $NF; exit }' "$noanswer_log")
    if ! grep -q 'Client.Timeout exceeded' "$noanswer_log"; then
      fail "precondition: the doc-7 test did not hit the client timeout"
      tail -20 "$noanswer_log" | sed 's/^/    /'
    elif [ "${passed_tests:-0}" -lt 1 ] || [ "${failed_tests:-0}" -ne 1 ]; then
      fail "precondition: want doc-7 as the only failure, got ${passed_tests:-?} passed and ${failed_tests:-?} failed; any other failure lets the prune decide on it instead"
    elif [ "${mocks_after:-0}" -ne "$mocks_before" ]; then
      fail "--remove-unused-mocks deleted $((mocks_before - ${mocks_after:-0})) of $mocks_before mocks after a request got no answer"
    elif sudo test -e "$set_dir/mappings.yaml"; then
      fail "a run where doc-7 got no answer created mappings.yaml, which has no entry for doc-7 and is never completed"
    else
      pass "no mock pruned and no mappings.yaml created after a request got no answer ($mocks_after of $mocks_before kept, $passed_tests tests passed)"
    fi
    grep -E 'skipping mock pruning|not writing mappings.yaml' "$noanswer_log" | sed 's/^/    /'
  fi
  kill "$hang_pid" 2>/dev/null

  # D2: point doc-7 back at the app and plant a copy of its mock that asks ES
  # for a document no test indexes (same body length, same record timestamps).
  sudo cp "$APP_DIR/tc7.orig.yaml" "$tc7"
  sudo python3 - "$(mock_file)" <<'PY'
import re, sys
path = sys.argv[1]
docs = open(path).read().rstrip("\n").split("\n---\n")
src = next(d for d in docs if '"title":"doc-7"' in d)
planted = re.sub(r"(?m)^name: .*$", "name: mock-planted", src, count=1).replace('"title":"doc-7"', '"title":"doc-9"')
open(path, "w").write("\n---\n".join(docs + [planted]) + "\n")
PY
  if ! grep -qE '^name:[[:space:]]+mock-planted$' "$(mock_file)" || [ "$(mock_count)" -ne $((mocks_before + 1)) ]; then
    fail "precondition: could not plant the unused mock (count $(mock_count), want $((mocks_before + 1)))"
  else
    replay "$APP_DIR/test_prune.log" --remove-unused-mocks
    prune_log="$APP_DIR/test_prune.plain.log"
    strip_ansi <"$APP_DIR/test_prune.log" >"$prune_log"
    passed_tests=$(awk '/Total test passed:/ { print $NF; exit }' "$prune_log")
    failed_tests=$(awk '/Total test failed:/ { print $NF; exit }' "$prune_log")
    mocks_after=$(mock_count)
    total_tests=$(find "$set_dir/tests" -name '*.yaml' | wc -l)
    if [ "${failed_tests:-1}" -ne 0 ] || [ "${passed_tests:-0}" -ne "$total_tests" ]; then
      fail "precondition: want all $total_tests tests to pass on the clean run, got ${passed_tests:-?} passed and ${failed_tests:-?} failed"
      tail -20 "$prune_log" | sed 's/^/    /'
    elif grep -qE '^name:[[:space:]]+mock-planted$' "$(mock_file)"; then
      fail "--remove-unused-mocks kept a mock no test used after a clean run; the prune did not run"
      grep 'skipping mock pruning' "$prune_log" | sed 's/^/    /'
    elif [ "${mocks_after:-0}" -ne "$mocks_before" ]; then
      fail "the clean run left ${mocks_after:-0} mocks; want the $mocks_before recorded ones (only the planted mock deleted)"
    elif ! sudo grep -q "id: $(basename "$tc7" .yaml)\$" "$set_dir/mappings.yaml" 2>/dev/null; then
      fail "the clean run did not create mappings.yaml with the doc-7 test"
    else
      pass "a clean run pruned the one unused mock, kept all $mocks_before recorded ones and created mappings.yaml"
    fi
  fi
fi

# ---------------------------------------------------------------------------
# Phase E — --remove-unused-mocks after a replay that stopped mid-set.
# Seven documents again and no mappings.yaml. The doc-4 test is parked on a
# listener that holds its request. Once that request arrives the keploy agent is
# stopped (SIGTERM, as docker stop or the kubelet stops one), and once the agent
# has exited the listener answers doc-4 with a 500: an ordinary failure, so no
# rule but the stop's can refuse the prune. The next test's per-test mock filter
# send then finds no agent, which ends the set: get-root and docs 1-3 passed,
# doc-4 failed and docs 5-7 never ran. (A kill timed off the log cannot land
# mid-set: the tests run milliseconds apart.)
# The stop must leave the replay's context live. A crashed app or a Ctrl-C
# cancels it, and UpdateMocks then fails on "context canceled" by itself, so
# mocks.yaml would survive such a stop even with no prune gate at all. Here only
# the gate stands between docs 5-7's mocks, outside the 5-test startup window,
# and the delete. The stopped run must leave mocks.yaml byte-identical and create
# no mappings.yaml: one created from it would list only the tests that ran, for
# good, since no default run adds to a file that exists. A full replay afterwards
# must then pass every test and create mappings.yaml with every document's test.
# ---------------------------------------------------------------------------
step "Phase E — --remove-unused-mocks: a replay stopped mid-set changes nothing"
record_fresh 7
tc4="$(find "$SND_PATH" -path '*/tests/*.yaml' -exec grep -l 'doc-4' {} + 2>/dev/null | head -1)"
set_dir="$(dirname "$(mock_file)")"
sudo rm -f "$set_dir/mappings.yaml"
total_tests=$(find "$set_dir/tests" -name '*.yaml' | wc -l)
if [ -z "$tc4" ] || [ "$total_tests" -lt 8 ]; then
  fail "precondition: want the doc-4 test among >= 8 tests, got test='$tc4' tests=$total_tests"
elif ! start_hanging_listener "$APP_DIR/stop-listener"; then
  fail "precondition: the stop listener did not start on an ephemeral 127.0.0.1 port"
else
  sudo cp "$(mock_file)" "$APP_DIR/mocks.before-stop.yaml"
  sudo cp "$tc4" "$APP_DIR/tc4.orig.yaml"
  point_test_at "$tc4" "$hang_port"
  cleanup
  # --api-timeout outlasts the agent's shutdown, so doc-4 is still parked when
  # the agent is gone and the listener answers it.
  sudo -E env "PATH=$PATH" "ELASTICSEARCH_URL=$ELASTICSEARCH_URL" \
    "$KEPLOY_BIN" test -c "./$APP_BIN" --path "$SND_PATH" --delay 10 --remove-unused-mocks --api-timeout 30 \
    >"$APP_DIR/test_stopped.log" 2>&1 &
  test_pid=$!
  agent_stopped=0
  agent_pids=""
  for i in $(seq 1 600); do
    if [ -s "$APP_DIR/stop-listener/accepted" ]; then
      # The natively started agent: `keploy agent --port N --proxy-port N ...`.
      agent_pids=$(pgrep -f -- ' agent --port [0-9]+ --proxy-port [0-9]+ ' | tr '\n' ' ')
      if [ "$(echo $agent_pids | wc -w)" -eq 1 ] && sudo kill -TERM $agent_pids; then
        for _ in $(seq 1 100); do
          sudo kill -0 $agent_pids 2>/dev/null || { agent_stopped=1; break; }
          sleep 0.2
        done
      fi
      touch "$APP_DIR/stop-listener/release"
      break
    fi
    sudo kill -0 "$test_pid" 2>/dev/null || break
    sleep 0.2
  done
  for i in $(seq 1 90); do sudo kill -0 "$test_pid" 2>/dev/null || break; sleep 1; done
  cleanup
  wait "$test_pid" 2>/dev/null
  kill "$hang_pid" 2>/dev/null
  sudo cp "$APP_DIR/tc4.orig.yaml" "$tc4"
  stopped_log="$APP_DIR/test_stopped.plain.log"
  strip_ansi <"$APP_DIR/test_stopped.log" >"$stopped_log"
  results=$(grep -c '"testcase id":' "$stopped_log")
  if [ "$agent_stopped" -ne 1 ]; then
    fail "precondition: want the keploy agent (one process, got '${agent_pids% }') stopped while doc-4 was parked"
    tail -20 "$stopped_log" | sed 's/^/    /'
  elif ! grep -q '"passed": "true"' "$stopped_log" || [ "$results" -ge "$total_tests" ] ||
       ! grep -q 'test-set stopped before running every test' "$stopped_log"; then
    fail "precondition: want a run that passed some tests and stopped before the rest, got $results of $total_tests results"
    tail -20 "$stopped_log" | sed 's/^/    /'
  elif ! grep -qE 'failed to update mock parameters on agent.*connection refused' "$stopped_log" ||
       grep -q 'application failed to run' "$stopped_log"; then
    # A cancelled context fails the same send with "context canceled" instead.
    fail "precondition: want the set ended, context live, by the per-test filter send finding no agent (connection refused), not by the app stopping"
    grep -E 'failed to update mock parameters|application failed to run|Signal received' "$stopped_log" | sed 's/^/    /'
  elif grep -q 'failed to simulate request' "$stopped_log"; then
    # A timeout, reset or EOF on doc-4 lets the no-answer or connection-error
    # rule, not the stop's, refuse the prune.
    fail "precondition: want doc-4 answered (the listener's 500), got a transport error"
    grep 'failed to simulate request' "$stopped_log" | sed 's/^/    /'
  else
    # Checked independently: a regression can break either one alone. Each pass
    # also needs the gate's own line, so a run whose writes failed for another
    # reason (a cancelled context) cannot pass for it.
    if ! sudo cmp -s "$APP_DIR/mocks.before-stop.yaml" "$(mock_file)"; then
      fail "the stopped run changed mocks.yaml ($(mock_count) mocks left of $(grep -cE '^name:[[:space:]]' "$APP_DIR/mocks.before-stop.yaml")); the unrun tests' mocks were pruned"
      grep -E 'failed to delete unused mocks' "$stopped_log" | sed 's/^/    /'
    elif ! grep -qE 'skipping mock pruning.*"reason": "not_every_test_got_a_verdict"' "$stopped_log"; then
      fail "mocks.yaml is unchanged, but the prune gate did not refuse with reason not_every_test_got_a_verdict"
      grep -E 'skipping mock pruning|failed to delete unused mocks' "$stopped_log" | sed 's/^/    /'
    else
      pass "the replay stopped after $results of $total_tests tests left mocks.yaml byte-identical (reason not_every_test_got_a_verdict)"
    fi
    if sudo test -e "$set_dir/mappings.yaml"; then
      fail "the stopped run created mappings.yaml from the $results tests it ran; the unrun tests have no entry in it"
    elif ! grep -qE 'not writing mappings\.yaml.*"reason": "not every loaded test got a verdict"' "$stopped_log"; then
      fail "no mappings.yaml exists, but the replay did not hold it for want of a verdict"
      grep -E 'not writing mappings.yaml|test-mock mappings' "$stopped_log" | sed 's/^/    /'
    else
      pass "the stopped run created no mappings.yaml (held: not every loaded test got a verdict)"
    fi
  fi
  grep -E 'skipping mock pruning|not writing mappings.yaml' "$stopped_log" | sed 's/^/    /'

  replay "$APP_DIR/test_after_stop.log"
  after_log="$APP_DIR/test_after_stop.plain.log"
  strip_ansi <"$APP_DIR/test_after_stop.log" >"$after_log"
  passed_tests=$(awk '/Total test passed:/ { print $NF; exit }' "$after_log")
  failed_tests=$(awk '/Total test failed:/ { print $NF; exit }' "$after_log")
  unmapped=""
  for t in $(find "$set_dir/tests" -name '*.yaml' -exec grep -l '"title":"doc-' {} + 2>/dev/null); do
    sudo grep -q "id: $(basename "$t" .yaml)\$" "$set_dir/mappings.yaml" 2>/dev/null || unmapped="$unmapped $(basename "$t" .yaml)"
  done
  if [ "${failed_tests:-1}" -ne 0 ] || [ "${passed_tests:-0}" -ne "$total_tests" ]; then
    fail "the full replay after the stop passed ${passed_tests:-?} and failed ${failed_tests:-?} of $total_tests tests; the unrun tests lost their mocks"
    grep -E '"passed": "false"' "$after_log" | sed 's/^/    /'
  elif [ -n "$unmapped" ]; then
    fail "after the full replay, mappings.yaml still has no entry for:$unmapped"
  else
    pass "the full replay after the stop passed all $total_tests tests and mapped every document's test"
  fi
fi

# ---------------------------------------------------------------------------
echo
if [ "$FAILURES" -eq 0 ]; then
  echo "ALL CHECKS PASSED — schema-noise detection and the --remove-unused-mocks guards verified on mux-elasticsearch."
  exit 0
fi
echo "$FAILURES CHECK(S) FAILED."
exit 1
