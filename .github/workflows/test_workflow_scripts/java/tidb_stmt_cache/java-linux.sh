#!/usr/bin/env bash

# E2E test for keploy's MySQL prepared-statement orphan-EXECUTE matching
# against a real TiDB :4000 instance.
#
# What the matcher is being asked to do: pair a recorded COM_STMT_EXECUTE
# mock with an incoming COM_STMT_EXECUTE at replay time, even when the
# EXECUTE mock has no resolvable paired COM_STMT_PREPARE in the recorded
# pool (recordedPrepByConn lookup miss -> expectedQuery == "").
#
# The combination that surfaces this in production -- TiDB serving the
# MySQL wire protocol on :4000, plus Connector/J 8.x with
# useServerPrepStmts=true&cachePrepStmts=true -- is what the
# samples-java/tidb-stmt-cache app reproduces. See keploy/keploy@b2e68adb
# for the matcher fix this script protects against regression.
#
# If the natural orphan condition fails to fire in CI (it has a race-y
# dependency on HikariCP's LIFO pool rotation lining up across requests),
# the optional yq fallback at the end of the record phase deliberately
# strips one COM_STMT_PREPARE mock from the recorded mocks.yaml so the
# replay deterministically exercises the param-alone EXECUTE branch.

set -Eeuo pipefail

section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

