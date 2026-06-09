#!/usr/bin/env bash

# E2E test for TiDB + MySQL Connector/J prepared-statement replay.
#
# Covers the TiDB + MySQL Connector/J prepared-statement replay cluster
# against single-node TiDB (:4000) with useServerPrepStmts=true&cachePrepStmts=true:
#
#   1. cachePrepStmts orphan EXECUTE  (/api/kv)            -> synthetic PREPARE_OK
#   2. stateful COM_QUERY read-back   (/api/kv/insert-select) -> in-window row
#   3. orphaned cross-query, same param shape (/api/cross) -> no cross-serving
#
# The script records once, then replays TWICE:
#   (a) baseline               -> exercises (2) and (3) and normal cachePrepStmts
#   (b) PREPARE-dropped mutant -> deterministically forces the orphan condition
#       (1) so the synthetic-PREPARE_OK + read-back path is exercised on every
#       run regardless of HikariCP pool timing.
#
# A regression in either path flips the test-set report to FAILED and fails
# the job loudly.

set -Eeuo pipefail

section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

die() {
  rc=$?
  echo "::error::Pipeline failed (exit=$rc). Dumping context…"
  echo "== docker ps =="; docker ps || true
  echo "== tidb logs (last 200) =="; docker compose logs --tail 200 tidb || true
  echo "== *.txt logs (last 120) =="; for f in ./*.txt; do [[ -f "$f" ]] && { echo "--- $f ---"; tail -n 120 "$f"; }; done
  exit "$rc"
}
trap die ERR

wait_for_tidb() {
  section "Wait for TiDB readiness"
  # The pingcap/tidb image ships no mysql client, so probe the SQL port's
  # status endpoint (HTTP :10080/status) instead of execing a client.
  for _ in $(seq 1 120); do
    if curl -sf http://localhost:10080/status >/dev/null 2>&1; then
      echo "TiDB is ready."; endsec; return 0
    fi
    sleep 1
  done
  echo "::error::TiDB did not become ready in time"; endsec; return 1
}

wait_for_app() {
  section "Wait for app HTTP port"
  for _ in $(seq 1 60); do
    if curl -sS http://localhost:8080/api/health -o /dev/null 2>/dev/null; then
      echo "App is responding."; endsec; return 0
    fi
    sleep 1
  done
  echo "::error::App did not start in time"; endsec; return 1
}

run_maven_build() {
  : > mvn_build.log
  for attempt in 1 2 3; do
    if { echo "== Maven build attempt ${attempt}/3 =="; mvn -B -U clean package -Dmaven.test.skip=true -q; } 2>&1 | tee -a mvn_build.log; then
      return 0
    fi
    echo "Maven build failed on attempt ${attempt}/3; retrying."
    [[ "$attempt" -lt 3 ]] && sleep $((attempt * 10))
  done
  echo "::error::Maven build failed after 3 attempts. See mvn_build.log."; return 1
}

send_request() {
  local kp_pid="$1"
  wait_for_app
  echo "=== cachePrepStmts orphan traffic (/api/kv) ==="
  for v in 1 2 3 4 5 6 7 8; do curl -sS "http://localhost:8080/api/kv/$v" || true; echo; done
  echo "=== stateful COM_QUERY read-back (/api/kv/insert-select) ==="
  for v in 100 200 300 400 500; do curl -sS "http://localhost:8080/api/kv/insert-select/$v" || true; echo; done
  echo "=== orphaned cross-query, identical param shape (/api/cross) ==="
  for v in 7 8 9; do curl -sS "http://localhost:8080/api/cross/$v" || true; echo; done
  sleep 10
  echo "Sending SIGINT to keploy ($kp_pid) for graceful shutdown"
  sudo kill -INT "$kp_pid" 2>/dev/null || true
}

# Drops the COM_STMT_PREPARE mock for "SELECT ? AS v" from every recorded
# test-set so replay must synthesize a PREPARE_OK (the orphan path, keploy#4226).
drop_prepare_mock() {
  section "Mutate mocks: drop the 'SELECT ? AS v' PREPARE to force the orphan condition"
  local dropped=0
  for mf in keploy/test-set-*/mocks.yaml; do
    [[ -f "$mf" ]] || continue
    python3 - "$mf" <<'PY'
import sys
p = sys.argv[1]
parts = open(p).read().split("\n---\n")
keep = [d for d in parts if not (("packet_type: COM_STMT_PREPARE\n" in d) and ("SELECT ? AS v" in d))]
open(p, "w").write("\n---\n".join(keep))
print(f"{p}: kept {len(keep)}/{len(parts)} docs")
PY
    dropped=1
  done
  [[ "$dropped" -eq 1 ]] || { echo "::error::no mocks.yaml found to mutate"; return 1; }
  endsec
}

replay_and_check() {
  local label="$1"
  section "Replay ($label)"
  set +e
  "$REPLAY_BIN" test -c "java -jar $JAR_NAME" --delay 20 --api-timeout 60 2>&1 | tee "test_logs_${label}.txt"
  local rc=$?
  set -e
  echo "Replay ($label) exit code: $rc"
  endsec

  section "Check reports ($label)"
  local run_dir
  run_dir=$(ls -1dt ./keploy/reports/test-run-* 2>/dev/null | head -n1 || true)
  [[ -n "${run_dir:-}" ]] || { echo "::error::No test-run dir for $label"; return 1; }
  local all_passed=true found=false
  for rpt in "$run_dir"/test-set-*-report.yaml; do
    [[ -f "$rpt" ]] || continue
    found=true
    local status; status=$(awk '/^status:/{print $2; exit}' "$rpt")
    echo "[$label] $(basename "$rpt"): ${status:-<missing>}"
    [[ "$status" == "PASSED" ]] || all_passed=false
  done
  endsec
  [[ "$found" == true ]] || { echo "::error::No reports for $label"; return 1; }
  [[ "$all_passed" == true ]] || { echo "::error::[$label] some tests FAILED"; return 1; }
  echo "[$label] all tests PASSED"
}

# --- Main ---
source "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/test-iid.sh"

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
[[ -n "$JAR_NAME" ]] || { echo "::error::JAR not found after build"; exit 1; }

section "Record"
"$RECORD_BIN" record -c "java -jar $JAR_NAME" > record.txt 2>&1 &
KEPLOY_PID=$!
send_request "$KEPLOY_PID"
set +e; wait "$KEPLOY_PID"; echo "Record exit: $?"; set -e
if grep -q "WARNING: DATA RACE" record.txt; then echo "::error::Data race during record"; cat record.txt; exit 1; fi
endsec

section "Shutdown TiDB before test mode"
docker compose down || true
echo "TiDB stopped — replay must use recorded mocks"
endsec

# Snapshot the pristine recording so the mutation phase starts from it.
cp -r keploy keploy.orig

# (a) baseline: stateful read-back + cross-query must replay correctly.
replay_and_check "baseline"

# (b) orphan mutant: drop the PREPARE mock, replay again from the snapshot.
rm -rf keploy && cp -r keploy.orig keploy
drop_prepare_mock
replay_and_check "orphan"

echo "All TiDB prepared-statement replay scenarios passed."
exit 0
