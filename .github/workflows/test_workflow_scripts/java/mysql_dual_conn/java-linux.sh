#!/usr/bin/env bash

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
sleep 5

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
exit 0
