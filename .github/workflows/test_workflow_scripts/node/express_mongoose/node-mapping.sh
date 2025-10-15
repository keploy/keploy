#!/usr/bin/env bash
# Safe, chatty CI script for Node + Mongo + Keploy

set -Eeuo pipefail

section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

die() {
  rc=$?
  echo "::error::Pipeline failed (exit=$rc). Dumping context…"
  echo "== docker ps =="; docker ps || true
  echo "== mongo logs (complete) =="; docker logs mongoDb || true
  echo "== workspace tree (depth 3) =="; find . -maxdepth 3 -type d -print | sort || true
  echo "== keploy tree (depth 4) =="; find ./keploy -maxdepth 4 -type f -print 2>/dev/null | sort || true
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
  endsec; return 1
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
  else
    echo "good!App started"
  fi

  curl "http://localhost:8000/students" || true

  curl -sS --request POST --url http://localhost:8000/students \
    --header 'content-type: application/json' \
    --data '{"_id":"68c81060905efc883765d155","name":"John Doe","email":"john@xyiz.com","phone":"0123456799"}' || true

  curl -sS --request PATCH --url http://localhost:8000/student/68c81060905efc883765d155 \
    --header 'content-type: application/json' \
    --data '{"_id":"68c81060905efc883765d155","name":"John Doe","email":"ayush@xyiz.com","phone":"012345679999"}' || true

  curl -sS --request DELETE --url http://localhost:8000/student/68c81060905efc883765d155 || true

  curl -sS http://localhost:8000/students || true

  sleep 10
  echo "$kp_pid Keploy PID"
  echo "Killing keploy"
  sudo kill "$kp_pid" 2>/dev/null || true
}

run_templatize() {
  local idx="$1"
  local extra_args="${2:-}"
  local logfile="templatize_logs${idx}.txt"

  section "Templatize #$idx (args: ${extra_args:-<none>})"
  set +e
  sudo -E env PATH="$PATH" "$RECORD_BIN" templatize $extra_args \
    > "$logfile" 2>&1
  local rc=$?
  set -e
  echo "Templatize #$idx exit code: $rc"
  cat "$logfile" || true

  if [[ $rc -ne 0 ]]; then
    echo "::warning::Keploy templatize exited non-zero (iteration $idx)"
  else
    echo "Templatize completed successfully for iteration $idx"
  fi

  endsec
  return "$rc"
}

run_rerecord() {
  local idx="$1"
  local extra_args="${2:-}"
  local logfile="rerecord_logs${idx}.txt"

  section "Rerecord #$idx (args: ${extra_args:-<none>})"
  
  # Start keploy rerecord in background, capture PID
  set +e
  sudo -E env PATH="$PATH" "$RECORD_BIN" rerecord -c 'npm start' --disable-mapping=false $extra_args \
    > "$logfile" 2>&1 &
  local KEPLOY_PID=$!

  # Wait + capture rc
  wait "$KEPLOY_PID"
  local rc=$?
  set -e
  echo "Rerecord #$idx exit code: $rc"
  cat "$logfile" || true

  if [[ $rc -ne 0 ]]; then
    echo "::warning::Keploy rerecord exited non-zero (iteration $idx)"
  else
    echo "Rerecord completed successfully for iteration $idx"
  fi

  # Check for data races and errors
  if grep -q "WARNING: DATA RACE" "$logfile"; then
    echo "::error::Data race detected in $logfile"
    exit 1
  fi
  if grep -q "ERROR" "$logfile"; then
    echo "::warning::Errors found in $logfile"
  fi

  echo "== keploy artifacts after rerecord (depth 3) =="
  find ./keploy -maxdepth 3 -type f | sort || true

  endsec
  return "$rc"
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

for i in 1 2; do
  section "Record iteration $i"
  app_name="nodeApp_${i}"

  # Start keploy recording in background, capture PID
  sudo -E env PATH="$PATH" "$RECORD_BIN" record -c 'npm start' --global-passthrough \
    > "${app_name}.txt" 2>&1 &
  KEPLOY_PID=$!

  # Drive traffic and stop keploy
  send_request "$KEPLOY_PID"

  # Wait + capture rc
  set +e
  wait "$KEPLOY_PID"
  rc=$?
  set -e
  echo "Record exit code: $rc"
  [[ $rc -ne 0 ]] && echo "::warning::Keploy record exited non-zero (iteration $i)"

  echo "== keploy artifacts (depth 3) =="
  find ./keploy -maxdepth 3 -type f | sort || true

  if [[ ! -d "./keploy/test-set-$((i-1))" ]]; then
  echo "::error::keploy/test-set-$((i-1)) folder does not exist. Failing pipeline."
  exit 1
  fi

  if grep -q "WARNING: DATA RACE" "${app_name}.txt"; then
    echo "::error::Data race detected in ${app_name}.txt"
    cat "${app_name}.txt"
    exit 1
  fi
  if grep -q "ERROR" "${app_name}.txt"; then
    echo "::warning::Errors found in ${app_name}.txt"
    cat "${app_name}.txt"
  fi

  endsec
  echo "Recorded test case and mocks for iteration ${i}"
done



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
  sudo -E env PATH="$PATH" "$REPLAY_BIN" test -c 'npm start' --disable-mapping=false --delay 10 $extra_args 
    # > "$logfile" 2>&1
  local rc=$?
  set -e
  echo "Replay #$idx exit code: $rc"
  cat "$logfile" || true

  # Find newest run dir and print set statuses
  local RUN_DIR
  RUN_DIR=$(ls -1dt ./keploy/reports/test-run-* 2>/dev/null | head -n1 || true)
  if [[ -z "${RUN_DIR:-}" ]]; then
    echo "::error::No test-run directory found after replay #$idx"
    return "$rc"
  fi
  echo "Using reports from: $RUN_DIR"
  local any_fail=false
  for rpt in "$RUN_DIR"/test-set-*-report.yaml; do
    [[ -f "$rpt" ]] || continue
    local test_status
    test_status=$(awk '/^status:/{print $2; exit}' "$rpt")
    echo "Test status for $(basename "$rpt"): ${test_status:-<missing>}"
    if [[ "$test_status" != "PASSED" ]]; then any_fail=true; fi
  done
  endsec

  if $any_fail; then
    return 1
  else
    return "$rc"
  fi
}

run_templatize 1

echo "Starting MongoDB for replay operation because of global passthrough"
docker run --name mongoDb --rm -p 27017:27017 -d mongo
wait_for_mongo
run_replay 1 --global-passthrough

run_rerecord 1 --amend-testset

section "Shutdown MongoDB before test mode"
docker stop mongoDb || true
docker rm mongoDb || true
echo "MongoDB stopped - Keploy should now use mocks for database interactions"
endsec

run_replay 2

# enable selected tests in keploy.yml (guarded)
if [[ -f "./keploy.yml" ]]; then
  sed -i 's/selectedTests: {}/selectedTests: {"test-set-0": ["test-1", "test-2"]}/' "./keploy.yml" || true
else
  echo "::warning::keploy.yml missing; cannot set selectedTests"
fi

run_replay 3

echo "All replays completed. If no errors above, CI can PASS."
exit 0