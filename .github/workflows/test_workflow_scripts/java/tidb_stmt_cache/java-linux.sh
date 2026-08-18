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

# Number of /api/blob requests driven below; the guard asserts at least this many
# blob COM_STMT_EXECUTE mocks, so the two can never drift apart.
BLOB_REQUESTS=3

# Capability gate for keploy#4262 (streamed-BLOB EXECUTE dropped at record).
# The fix lives in this build and ships in no released binary yet, so a 'latest'
# RECORD_BIN cannot capture the COM_STMT_EXECUTE at all: the recording holds the
# RESET and SEND_LONG_DATA mocks but no EXECUTE, and replaying it reproduces the
# very bug being fixed (the /api/blob tests fail with "Can not read response from
# server"). Drive the blob traffic -- and its guard -- only when the recorder is
# this build, so the record_latest_replay_build cell still validates the other
# paths instead of being permanently red. Drop this gate once the fix ships in a
# release. Mirrors the RECORD_BIN capability gate in golang/connect_tunnel.
recorder_streams_blobs() {
  case "${RECORD_BIN:-}" in
    */build/keploy|*/build-no-race/keploy) return 0 ;;
    *) return 1 ;;
  esac
}

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

  # Streamed-BLOB writes (keploy#4262). setBinaryStream makes Connector/J send
  # the value out of band and pipeline COM_STMT_RESET -> COM_STMT_SEND_LONG_DATA
  # -> COM_STMT_EXECUTE per re-execution. Driven repeatedly on purpose: the
  # RESET only appears when the statement is re-executed from the cache.
  if recorder_streams_blobs; then
    echo "=== streamed BLOB via SEND_LONG_DATA (/api/blob) ==="
    for _ in $(seq 1 "$BLOB_REQUESTS"); do curl -sS "http://localhost:8080/api/blob/2048" || true; echo; done
  else
    echo "=== streamed BLOB via SEND_LONG_DATA (/api/blob): SKIPPED ==="
    echo "RECORD_BIN (${RECORD_BIN:-unset}) predates the keploy#4262 record fix and cannot"
    echo "capture the streamed EXECUTE; not driving /api/blob."
  fi

  sleep 10
  echo "Sending SIGINT to keploy ($kp_pid) for graceful shutdown"
  sudo kill -INT "$kp_pid" 2>/dev/null || true
}

