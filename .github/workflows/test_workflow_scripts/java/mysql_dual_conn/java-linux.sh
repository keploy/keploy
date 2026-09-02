#!/usr/bin/env bash

source "${GITHUB_WORKSPACE:-${PWD%/samples-*}}/.github/workflows/test_workflow_scripts/docker-build-retry.sh"

# E2E test for MySQL dual-connection handshake matching.
#
# Validates that Keploy correctly matches HandshakeResponse41 packets when
# an application uses multiple MySQL connection pools with different
# credentials, databases, and JDBC URL parameters (causing different
# capability flags). Without the fix, the second pool's handshake fails
# with: "no mysql mocks matched the HandshakeResponse41"

set -Eeuo pipefail

section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

die() {
  rc=$?
  echo "::error::Pipeline failed (exit=$rc). Dumping context…"
  echo "== docker ps =="; docker ps || true
  echo "== mysql logs (last 200 lines) =="; docker compose logs --tail 200 mysql || true
  echo "== workspace tree (depth 3) =="; find . -maxdepth 3 -type d -print | sort || true
  echo "== keploy tree (depth 4) =="; find ./keploy -maxdepth 4 -type f -print 2>/dev/null | sort | head -n 20 || true; echo "... (truncated)"
  echo "== *.txt logs (last 100 lines) =="; for f in ./*.txt; do [[ -f "$f" ]] && { echo "--- $f ---"; tail -n 100 "$f"; }; done
  exit "$rc"
}
trap die ERR

wait_for_mysql() {
  section "Wait for MySQL readiness"
  for i in {1..120}; do
    if docker compose exec -T mysql mysql -uroot -prootpass -e "SELECT 1" >/dev/null 2>&1; then
      echo "MySQL is ready."
      endsec; return 0
    fi
    sleep 1
  done
  echo "::error::MySQL did not become ready in time"
  endsec; return 1
}

wait_for_app() {
  section "Wait for app HTTP port"
  for i in {1..60}; do
    if curl -sS http://localhost:8080/api/oms -o /dev/null 2>/dev/null; then
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

    echo "Maven build failed on attempt ${attempt}/3. The script will retry automatically; if all attempts fail, review mvn_build.log for the root cause."
    if [[ "$attempt" -lt 3 ]]; then
      sleep $((attempt * 10))
    fi
  done

  echo "::error::Maven build failed after 3 attempts. Review mvn_build.log to inspect the output from all attempts and identify the cause."
  return 1
}

send_request() {
  local kp_pid="$1"

  wait_for_app

  echo "=== Query both databases (triggers dual-handshake) ==="
  curl -sS http://localhost:8080/api/query-both || true

  echo "=== Query OMS only ==="
  curl -sS http://localhost:8080/api/oms || true

  echo "=== Query Camunda only ==="
  curl -sS http://localhost:8080/api/camunda || true

  echo "=== Query both again (second round) ==="
  curl -sS http://localhost:8080/api/query-both || true

  # Exercises the COM_STMT_RESET synthetic-OK fallback (keploy#4217).
  # Re-executes a server-prepared statement 5 times on the same JDBC
  # connection; Connector/J 8.x emits COM_STMT_RESET between executions.
  echo "=== /api/oms/stmt-reset/5 (trigger COM_STMT_RESET) ==="
  curl -sS http://localhost:8080/api/oms/stmt-reset/5 || true

  # Column-type fidelity (keploy#4426). Both endpoints bind a parameter,
  # so Connector/J issues COM_STMT_EXECUTE and MySQL answers with a
  # BINARY-protocol result set — the format whose FLOAT/DOUBLE columns
  # carry raw IEEE-754 bytes. An unparameterised SELECT would come back
  # as a text result set (every value a string) and would not touch this
  # code path at all.
  #
  # Gated: see the MYSQL_NUMERIC_FIDELITY note at the guard below.
  if [[ "${MYSQL_NUMERIC_FIDELITY:-}" == "true" ]]; then
    echo "=== /api/oms/numerics (FLOAT/DOUBLE/BIGINT UNSIGNED, binary rows) ==="
    curl -sS http://localhost:8080/api/oms/numerics || true

    # Binds a FLOAT parameter: float32 on the wire, float64 out of the
    # mock file. Exercises the COM_STMT_EXECUTE parameter decode and the
    # matcher's mixed-width float comparison.
    echo "=== /api/oms/float-param/9.99 (FLOAT bound parameter) ==="
    curl -sS http://localhost:8080/api/oms/float-param/9.99 || true
  fi

  # Let keploy persist, then gracefully stop it
  sleep 10
  echo "$kp_pid Keploy PID"
  echo "Sending SIGINT to keploy for graceful shutdown"
  sudo kill -INT "$kp_pid" 2>/dev/null || true
  # Caller waits on the PID — don't reap here to avoid double-wait
}

# --- Main ---

source "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/test-iid.sh"

# Clean slate
sudo rm -rf keploy/ keploy.yml

section "Start MySQL"
docker_compose_pull_retry
docker compose up -d
wait_for_mysql
endsec

section "Build"
source "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/update-java.sh"
run_maven_build
endsec

JAR_NAME=$(ls target/mysql-dual-conn-*.jar 2>/dev/null | head -n1)
if [[ -z "$JAR_NAME" ]]; then
  echo "::error::JAR not found after build"
  exit 1
fi

do_record_iteration() {
  local i="$1"
  local extra_flags="${2:-}"
  local label="${extra_flags:+_json}"
  local app_name="dualConn_${i}${label}"
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

# --- Regression guard for keploy#4426 (FLOAT/DOUBLE decoded as bit patterns) ---
# This one has to be asserted on the RECORDED ARTIFACT, not on the replay
# report. The defect corrupted values at record time: a FLOAT column
# holding 9.99 was written to the mock as 1.0926057e+09 and a DOUBLE as
# 4.621813488089437e+18, because the wire bytes were converted numerically
# instead of reinterpreted as IEEE-754. Record and replay were wrong in the
# same direction, so a report-status check alone can pass while every
# recorded row is nonsense.
#
# Compat matrix: this needs BOTH binaries to carry the fix. A record
# binary without it writes the bit pattern into the mock; a replay
# binary without it type-asserts ce.Value.(float32) on a correctly
# recorded FLOAT and panics, tearing down the connection. So a
# mixed-version cell cannot exercise the fixture either way, and
# MYSQL_NUMERIC_FIDELITY is set only on record_build_replay_build —
# exactly the pattern mysql_auto_port uses for port detection.
#
# There is no CLI capability to probe here (the fix adds no flag), so
# the matrix cell states the fact directly rather than a probe guessing
# at it. The flag being unset therefore means "skewed cell", never
# "feature missing", so this cannot silently go green on the cell that
# is supposed to assert.
section "Guard: FLOAT/DOUBLE column fidelity (keploy#4426)"
if [[ "${MYSQL_NUMERIC_FIDELITY:-}" != "true" ]]; then
  echo "Skipping: FLOAT/DOUBLE column fidelity needs BOTH the record and replay"
  echo "binaries to carry the keploy#4426 fix. Expected for cross-version cells."
  endsec
else
  # Closes the ::group:: before exiting, so the ::error:: annotations
  # don't render inside a collapsed section.
  guard_fail() {
    echo "::error::$*"
    endsec
    exit 1
  }

  mapfile -t mock_files < <(find ./keploy \( -name 'mocks.yaml' -o -name 'mocks.json' \) | sort)
  if [[ ${#mock_files[@]} -eq 0 ]]; then
    guard_fail "keploy#4426 guard: no mock files found under ./keploy"
  fi

  # The negative check runs on every file. The positive checks run only on
  # files that actually captured the fixture, so adding a future test-set
  # that doesn't hit /api/oms/numerics can't turn this into a false
  # failure — but at least one file must carry it, or the negative check
  # would be vacuous.
  #
  # Per file rather than across the set: the YAML and JSON storage formats
  # decode numbers through different code, so a JSON-only regression must
  # not be masked by the YAML file happening to hold the right value.
  fixture_seen=false
  for mf in "${mock_files[@]}"; do
    echo "--- checking $mf"

    # The exact corruptions this bug produced for a column holding 9.99.
    if grep -qE '1\.0926057e\+09|4\.621813488089437e\+18' "$mf"; then
      grep -nE '1\.0926057e\+09|4\.621813488089437e\+18' "$mf" | head -10
      guard_fail "keploy#4426 regression in $mf: a FLOAT/DOUBLE column was recorded as its IEEE-754 bit pattern"
    fi

    # Optional space after the colon so this keeps working if the JSON
    # mock writer ever gains SetIndent.
    if ! grep -qE '"?table"?: ?"?numeric_fidelity"?' "$mf"; then
      echo "    (no numeric_fidelity rows in this file — skipping positive checks)"
      continue
    fi
    fixture_seen=true

    if ! grep -qE '"?name"?: ?"?price_f"?' "$mf"; then
      guard_fail "keploy#4426 guard: numeric_fidelity present in $mf but no price_f column"
    fi

    # Unquoted 9.99. A quoted "9.99" means the row arrived as a TEXT result
    # set, whose values are length-encoded strings — that path never touches
    # the binary decode this guard exists to protect.
    if ! grep -qE '(value: 9\.99$|"value": ?9\.99)' "$mf"; then
      grep -nE '"?name"?: ?"?(price_f|ratio_d)"?' -A 1 "$mf" | head -20
      guard_fail "keploy#4426 guard: no unquoted 9.99 in $mf — a quoted \"9.99\" means a TEXT result set, which does not exercise the binary decode path"
    fi

    # BIGINT UNSIGNED above MaxInt64 has no lossless float64 form, so a
    # format that routes it through one collapses distinct rows onto the
    # same number (18446744073709551615 -> 18446744073709552000, or
    # -9223372036854775808 once that float is narrowed to an integer).
    if ! grep -q '18446744073709551615' "$mf"; then
      grep -nE '"?name"?: ?"?big_u"?' -A 1 "$mf" | head -20
      guard_fail "keploy#4426 guard: BIGINT UNSIGNED max lost precision in $mf"
    fi
  done

  if [[ "$fixture_seen" != "true" ]]; then
    guard_fail "keploy#4426 guard: no mock file captured the numeric_fidelity table — /api/oms/numerics was never recorded, so this guard asserted nothing"
  fi
  echo "OK: FLOAT/DOUBLE/BIGINT UNSIGNED columns recorded with their real values."
  endsec
fi

section "Shutdown MySQL before test mode"
docker compose down || true
echo "MySQL stopped — Keploy should now use mocks for database interactions"
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

# --- Regression guard for keploy#4372 (MySQL system-variable matcher) ---
# Connector/J issues a live `SELECT @@session.transaction_isolation` during pool
# setup. Before #4372 an unrecorded read of it could be cross-served a DIFFERENT
# system variable's mock, and Connector/J then threw "Could not map transaction
# isolation '<value>'" and the pool never initialised. The report status alone
# would not always surface this, so assert the marker never appears in the replay
# log. This is additive — it does not alter the existing report-status gate above.
section "Guard: system-variable matcher (keploy#4372)"
if grep -qi "Could not map transaction isolation" test_logs.txt 2>/dev/null; then
  echo "::error::keploy#4372 regression: 'Could not map transaction isolation' in replay log — a system-variable read was cross-served the wrong mock"
  grep -i "Could not map transaction isolation" test_logs.txt | head -5
  exit 1
fi
echo "OK: no transaction-isolation cross-match marker in replay log."
endsec

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
