#!/usr/bin/env bash
# Safe, chatty CI script for Node + Mongo + Keploy

set -Eeuo pipefail

section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

die() {
  rc=$?
  echo "::error::Pipeline failed (exit=$rc). Dumping context…"
  echo "== docker ps =="; docker ps || true
  echo "== mongo logs (tail 200) =="; docker logs --tail 200 mongoDb || true
  echo "== workspace tree (depth 3) =="; find . -maxdepth 3 -type d -print | sort || true
  echo "== keploy tree (depth 4) =="; find ./keploy -maxdepth 4 -type f -print 2>/dev/null | sort || true
  echo "== *.txt logs (tail 200) =="; for f in ./*.txt; do [[ -f "$f" ]] && { echo "--- $f ---"; tail -n 200 "$f"; }; done
  for f in test_logs*.txt; do [[ -f "$f" ]] && { echo "== $f (tail 200) =="; tail -n 200 "$f"; }; done
  exit "$rc"
}
trap die ERR

wait_for_mongo() {
  section "Wait for Mongo readiness"
  for i in {1..90}; do
    # prefer container check
    if docker exec mongoDb mongosh --quiet --eval "db.adminCommand('ping').ok" >/dev/null 2>&1; then
      echo "Mongo responds to ping."
      endsec; return 0
    fi
    # fallback to TCP
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
  for i in $(seq 1 "$tries"); do
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

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

section "Start Mongo"
docker run --name mongoDb --rm -p 27017:27017 -d mongo
wait_for_mongo
endsec

section "Prepare app"
npm ci || npm install
sed -i "s/mongoDb:27017/localhost:27017/" "src/db/connection.js"
rm -rf keploy/
# fresh keploy.yml
[[ -f ./keploy.yml ]] && rm ./keploy.yml
sudo "$RECORD_BIN" config --generate
# Update noise safely (only if key exists)
config_file="./keploy.yml"
# shellcheck disable=SC2016
sed -i 's/global: {}/global: {"body": {"page":[]}}/' "$config_file"
endsec

for i in 1 2; do
  section "Record iteration $i"
  app_name="nodeApp_${i}"

  # Start keploy recording in background, capture PID
  sudo -E env PATH="$PATH" "$RECORD_BIN" record -c 'npm start' \
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

  if grep -q "WARNING: DATA RACE" "${app_name}.txt"; then
    echo "::error::Data race detected in ${app_name}.txt"
    tail -n 200 "${app_name}.txt"
    exit 1
  fi
  if grep -q "ERROR" "${app_name}.txt"; then
    echo "::warning::Errors found in ${app_name}.txt"
    tail -n 200 "${app_name}.txt"
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
run_replay() {
  local idx="$1" extra_args="${2:-}" logfile="test_logs${idx}.txt"

  section "Replay #$idx (args: ${extra_args:-<none>})"
  set +e
  sudo -E env PATH="$PATH" "$REPLAY_BIN" test -c 'npm start' --delay 10 $extra_args \
    > "$logfile" 2>&1
  local rc=$?
  set -e
  echo "Replay #$idx exit code: $rc"
  tail -n 200 "$logfile" || true

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
    status=$(awk '/^status:/{print $2; exit}' "$rpt")
    echo "Test status for $(basename "$rpt"): ${status:-<missing>}"
    [[ "$status" == "PASSED" ]] || any_fail=true
  done
  endsec

  $any_fail && return 1 || return "$rc"
}

run_replay 1
run_replay 2 "--testsets test-set-0"

# enable selected tests in keploy.yml (guarded)
if [[ -f "./keploy.yml" ]]; then
  sed -i 's/selectedTests: {}/selectedTests: {"test-set-0": ["test-1", "test-2"]}/' "./keploy.yml" || true
else
  echo "::warning::keploy.yml missing; cannot set selectedTests"
fi

run_replay 3 "--apiTimeout 30"

echo "All replays completed. If no errors above, CI can PASS."