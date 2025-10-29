#!/usr/bin/env bash
# Safe, chatty CI script for Java + Postgres + Keploy with auto API-prefix detection

set -Eeuo pipefail

section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

die() {
  rc=$?
  echo "::error::Pipeline failed (exit=$rc). Dumping context…"
  echo "== docker ps =="; docker ps || true
  echo "== postgres logs (complete) =="; docker logs mypostgres || true
  echo "== workspace tree (depth 3) =="; find . -maxdepth 3 -type d -print | sort || true
  echo "== keploy tree (depth 4) =="; find ./keploy -maxdepth 4 -type f -print 2>/dev/null | sort || true
  echo "== *.txt logs (complete) =="; for f in ./*.txt; do [[ -f "$f" ]] && { echo "--- $f ---"; cat "$f"; }; done
  [[ -f test_logs.txt ]] && { echo "== test_logs.txt (complete) =="; cat test_logs.txt; }
  exit "$rc"
}
trap die ERR

http_code() {
  # prints HTTP status code or 000 on error
  curl -s -o /dev/null -w "%{http_code}" "$1" 2>/dev/null || echo 000
}

wait_for_postgres() {
  section "Wait for Postgres readiness"
  for i in {1..120}; do
    if docker exec mypostgres pg_isready -U petclinic -d petclinic >/dev/null 2>&1; then
      echo "Postgres is ready."
      endsec; return 0
    fi
    # Fallback probe
    docker exec mypostgres psql -U petclinic -d petclinic -c "SELECT 1" >/dev/null 2>&1 && { echo "Postgres responded."; endsec; return 0; }
    sleep 1
  done
  echo "::error::Postgres did not become ready in time"
  endsec; return 1
}

wait_for_http_port() {
  # waits for *any* HTTP response from root or actuator (not strict 200)
  local base="http://localhost:9966"
  section "Wait for app HTTP port"
  for i in {1..180}; do
    if curl -sS "${base}/" -o /dev/null || curl -sS "${base}/actuator/health" -o /dev/null; then
      echo "HTTP port responded."
      endsec; return 0
    fi
    sleep 1
  done
  echo "::error::App did not open HTTP port on 9966"
  endsec; return 1
}

detect_api_prefix() {
  # returns either /petclinic/api or /api (echo to stdout), otherwise empty
  local base="http://localhost:9966"
  local candidates=( "/petclinic/api" "/api" )
  for p in "${candidates[@]}"; do
    local code
    code=$(http_code "${base}${p}/pettypes")
    if [[ "$code" =~ ^(200|201|202|204)$ ]]; then
      echo "$p"; return 0
    fi
  done
  # If no 2xx, still check which gives *any* non-404 (e.g., 401/403 if security toggled)
  for p in "${candidates[@]}"; do
    local code
    code=$(http_code "${base}${p}/pettypes")
    if [[ "$code" != "404" && "$code" != "000" ]]; then
      echo "$p"; return 0
    fi
  done
  # Fallback to actuator presence: assume /api if actuator exists
  if [[ "$(http_code "${base}/actuator/health")" == "200" ]]; then
    echo "/api"; return 0
  fi
  echo ""
  return 1
}

send_request() {
  local kp_pid="$1"
  local base="http://localhost:9966"

  wait_for_http_port

  # Try to detect API prefix dynamically
  local API_PREFIX
  API_PREFIX=$(detect_api_prefix || true)

  if [[ -z "${API_PREFIX}" ]]; then
    echo "::warning::Could not auto-detect API prefix. Trying both /petclinic/api and /api."
    # Try both paths to maximize coverage
    local paths=( "/petclinic/api" "/api" )
    for pref in "${paths[@]}"; do
      curl -sS "${base}${pref}/pettypes" || true
      curl -sS --request POST \
        --url "${base}${pref}/pettypes" \
        --header 'content-type: application/json' \
        --data '{"name":"John Doe"}' || true
      curl -sS "${base}${pref}/pettypes" || true
      curl -sS --request POST \
        --url "${base}${pref}/pettypes" \
        --header 'content-type: application/json' \
        --data '{"name":"Alice Green"}' || true
      curl -sS "${base}${pref}/pettypes" || true
      curl -sS --request DELETE "${base}${pref}/pettypes/1" || true
      curl -sS "${base}${pref}/pettypes" || true
    done
  else
    echo "Detected API prefix: ${API_PREFIX}"
    echo "good!App started"
    curl -sS "${base}${API_PREFIX}/pettypes" || true

    curl -sS --request POST \
      --url "${base}${API_PREFIX}/pettypes" \
      --header 'content-type: application/json' \
      --data '{"name":"John Doe"}' || true

    curl -sS "${base}${API_PREFIX}/pettypes" || true

    curl -sS --request POST \
      --url "${base}${API_PREFIX}/pettypes" \
      --header 'content-type: application/json' \
      --data '{"name":"Alice Green"}' || true

    curl -sS "${base}${API_PREFIX}/pettypes" || true

    curl -sS --request DELETE "${base}${API_PREFIX}/pettypes/1" || true

    curl -sS "${base}${API_PREFIX}/pettypes" || true
  fi

  # Let keploy persist, then stop it
  sleep 10
  echo "$kp_pid Keploy PID"
  echo "Killing keploy"
  sudo kill "$kp_pid" 2>/dev/null || true
}

# ----- main -----

source ./../../../.github/workflows/test_workflow_scripts/test-iid.sh

section "Git branch"
git fetch origin
git checkout native-linux
endsec

section "Start Postgres"
docker run -d --name mypostgres -e POSTGRES_USER=petclinic -e POSTGRES_PASSWORD=petclinic \
  -e POSTGRES_DB=petclinic -p 5432:5432 postgres:15.2
wait_for_postgres
# seed DB
docker cp ./src/main/resources/db/postgresql/initDB.sql mypostgres:/initDB.sql
docker exec mypostgres psql -U petclinic -d petclinic -f /initDB.sql
endsec

section "Java setup"
source ./../../../.github/workflows/test_workflow_scripts/update-java.sh
endsec

# Clean once (keep artifacts across iterations)
sudo rm -rf keploy/

for i in 1 2; do
  section "Record iteration $i"

  # Build app (captured to log)
  mvn clean install -Dmaven.test.skip=true | tee -a mvn_build.log

  app_name="javaApp_${i}"

  # Start keploy in background, capture PID
  sudo -E env PATH="$PATH" "$RECORD_BIN" record \
    -c 'java -jar target/spring-petclinic-rest-3.0.2.jar' \
    > "${app_name}.txt" 2>&1 &
  KEPLOY_PID=$!

  # Drive traffic and stop keploy
  send_request "$KEPLOY_PID"

  # Wait for keploy exit and capture code
  set +e
  wait "$KEPLOY_PID"
  rc=$?
  set -e
  echo "Record exit code: $rc"
  [[ $rc -ne 0 ]] && echo "::warning::Keploy record exited non-zero (iteration $i)"

  # Quick sanity: ensure something was written
  echo "== keploy artifacts after record =="
  find ./keploy -maxdepth 3 -type f | sort || true

  # Surface issues from record logs
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

section "Shutdown Postgres before test mode"
# Stop Postgres container - Keploy should use mocks for database interactions
docker stop mypostgres || true
docker rm mypostgres || true
echo "Postgres stopped - Keploy should now use mocks for database interactions"
endsec

section "Replay"
set +e
sudo -E env PATH="$PATH" "$REPLAY_BIN" test \
  -c 'java -jar target/spring-petclinic-rest-3.0.2.jar' \
  --delay 20 \
  > test_logs.txt 2>&1
REPLAY_RC=$?
set -e
echo "Replay exit code: $REPLAY_RC"
cat test_logs.txt || true
endsec

# ✅ Extract and validate coverage percentage from log
coverage_line=$(grep -Eo "Total Coverage Percentage:[[:space:]]+[0-9]+(\.[0-9]+)?%" "test_logs.txt" | tail -n1 || true)

if [[ -z "$coverage_line" ]]; then
  echo "::error::No coverage percentage found in test_logs.txt"
  return 1
fi

coverage_percent=$(echo "$coverage_line" | grep -Eo "[0-9]+(\.[0-9]+)?" || echo "0")
echo "📊 Extracted coverage: ${coverage_percent}%"

# Fail if coverage ≤ 0%
if (( $(echo "$coverage_percent <= 0" | bc -l) )); then
  echo "::error::Coverage below threshold (0%). Found: ${coverage_percent}%"
  exit 1
else
  echo "✅ Coverage meets threshold (> 0%)"
fi

section "Check reports"
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
  [[ "$status" == "PASSED" ]] || all_passed=false
done
endsec

if [[ "$all_passed" == "true" && $REPLAY_RC -eq 0 ]]; then
  echo "All tests passed"
  exit 0
fi

echo "::error::Some tests failed or replay exited non-zero"
exit 1