#!/usr/bin/env bash

# safer bash, but we’ll locally disable -e around commands we want to inspect
set -Eeuo pipefail

cat main.go

# ----- helpers -----
section()  { echo "::group::$*"; }
endsec()   { echo "::endgroup::"; }
die() {
  rc=$?
  echo "::error::Pipeline failed (exit=$rc). Dumping context…"
  echo "== docker ps =="; docker ps || true
  echo "== mysql logs (complete) =="; docker logs mysql-container || true
  echo "== workspace tree (depth 3) =="; find . -maxdepth 3 -type d -print | sort || true
  echo "== keploy tree (depth 4) =="; find ./keploy -maxdepth 4 -type f -print 2>/dev/null | sort || true
  echo "== *.txt logs (complete) =="; for f in ./*.txt; do [[ -f "$f" ]] && { echo "--- $f ---"; cat "$f"; }; done
  [[ -f test_logs.txt ]] && { echo "== test_logs.txt (complete) =="; cat test_logs.txt; }
  exit "$rc"
}
trap die ERR

wait_for_mysql() {
  section "Wait for MySQL readiness"
  # ping until mysqld accepts connections
  for i in {1..60}; do
    if docker exec mysql-container mysql -uroot -ppassword -e "SELECT 1" >/dev/null 2>&1; then
      echo "MySQL is ready."
      endsec; return 0
    fi
    sleep 1
  done
  echo "::error::MySQL did not become ready in time"
  endsec; return 1
}

send_request() {
  local kp_pid="$1"

  # Wait for the app to report healthy
  for i in {1..60}; do
    if curl -fsS http://localhost:9090/healthcheck >/dev/null; then
      echo "good!App started"
      break
    fi
    sleep 1
  done

  echo "== Seed special datetime rows =="
  curl -sS -X POST http://localhost:9090/seed/dates || true

  echo "== Basic flows from original script =="
  curl -sS -X POST http://localhost:9090/shorten -H "Content-Type: application/json" \
    -d '{"url": "https://github.com"}' || true
  # keep one resolve from the old tests
  curl -sS http://localhost:9090/resolve/4KepjkTT || true

  echo "== Query by exact end_time timestamps =="
  # 1) RFC3339 min-sentinel like "9999-01-01T00:00:00Z"
  curl -sS "http://localhost:9090/query/by-endtime?ts=9999-01-01T00:00:00Z" || true

  # 2) RFC3339 max-sentinel with microseconds
  curl -sS "http://localhost:9090/query/by-endtime?ts=9999-12-31T23:59:59.999999Z" || true

  # 3) MySQL-style with space "1970-01-01 00:00:00"
  curl -sS "http://localhost:9090/query/by-endtime?ts=1970-01-01%2000:00:00" || true

  # 4) Lower bound valid (1000-01-01)
  curl -sS "http://localhost:9090/query/by-endtime?ts=1000-01-01T00:00:00Z" || true

  # 5) Leap second-ish / leap day example
  curl -sS "http://localhost:9090/query/by-endtime?ts=2020-02-29T12:34:56Z" || true

  # 6) Offset input (should normalize to UTC in response)
  #    First with explicit offset in the query param (needs URL-encoding for '+')
  curl -sS "http://localhost:9090/query/by-endtime?ts=2023-07-01T18:30:00%2B05:30" || true
  #    And the UTC-equivalent time (13:00:00Z)
  curl -sS "http://localhost:9090/query/by-endtime?ts=2023-07-01T13:00:00Z" || true

  echo "== Sentinel pair =="
  curl -sS http://localhost:9090/query/sentinels || true

  echo "== Lookup by label (short_code) =="
  # leap case present in the seed
  curl -sS http://localhost:9090/query/label/dt-leap-2020-02-29T12:34:56Z || true
  # also try resolving by the same short_code via /resolve
  curl -sS http://localhost:9090/resolve/dt-leap-2020-02-29T12:34:56Z || true

  echo "== List all seeded date rows =="
  curl -sS http://localhost:9090/query/dates || true

  echo "== Active rows (non-expired) =="
  curl -sS http://localhost:9090/query/active || true

  # Give Keploy a moment to persist artifacts, then stop it cleanly
  sleep 10
  echo "$kp_pid Keploy PID"
  echo "Killing Keploy"
  sudo kill "$kp_pid" 2>/dev/null || true
}

run_record_iteration() {
  local idx="$1"
  local app_name="urlShort_${idx}"

  section "Record iteration $idx"

  # Clean slate per run
  rm -rf keploy/ keploy.yml || true

  # Start mysql (once) only for first iteration
  if ! docker ps --format '{{.Names}}' | grep -q '^mysql-container$'; then
    docker run --name mysql-container -e MYSQL_ROOT_PASSWORD=password -e MYSQL_DATABASE=uss \
      -p 3306:3306 --rm -d mysql:latest
    wait_for_mysql
  fi

  # Generate config
  sudo "$RECORD_BIN" config --generate
  sed -i 's/global: {}/global: {"body": {"updated_at":[]}}/' ./keploy.yml

  # Build app
  go build -o urlShort

  # Start recording in background so we capture its PID explicitly
  sudo -E env PATH="$PATH" "$RECORD_BIN" record -c "./urlShort" --generateGithubActions=false \
    > "${app_name}.txt" 2>&1 & 
  local KEPLOY_PID=$!

  # Drive traffic + stop keploy
  send_request "$KEPLOY_PID"

  # Wait for keploy exit and capture code
  set +e
  wait "$KEPLOY_PID"
  local rc=$?
  set -e
  echo "Record exit code: $rc"
  if [[ $rc -ne 0 ]]; then
    echo "::error::Keploy record exited with $rc (iteration $idx)"
  fi

  # Quick sanity: ensure something was written
  echo "== keploy artifacts after record =="
  find ./keploy -maxdepth 3 -type f | sort || true

  # Fail on obvious errors/races in log
  if grep -q "WARNING: DATA RACE" "${app_name}.txt"; then
    echo "::error::Data race detected in ${app_name}.txt"
    cat "${app_name}.txt"
    return 1
  fi
  if grep -q "ERROR" "${app_name}.txt"; then
    echo "::warning::Errors found in ${app_name}.txt (not fatal unless record failed)"
    cat "${app_name}.txt"
  fi

  endsec
}

# ----- main flow -----

section "Environment"
echo "RECORD_BIN: $RECORD_BIN"
echo "REPLAY_BIN : $REPLAY_BIN"
"$RECORD_BIN" version 2>/dev/null || true
"$REPLAY_BIN" version  2>/dev/null || true
endsec

for i in 1 2; do
  run_record_iteration "$i"
  echo "Recorded test case and mocks for iteration $i"
done

section "Shutdown MySQL before test mode"
# Stop MySQL container - Keploy should use mocks for database interactions
docker stop mysql-container || true
docker rm mysql-container || true
echo "MySQL stopped - Keploy should now use mocks for database interactions"
endsec

section "Replay"
# Run replay but DON'T crash the step; capture rc and print logs
set +e
sudo -E env PATH="$PATH" "$REPLAY_BIN" test -c "./urlShort" --delay 7 --generateGithubActions=false \
  > test_logs.txt 2>&1
REPLAY_RC=$?
set -e
echo "Replay exit code: $REPLAY_RC"
cat test_logs.txt || true
endsec

# If replay failed, still try to read reports to say which set failed
section "Check reports"
# Find the most recent test-run dir (don’t assume test-run-0)
RUN_DIR=$(ls -1dt ./keploy/reports/test-run-* 2>/dev/null | head -n1 || true)
if [[ -z "${RUN_DIR:-}" ]]; then
  echo "::error::No test-run directory found under ./keploy/reports"
  [[ $REPLAY_RC -ne 0 ]] && exit "$REPLAY_RC" || exit 1
fi

echo "Using reports from: $RUN_DIR"
all_passed=true
for rpt in "$RUN_DIR"/test-set-*-report.yaml; do
  [[ -f "$rpt" ]] || continue
  status=$(awk '/^status:/{print $2; exit}' "$rpt")
  echo "Test status for $(basename "$rpt"): ${status:-<missing>}"
  if [[ "$status" != "PASSED" ]]; then
    all_passed=false
  fi
done
endsec

if [[ "$all_passed" == "true" && $REPLAY_RC -eq 0 ]]; then
  echo "All tests passed"
  exit 0
fi

echo "::error::Some tests failed or replay exited non-zero"
exit 1