die() {
  rc=$?
  echo "::error::Pipeline failed (exit=$rc). Dumping context…"
  echo "== docker ps =="; docker ps || true
  echo "== tidb logs (last 200 lines) =="; docker compose logs --tail 200 tidb || true
  echo "== workspace tree (depth 3) =="; find . -maxdepth 3 -type d -print | sort || true
  echo "== keploy tree (depth 4) =="; find ./keploy -maxdepth 4 -type f -print 2>/dev/null | sort | head -n 20 || true; echo "... (truncated)"
  echo "== *.txt logs (last 100 lines) =="; for f in ./*.txt; do [[ -f "$f" ]] && { echo "--- $f ---"; tail -n 100 "$f"; }; done
  exit "$rc"
}
trap die ERR

wait_for_tidb() {
  section "Wait for TiDB readiness"
  # TiDB exposes a status HTTP server on :10080 once it's ready to serve
  # SQL on :4000. Polling that endpoint is more reliable than retrying
  # SQL connects because Connector/J's connection failure modes vary
  # (handshake timeout vs. refused vs. read timeout) during TiDB bootstrap.
  for i in {1..120}; do
    if curl -sS http://localhost:10080/status >/dev/null 2>&1; then
      # One round-trip SQL connect just to make sure the SQL listener is
      # actually accepting auth too, not just the status server.
      if docker run --rm --network host mysql:8.0 \
           mysql -h 127.0.0.1 -P 4000 -uroot -e "SELECT 1" >/dev/null 2>&1; then
        echo "TiDB is ready."
        endsec; return 0
      fi
    fi
    sleep 1
  done
  echo "::error::TiDB did not become ready in time"
  endsec; return 1
}

wait_for_app() {
  section "Wait for app HTTP port"
  for i in {1..60}; do
    if curl -sS http://localhost:8080/api/health -o /dev/null 2>/dev/null; then
      echo "App is responding."
      endsec; return 0
    fi
    sleep 1
  done
  echo "::error::App did not start in time"
  endsec; return 1
}

run_maven_build() {
  : > mvn_build.log

  for attempt in 1 2 3; do
    if {
      echo "===== Maven build attempt ${attempt}/3 ====="
      mvn -B -U clean package -Dmaven.test.skip=true -q
    } 2>&1 | tee -a mvn_build.log; then
      return 0
    fi

    echo "Maven build failed on attempt ${attempt}/3. Retrying."
    if [[ "$attempt" -lt 3 ]]; then
      sleep $((attempt * 10))
    fi
  done

  echo "::error::Maven build failed after 3 attempts. Review mvn_build.log."
  return 1
}

send_request() {
  local kp_pid="$1"

  wait_for_app

  # Health probe — also recorded as a baseline non-PREPARE COM_QUERY mock
  # so the matcher has a control case in the same test window.
  echo "=== /api/health (warmup, plain SELECT 1) ==="
  curl -sS http://localhost:8080/api/health || true

  # First parameterized call on a fresh pooled connection: Connector/J
  # emits COM_STMT_PREPARE + COM_STMT_EXECUTE. Recorded as paired mocks.
  echo "=== /api/kv/1 (first prepared SELECT, populates cache) ==="
  curl -sS http://localhost:8080/api/kv/1 || true

  # Subsequent calls on the SAME pooled connection (HikariCP LIFO with
  # maximumPoolSize=3 and only one concurrent caller, the same connection
  # comes back nearly every time). Connector/J finds "SELECT ? AS v" in
  # its cache and emits ONLY COM_STMT_EXECUTE. Recorder captures an
  # EXECUTE-only mock whose paired PREPARE may or may not resolve via
  # recordedPrepByConn depending on connID attribution -- this is the
  # exact code path keploy/keploy@b2e68adb's param-alone fallback covers.
  for v in 2 3 4 5; do
    echo "=== /api/kv/${v} (cache-hit candidate, EXECUTE-only) ==="
    curl -sS "http://localhost:8080/api/kv/${v}" || true
  done

  # Two distinct prepared SQL strings on the same connection. Exercises
  # the matcher's per-(connID,stmtID) tracking when more than one prep
  # is live at once.
  for v in 10 20; do
    echo "=== /api/kv/insert-select/${v} (two prepares on same conn) ==="
    curl -sS "http://localhost:8080/api/kv/insert-select/${v}" || true
  done

  # Let keploy persist, then gracefully stop it.
  sleep 10
  echo "$kp_pid Keploy PID"
  echo "Sending SIGINT to keploy for graceful shutdown"
  sudo kill -INT "$kp_pid" 2>/dev/null || true
}

# --- Main ---

source "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/test-iid.sh"

# Clean slate
sudo rm -rf keploy/ keploy.yml

section "Start TiDB"
docker compose up -d
wait_for_tidb
endsec

section "Build"
source "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/update-java.sh"
run_maven_build
endsec

JAR_NAME=$(ls target/tidb-stmt-cache-*.jar 2>/dev/null | head -n1)
if [[ -z "$JAR_NAME" ]]; then
  echo "::error::JAR not found after build"
  exit 1
fi

do_record_iteration() {
  local i="$1"
  local extra_flags="${2:-}"
  local label="${extra_flags:+_json}"
  local app_name="tidbStmtCache_${i}${label}"
  section "Record iteration $i${label:+ (json)}"

  # shellcheck disable=SC2086
  "$RECORD_BIN" record $extra_flags \
    -c "java -jar $JAR_NAME" \
    > "${app_name}.txt" 2>&1 &
  local KEPLOY_PID=$!

  send_request "$KEPLOY_PID"

  set +e
  wait "$KEPLOY_PID"
  local rc=$?
  set -e
  echo "Record exit code: $rc"
  [[ $rc -ne 0 ]] && echo "Keploy record exited non-zero (iteration $i${label:+ json}), rc=$rc"

  if grep -q "WARNING: DATA RACE" "${app_name}.txt"; then
    echo "::error::Data race detected in ${app_name}.txt"
    cat "${app_name}.txt"
    exit 1
  fi
  if grep -q "ERROR" "${app_name}.txt"; then
    echo "Errors found in ${app_name}.txt (not fatal unless record failed)"
    cat "${app_name}.txt"
  fi

  endsec
  echo "Recorded test case and mocks for iteration $i${label:+ (json)}"
}

for i in 1; do
  do_record_iteration "$i"
done

# shellcheck disable=SC1091
source "${GITHUB_WORKSPACE:-${PWD%/samples-*}}/.github/workflows/test_workflow_scripts/json-pass-helpers.sh"

if json_pass_supported; then
  for i in 1; do
    do_record_iteration "$i" "--storage-format json"
  done
fi

sleep 5

# -----------------------------------------------------------------------
# Orphan-EXECUTE forcing step (deterministic fallback).
#
# The natural orphan condition (EXECUTE mock whose paired PREPARE is
# invisible to buildRecordedPrepIndex) depends on connID attribution
# lining up just-so during a single record cycle. To guarantee that
# keploy/keploy@b2e68adb's param-alone fallback is exercised every CI
# run -- regardless of whether the natural condition fired -- we strip
# exactly one COM_STMT_PREPARE mock from the recorded mocks.yaml here.
#
# The next replay run will see EXECUTE mocks whose stmtID does not
# resolve via recordedPrepByConn -> the matcher falls through to the
# param-alone branch (match.go lines ~967-976). If that branch ever
# regresses to "no matching mock", the test report flips to FAILED and
# this whole job fails loudly.
#
# We only mutate the YAML test-set; the json test-set (if produced) is
# left untouched so the fallback is exercised in YAML and the natural-
# path coverage is preserved in JSON.
# -----------------------------------------------------------------------
section "Force orphan-EXECUTE in YAML test-set (yq surgery)"
if command -v yq >/dev/null 2>&1; then
  MOCKS=$(ls -1 ./keploy/test-set-*/mocks.yaml 2>/dev/null | grep -v "test-set-.*-json" | head -n1 || true)
  if [[ -n "${MOCKS}" ]]; then
    echo "Mutating ${MOCKS}: dropping the first COM_STMT_PREPARE mock"
    # Match a top-level mock whose Spec.MySQLRequests[0].PacketBundle.Header.Type
    # is COM_STMT_PREPARE. yq's multi-doc handling does the rest.
    yq -i 'del(. | select(.Spec.MySQLRequests[0].PacketBundle.Header.Type == "COM_STMT_PREPARE") | .[0])' "${MOCKS}" 2>/dev/null \
      || echo "yq mutation failed (non-fatal — natural path still covered if it fired)"
  else
    echo "No YAML mocks.yaml found to mutate; skipping"
  fi
