#!/usr/bin/env bash
# Safe, chatty CI script for Java + Postgres + Keploy

set -Eeuo pipefail

section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

die() {
  rc=$?
  echo "::error::Pipeline failed (exit=$rc). Dumping context…"
  echo "== docker ps =="; docker ps || true
  echo "== postgres logs (tail 200) =="; docker logs --tail 200 mypostgres || true
  echo "== workspace tree (depth 3) =="; find . -maxdepth 3 -type d -print | sort || true
  echo "== keploy tree (depth 4) =="; find ./keploy -maxdepth 4 -type f -print 2>/dev/null | sort || true
  echo "== *.txt logs (tail 200) =="; for f in ./*.txt; do [[ -f "$f" ]] && { echo "--- $f ---"; tail -n 200 "$f"; }; done
  [[ -f test_logs.txt ]] && { echo "== test_logs.txt (tail 200) =="; tail -n 200 test_logs.txt; }
  exit "$rc"
}
trap die ERR

wait_for_postgres() {
  section "Wait for Postgres readiness"
  for i in {1..90}; do
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

  # Ensure app is up
  if ! wait_for_http "http://localhost:9966/petclinic/api/pettypes" 120; then
    echo "::error::App did not become healthy at /pettypes"
  else
    echo "good!App started"
  fi

  # Traffic to generate tests/mocks
  curl -sS http://localhost:9966/petclinic/api/pettypes || true
  curl -sS --request POST \
    --url http://localhost:9966/petclinic/api/pettypes \
    --header 'content-type: application/json' \
    --data '{"name":"John Doe"}' || true
  curl -sS http://localhost:9966/petclinic/api/pettypes || true
  curl -sS --request POST \
    --url http://localhost:9966/petclinic/api/pettypes \
    --header 'content-type: application/json' \
    --data '{"name":"Alice Green"}' || true
  curl -sS http://localhost:9966/petclinic/api/pettypes || true
  curl -sS --request DELETE http://localhost:9966/petclinic/api/pettypes/1 || true
  curl -sS http://localhost:9966/petclinic/api/pettypes || true

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

  # Wait for keploy and capture rc
  set +e
  wait "$KEPLOY_PID"
  rc=$?
  set -e
  echo "Record exit code: $rc"
  [[ $rc -ne 0 ]] && echo "::warning::Keploy record exited non-zero (iteration $i)"

  # Basic validation + surface issues
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

section "Replay"
set +e
sudo -E env PATH="$PATH" "$REPLAY_BIN" test \
  -c 'java -jar target/spring-petclinic-rest-3.0.2.jar' \
  --delay 20 \
  > test_logs.txt 2>&1
REPLAY_RC=$?
set -e
echo "Replay exit code: $REPLAY_RC"
tail -n 200 test_logs.txt || true
endsec

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