# Regression guard for keploy#4262 (streamed-BLOB EXECUTE dropped at record).
#
# Asserted on the RECORDED ARTIFACT, not just the replay report. A parameter
# sent with COM_STMT_SEND_LONG_DATA is absent from the COM_STMT_EXECUTE
# payload while still being non-NULL in the null bitmap, so a decoder that
# reads a value for every non-NULL parameter runs off the end, rejects the
# command, and the EXECUTE never reaches the recorder at all. The recording
# then contains RESET and SEND_LONG_DATA mocks but no EXECUTE, and replay
# fails with "Can not read response from server".
assert_blob_execute_recorded() {
  section "Guard: streamed-BLOB EXECUTE is recorded (keploy#4262)"
  if ! recorder_streams_blobs; then
    echo "SKIP: RECORD_BIN (${RECORD_BIN:-unset}) predates the keploy#4262 record fix, so"
    echo "      /api/blob was not driven and there is nothing to assert."
    endsec; return 0
  fi

  local mocks slds blobout blobid blobexecs
  mocks=$(find ./keploy -name 'mocks.yaml' -o -name 'mocks.json' 2>/dev/null)
  if [[ -z "$mocks" ]]; then
    echo "::error::keploy#4262 guard: no mock files under ./keploy"
    endsec; return 1
  fi

  # grep -h | wc -l rather than summing per-file -c counts: no bc dependency
  # (not installed on every runner) and it behaves the same for one file or
  # several. A mocks file that cannot be read raises grep's exit 2 through
  # pipefail into the ERR trap, so it is never mistaken for a count of zero.
  # shellcheck disable=SC2086
  slds=$(grep -h 'requestOperation: COM_STMT_SEND_LONG_DATA' $mocks 2>/dev/null | wc -l)
  if [[ "${slds:-0}" -eq 0 ]]; then
    echo "::error::keploy#4262 guard: no COM_STMT_SEND_LONG_DATA mocks — /api/blob did not"
    echo "::error::produce a streamed write, so this guard would assert nothing."
    endsec; return 1
  fi

  # Count the blob EXECUTEs specifically, rather than COM_STMT_EXECUTE across all
  # mocks: /api/kv and /api/cross contribute ~19 non-blob EXECUTEs, so a plain
  # total stays comfortably non-zero even when every blob EXECUTE has been
  # dropped — which is exactly the shape of the bug, and exactly what a recording
  # made by the pre-fix release looks like (19 EXECUTEs, 3 SEND_LONG_DATA, none
  # of them for blob_stream). An EXECUTE names its statement only by id, so learn
  # the id the "INSERT INTO blob_stream" COM_STMT_PREPARE was handed back in its
  # PREPARE_OK and count the EXECUTEs that reference it.
  #
  # Two things the obvious version gets wrong:
  #   - ids are scoped to a connection and restart at 1 on each, and connIDs in
  #     turn restart at 0 in every recording, so the key is file+connID+id. A
  #     bare id would let a second pooled connection's statement 1 be counted as
  #     the first connection's blob statement 1 (false green) and would miss blob
  #     EXECUTEs issued on that second connection (false red); dropping the file
  #     would do the same across two test-sets. connID is emitted in each mock's
  #     metadata, ahead of the requests it scopes, and is reset per document so a
  #     document without one cannot inherit a neighbour's — an inherited connID
  #     is what turns a dropped EXECUTE into a false green. The sample drives one
  #     HikariCP connection and records a single test-set today; none of this
  #     depends on that staying true.
  #   - mocks are not guaranteed to be emitted in wire order, so an EXECUTE can
  #     be read before the PREPARE naming its id. Ids are therefore resolved at
  #     END rather than inline. The one thing this cannot model is a statement id
  #     retired by COM_STMT_CLOSE and handed to a different statement later on
  #     the same connection, which needs wire order to disambiguate; TiDB and
  #     MySQL allocate ids from a monotonic per-session counter and the sample
  #     never closes a statement, so no recording here can hit it.
  # shellcheck disable=SC2086
  blobout=$(awk '
    FNR==1 || /^---$/                { mode=""; isblob=0; conn="" }
    /^ *connID: / { conn=$0; sub(/^ *connID: */, "", conn); gsub(/"/, "", conn); next }
    /packet_type: COM_STMT_PREPARE$/ { mode="prep"; isblob=0; next }
    mode=="prep" && /query: .*INSERT INTO `?blob_stream/ { isblob=1; next }
    /packet_type: COM_STMT_PREPARE_OK/ { if (mode=="prep") mode="prepok"; next }
    mode=="prepok" && /statement_id: [0-9]+$/ {
      if (isblob && !((FILENAME SUBSEP conn SUBSEP $NF) in blob)) {
        blob[FILENAME SUBSEP conn SUBSEP $NF] = 1
        ids = (ids == "" ? "" : ids "/") conn ":" $NF
      }
      mode=""; next
    }
    /packet_type: COM_STMT_EXECUTE/  { mode="exec"; next }
    mode=="exec" && /statement_id: [0-9]+$/ { seen[++k] = FILENAME SUBSEP conn SUBSEP $NF; mode=""; next }
    END {
      for (i = 1; i <= k; i++) if (seen[i] in blob) n++
      print (ids == "" ? "none" : ids), n+0
    }
  ' $mocks)
  read -r blobid blobexecs <<<"$blobout"

  if [[ "$blobid" == "none" ]]; then
    echo "::error::keploy#4262 guard: no COM_STMT_PREPARE for 'INSERT INTO blob_stream' was"
    echo "::error::recorded, so blob EXECUTEs cannot be told apart from the other statements'."
    endsec; return 1
  fi
  # -lt, not -ne: the only failure keploy#4262 can produce is under-recording, and
  # an overcount would not be that bug.
  if [[ "$blobexecs" -lt "$BLOB_REQUESTS" ]]; then
    echo "::error::keploy#4262 regression: ${BLOB_REQUESTS} /api/blob requests produced only"
    echo "::error::${blobexecs} COM_STMT_EXECUTE mocks for the blob_stream statement"
    echo "::error::(conn:stmt ${blobid}), alongside ${slds} COM_STMT_SEND_LONG_DATA mocks. The"
    echo "::error::streamed-BLOB EXECUTE is being dropped at decode time; replay will fail"
    echo "::error::with 'Can not read response from server'."
    endsec; return 1
  fi
  echo "OK: ${blobexecs}/${BLOB_REQUESTS} COM_STMT_EXECUTE mocks for blob_stream"
  echo "    (conn:stmt ${blobid}) alongside ${slds} COM_STMT_SEND_LONG_DATA mocks."
  endsec
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

# Before any mock mutation below, while the recording is still pristine.
assert_blob_execute_recorded

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