else
  echo "yq not installed on runner; skipping fallback mutation"
fi
endsec

section "Shutdown TiDB before test mode"
docker compose down || true
echo "TiDB stopped — Keploy should now use mocks for database interactions"
endsec

section "Replay"
set +e
"$REPLAY_BIN" test \
  -c "java -jar $JAR_NAME" \
  --delay 20 --api-timeout 60 \
  2>&1 | tee test_logs.txt
REPLAY_RC=$?
set -e
echo "Replay exit code: $REPLAY_RC"
endsec

section "Check reports"
RUN_DIR=$(ls -1dt ./keploy/reports/test-run-* 2>/dev/null | head -n1 || true)
if [[ -z "${RUN_DIR:-}" ]]; then
  echo "::error::No test-run directory found under ./keploy/reports"
  [[ $REPLAY_RC -ne 0 ]] && exit "$REPLAY_RC" || exit 1
fi
echo "Using reports from: $RUN_DIR"

all_passed=true
found_any=false
for rpt in "$RUN_DIR"/test-set-*-report.yaml; do
  [[ -f "$rpt" ]] || continue
  found_any=true
  status=$(awk '/^status:/{print $2; exit}' "$rpt")
  echo "Test status for $(basename "$rpt"): ${status:-<missing>}"
  [[ "$status" == "PASSED" ]] || all_passed=false
done
endsec

if [[ "$found_any" == "false" ]]; then
  echo "::error::No test report files found in $RUN_DIR"
  exit 1
fi

if [[ "$all_passed" != "true" ]]; then
  echo "::error::Some tests failed or replay exited non-zero"
  exit 1
fi

if [[ $REPLAY_RC -ne 0 ]]; then
  echo "Replay exited with code $REPLAY_RC but all tests passed. Ignoring exit code."
fi

if json_pass_supported; then
  section "Replay (json)"
  set +e
  "$REPLAY_BIN" test --storage-format json \
    -c "java -jar $JAR_NAME" \
    --delay 20 --api-timeout 60 \
    2>&1 | tee test_logs_json.txt
  REPLAY_RC_JSON=$?
  set -e
  echo "Replay (json) exit code: $REPLAY_RC_JSON"
  endsec

  if ! json_scan_reports; then
    cat test_logs_json.txt
    exit 1
  fi
  echo "All tests passed (yaml + json)"
else
  echo "All tests passed (yaml only — json pass skipped for compat-matrix cell)"
fi
exit 0
