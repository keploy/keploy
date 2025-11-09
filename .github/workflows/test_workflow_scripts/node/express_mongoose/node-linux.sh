#!/usr/bin/env bash
# Safe, chatty CI script for Node + Mongo + Keploy (fail-fast, reference-aligned)

set -Eeuo pipefail
set -o errtrace

section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

die() {
  rc=$?
  echo "::error::Pipeline failed (exit=$rc). Dumping context…"
  echo "== docker ps =="; docker ps || true
  echo "== mongo logs (complete) =="; docker logs mongoDb || true
  echo "== workspace tree (depth 3) =="; find . -maxdepth 3 -type d -print | sort || true
  echo "== keploy tree (depth 4) =="; find ./keploy -maxdepth 4 -type f -print 2>/dev/null | sort || true
  echo "== keploy_agent logs (complete) =="; for f in keploy_agent*; do [[ -f "$f" ]] && { echo "--- $f ---"; cat "$f"; }; done
  echo "== *.txt logs (complete) =="; for f in ./*.txt; do [[ -f "$f" ]] && { echo "--- $f ---"; cat "$f"; }; done
  for f in test_logs*.txt; do [[ -f "$f" ]] && { echo "== $f (complete) =="; cat "$f"; }; done
  exit "$rc"
}
trap die ERR

wait_for_mongo() {
  section "Wait for Mongo readiness"
  for i in {1..90}; do
    if docker exec mongoDb mongosh --quiet --eval "db.adminCommand('ping').ok" >/dev/null 2>&1; then
      echo "Mongo responds to ping."
      endsec; return 0
    fi
    if (echo > /dev/tcp/127.0.0.1/27017) >/dev/null 2>&1; then
      echo "Mongo TCP port open."
      endsec; return 0
    fi
    sleep 1
  done
  echo "::error::Mongo did not become ready in time"
  endsec
  return 1
}

wait_for_http() {
  local url="$1" tries="${2:-60}"
  for _ in $(seq 1 "$tries"); do
    if curl -fsS "$url" >/dev/null; then return 0; fi
    sleep 1
  done
  return 1
}

send_request() {
  local kp_pid="$1"

  if ! wait_for_http "http://localhost:8000/students" 120; then
    echo "::error::App did not become healthy at /students"
    # Let the pipeline fail by returning non-zero
    return 1
  else
    echo "good! App started"
  fi

  # Drive a bit of traffic (best-effort)
  curl -sS --request POST --url http://localhost:8000/students \
    --header 'content-type: application/json' \
    --data '{"name":"John Doe","email":"john@xyiz.com","phone":"0123456799"}' || true

  curl -sS --request POST --url http://localhost:8000/students \
    --header 'content-type: application/json' \
    --data '{"name":"Alice Green","email":"green@alice.com","phone":"3939201584"}' || true

  curl -sS http://localhost:8000/students || true
  curl -sS http://localhost:8000/get || true

  sleep 10
  echo "$kp_pid Keploy PID"
  echo "Killing keploy"
  sudo kill "$kp_pid" 2>/dev/null || true
}

# ----- main -----

# Load test scripts and start MongoDB container
source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

section "Start Mongo"
docker run --name mongoDb --rm -p 27017:27017 -d mongo
wait_for_mongo
endsec

section "Prepare app"
npm ci || npm install
sed -i "s/mongoDb:27017/localhost:27017/" "src/db/connection.js"
rm -rf keploy/
[[ -f "./keploy.yml" ]] && rm ./keploy.yml

# Generate the keploy-config file.
sudo "$RECORD_BIN" config --generate

# Update the global noise to page (ignore changes to this field)
config_file="./keploy.yml"
sed -i 's/global: {}/global: {"body": {"page":[]}}/' "$config_file"
endsec

sudo "$RECORD_BIN" agent \
 > keploy_agent_record.log 2>&1 &
AGENT_PID=$!
echo "Keploy Agent PID: $AGENT_PID"

for i in 1 2; do
  section "Record iteration $i"
  app_name="nodeApp_${i}"

  # Start keploy recording in background, capture PID
  sudo -E env PATH="$PATH" "$RECORD_BIN" record -c 'npm start' \
    > "${app_name}.txt" 2>&1 &
  KEPLOY_PID=$!

  # Drive traffic and stop keploy (will fail the pipeline if health never comes up)
  send_request "$KEPLOY_PID"

  cat "${app_name}.txt"

  # Wait + capture rc
  set +e
  wait "$KEPLOY_PID"
  rc=$?
  set -e
  echo "Record exit code: $rc"

  # Fail hard like the reference script
  if grep -q "WARNING: DATA RACE" "${app_name}.txt"; then
    echo "::error::Data race detected in ${app_name}.txt"
    cat "${app_name}.txt"
    exit 1
  fi
  if grep -q "ERROR" "${app_name}.txt"; then
    echo "::error::Error found during recording (iteration $i)"
    cat "${app_name}.txt"
    exit 1
  fi
  if [[ $rc -ne 0 ]]; then
    echo "::error::Keploy record exited non-zero (iteration $i)"
    cat "${app_name}.txt" || true
    exit "$rc"
  fi

  echo "== keploy artifacts (depth 3) =="
  find ./keploy -maxdepth 3 -type f | sort || true

  # Ensure at least one test/mocks were produced for this iteration
  if ! find ./keploy -type f -name 'test-*.yaml' -o -name 'mocks-*.yaml' | grep -q .; then
    echo "::error::No tests/mocks produced in iteration $i"
    cat "${app_name}.txt" || true
    exit 1
  fi

  endsec
  echo "Recorded test case and mocks for iteration ${i}"
done

# Optional tweak to a mock; guard if file exists
mocks_file="keploy/test-set-0/tests/test-5.yaml"
if [[ -f "$mocks_file" ]]; then
  sed -i 's/"page":1/"page":4/' "$mocks_file"
else
  echo "::warning::$mocks_file not found; skipping page change"
fi

# ---- Replays ----
# Shutdown MongoDB before test mode to verify Keploy mocking works correctly
section "Shutdown MongoDB before test mode"
docker stop mongoDb || true
docker rm mongoDb || true
echo "MongoDB stopped - Keploy should now use mocks for database interactions"
endsec

run_replay() {
  local idx="$1"
  local extra_args="${2:-}"
  local logfile="test_logs${idx}.txt"

  section "Replay #$idx (args: ${extra_args:-<none>})"
  set +e
  sudo -E env PATH="$PATH" "$REPLAY_BIN" test -c 'npm start' --delay 10 $extra_args \
    > "$logfile" 2>&1
  local rc=$?
  set -e
  echo "Replay #$idx exit code: $rc"
  cat "$logfile" || true

  # Fail on log errors like the reference
  if grep -q "WARNING: DATA RACE" "$logfile"; then
    echo "::error::Data race detected in replay #$idx"
    return 1
  fi
  if grep -q "ERROR" "$logfile"; then
    echo "::error::Error found in replay #$idx"
    return 1
  fi

  # Find newest run dir and validate reports
  local RUN_DIR
  RUN_DIR=$(ls -1dt ./keploy/reports/test-run-* 2>/dev/null | head -n1 || true)
  if [[ -z "${RUN_DIR:-}" ]]; then
    echo "::error::No test-run directory found after replay #$idx"
    return 1
  fi
  echo "Using reports from: $RUN_DIR"

  local any_fail=false
  local any_seen=false
  shopt -s nullglob
  for rpt in "$RUN_DIR"/test-set-*-report.yaml; do
    any_seen=true
    local status
    status=$(awk '/^status:/{print $2; exit}' "$rpt")
    echo "Test status for $(basename "$rpt"): ${status:-<missing>}"
    if [[ -z "${status:-}" || "$status" != "PASSED" ]]; then
      any_fail=true
    fi
  done

  local coverage_file="$RUN_DIR/coverage.yaml"
  if [[ -f "$coverage_file" ]]; then
    echo "✅ Coverage file found: $coverage_file"
  else
    echo "::error::Coverage file not found in $RUN_DIR"
    return 1
  fi

  # ✅ Extract and validate coverage percentage from log
  local coverage_line coverage_percent
  coverage_line=$(grep -Eo "Total Coverage Percentage:[[:space:]]+[0-9]+(\.[0-9]+)?%" "$logfile" | tail -n1 || true)

  if [[ -z "$coverage_line" ]]; then
    echo "::error::No coverage percentage found in $logfile"
    return 1
  fi

  coverage_percent=$(echo "$coverage_line" | grep -Eo "[0-9]+(\.[0-9]+)?" || echo "0")
  echo "📊 Extracted coverage: ${coverage_percent}%"

  # Compare coverage with threshold (40%)
  if (( $(echo "$coverage_percent < 40" | bc -l) )); then
    echo "::error::Coverage below threshold (40%). Found: ${coverage_percent}%"
    return 1
  else
    echo "✅ Coverage meets threshold (>= 40%)"
  fi

  shopt -u nullglob

  if ! $any_seen; then
    echo "::error::No test-set reports found in $RUN_DIR"
    return 1
  fi

  endsec

  if $any_fail; then
    return 1
  else
    return "$rc"
  fi
}

# Replays (will fail pipeline if any returns non-zero due to set -e)
run_replay 1
run_replay 2 "--testsets test-set-0"

# enable selected tests in keploy.yml (guarded)
if [[ -f "./keploy.yml" ]]; then
  sed -i 's/selectedTests: {}/selectedTests: {"test-set-0": ["test-1", "test-2"]}/' "./keploy.yml" || true
else
  echo "::warning::keploy.yml missing; cannot set selectedTests"
fi

run_replay 3 "--apiTimeout 30"

echo "All replays completed and PASSED."
exit 0
