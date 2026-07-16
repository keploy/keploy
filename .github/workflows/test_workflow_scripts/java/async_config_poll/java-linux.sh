#!/usr/bin/env bash

# E2E test for the async-egress engine (keploy#4368) via the async-config-poll
# sample (keploy/samples-java).
#
# The app watches a config service in the background
# (GET /v1/buckets/app-config?watch=true&version=N) from a daemon thread — async
# relative to the ingress testcases. keploy.yml declares this as an async lane
# ("config-watch", version treated as volatile). This script records the two
# HTTP tests (/health, /rules/{useCase}) plus the MySQL + config-service egress,
# then replays with the real deps down.
#
# It runs in one of two scenarios (SCENARIO env, default "periodic"):
#
#   periodic  — the app polls on a fast interval and the stub answers each poll
#               immediately (lane type: http). Asserts the async engine SERVED
#               the watch polls with no shape drift (served >= 1, shape_flags 0).
#
#   httppoll  — the app opens a SINGLE watch poll (WATCH_ONCE) and the stub HOLDS
#               it open for a server-timeout (POLL_HOLD_SECONDS) before answering
#               (lane type: httpPoll, via keploy-httppoll.yml). Asserts the poll
#               is recorded as kind:HttpPoll with a pollDurationMs, and that at
#               replay the async engine HELD it until its resolve testcase and
#               then served it (held >= 1). NOTE: the >60s hang-watchdog
#               exemption that lets a long-held poll be recorded at all is unit-
#               tested (supervisor TestHangNotDetectedWhenSuspended); this e2e
#               uses a short hold so CI stays fast.
#
# NOTE: this sample is deliberately Spring Boot 1.5 / Java 8, so this script does
# NOT source update-java.sh (which pins Java 17). The calling workflow sets up
# Temurin 8 and exports JAVA_HOME; the app is launched via an absolute
# "$JAVA_HOME/bin/java" so it resolves after keploy self-elevates.

set -Eeuo pipefail

section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

die() {
  rc=$?
  echo "::error::Pipeline failed (exit=$rc). Dumping context…"
  echo "== docker ps =="; docker ps || true
  echo "== mysql logs (last 200) =="; docker compose logs --tail 200 mysql || true
  echo "== config-stub log =="; tail -n 50 config-stub.log 2>/dev/null || true
  echo "== *.txt logs (last 120) =="; for f in ./*.txt; do [[ -f "$f" ]] && { echo "--- $f ---"; tail -n 120 "$f"; }; done
  exit "$rc"
}
trap die ERR

APP_JAVA="${JAVA_HOME:?JAVA_HOME must be set by the workflow}/bin/java"

# --- Scenario selection ---
SCENARIO="${SCENARIO:-periodic}"
echo "Async scenario: $SCENARIO"
case "$SCENARIO" in
  periodic)
    # Fast poll + widened request window so a watch poll reliably lands inside a
    # testcase at replay (so the async lane is exercised, not just drained).
    APP_ENV="env WATCH_INTERVAL_MS=150 RULES_DELAY_MS=600"
    STUB_ENV=""
    POLL_SETTLE=4          # seconds to let background polls be recorded
    ;;
  httppoll)
    # One watch poll (WATCH_ONCE), held open by the stub for POLL_HOLD_SECONDS so
    # Keploy records a single server-timeout long-poll as kind:HttpPoll. Uses the
    # httpPoll lane config (keploy-httppoll.yml).
    APP_ENV="env WATCH_ONCE=true RULES_DELAY_MS=600"
    POLL_HOLD_SECONDS=5
    STUB_ENV="POLL_HOLD_SECONDS=${POLL_HOLD_SECONDS}"
    POLL_SETTLE=$((POLL_HOLD_SECONDS + 5))   # hold + margin for the poll to resolve & be recorded
    cp keploy-httppoll.yml keploy.yml        # activate the httpPoll lane for this run
    ;;
  *)
    echo "::error::unknown SCENARIO='$SCENARIO' (expected periodic|httppoll)"; exit 1 ;;
esac

wait_for_mysql() {
  section "Wait for MySQL (real networked server)"
  # -h127.0.0.1 forces TCP so we don't get a false-positive during MySQL's
  # init-temp-server phase (the container socket is up before :3306 is).
  for _ in $(seq 1 120); do
    if docker compose exec -T mysql mysql -h127.0.0.1 -uroot -prootpass -e "SELECT 1" >/dev/null 2>&1; then
      echo "MySQL is ready."; endsec; return 0
    fi
    sleep 1
  done
  echo "::error::MySQL did not become ready in time"; endsec; return 1
}

wait_for_config_stub() {
  section "Wait for config-service stub (:9100)"
  # watch=false so the readiness probe is never held (POLL_HOLD only holds watch=true).
  for _ in $(seq 1 30); do
    if [[ "$(curl -s -o /dev/null -w '%{http_code}' 'http://127.0.0.1:9100/v1/buckets/app-config?watch=false&version=0' 2>/dev/null)" == "200" ]]; then
      echo "config-stub is ready."; endsec; return 0
    fi
    sleep 1
  done
  echo "::error::config-stub did not become ready on :9100"; endsec; return 1
}

wait_for_app() {
  section "Wait for app HTTP port"
  for _ in $(seq 1 60); do
    if [[ "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/health 2>/dev/null)" == "200" ]]; then
      echo "App is responding."; endsec; return 0
    fi
    sleep 1
  done
  echo "::error::App did not return 200 from /health in time"; endsec; return 1
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

# --- Main ---
source "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/test-iid.sh"

# Keep keploy.yml (it declares the async lane); only drop stale recordings.
sudo rm -rf keploy

section "Start dependencies (MySQL + config-service stub)"
# MySQL init runs in the background and overlaps the Maven build below; it is
# only awaited (wait_for_mysql) just before the app boots under keploy.
docker compose up -d
( cd config-stub && go build -o /tmp/acp-config-stub . )
env ${STUB_ENV} /tmp/acp-config-stub > config-stub.log 2>&1 &
CONFIG_STUB_PID=$!
endsec
wait_for_config_stub

section "Build app"
run_maven_build
JAR=$(ls target/async-config-poll*.jar 2>/dev/null | head -n1)
[[ -n "$JAR" ]] || { echo "::error::JAR not found after build"; exit 1; }
echo "jar: $JAR"
endsec

wait_for_mysql  # ready by now — its init overlapped the build

section "Record"
"$RECORD_BIN" record -c "$APP_ENV $APP_JAVA -jar $JAR" > record.txt 2>&1 &
KEPLOY_PID=$!
wait_for_app
echo "=== drive the recorded tests ==="
curl -s -o /dev/null -w "GET /health          -> %{http_code}\n" http://127.0.0.1:8080/health
curl -s -o /dev/null -w "GET /rules/ORDER_FLOW -> %{http_code}\n" \
  -H "X-Tenant-Id: ACME" -H "X-Agent-Id: 957" http://127.0.0.1:8080/rules/ORDER_FLOW
echo "Letting the background watch poll(s) resolve & record (${POLL_SETTLE}s)…"
sleep "$POLL_SETTLE"
echo "Sending SIGINT to keploy ($KEPLOY_PID) for graceful shutdown"
sudo kill -INT "$KEPLOY_PID" 2>/dev/null || true
set +e; wait "$KEPLOY_PID"; echo "Record exit: $?"; set -e
if grep -q "WARNING: DATA RACE" record.txt; then echo "::error::Data race during record"; cat record.txt; exit 1; fi
echo "== recorded mock kinds =="; grep -aE "^kind:" keploy/test-set-0/mocks.yaml 2>/dev/null | sort | uniq -c
echo "== async-stamped mocks =="; grep -ac 'async: "true"' keploy/test-set-0/mocks.yaml 2>/dev/null || true

# httppoll: the single watch poll must be recorded as kind:HttpPoll with an
# open-duration (pollDurationMs). This is the record-side proof of the feature.
if [[ "$SCENARIO" == "httppoll" ]]; then
  hp=$(grep -acE '^kind: HttpPoll$' keploy/test-set-0/mocks.yaml 2>/dev/null || true)
  [[ "${hp:-0}" -ge 1 ]] || { echo "::error::httppoll: no kind:HttpPoll mock recorded — the long-poll was not captured"; exit 1; }
  grep -aq 'pollDurationMs:' keploy/test-set-0/mocks.yaml || { echo "::error::httppoll: recorded HttpPoll mock has no pollDurationMs"; exit 1; }
  echo "httppoll: recorded ${hp} HttpPoll mock(s) with pollDurationMs."
fi
endsec

section "Shutdown deps before test mode"
docker compose down -v || true
kill "$CONFIG_STUB_PID" 2>/dev/null || true
echo "MySQL + config-stub stopped — replay must use recorded mocks (incl. the async watch polls)"
endsec

section "Replay"
set +e
"$REPLAY_BIN" test -c "$APP_ENV $APP_JAVA -jar $JAR" --delay 25 --api-timeout 60 2>&1 | tee test_logs.txt
REPLAY_RC=$?
set -e
echo "Replay exit code: $REPLAY_RC"
endsec

section "Check test reports"
RUN_DIR=$(ls -1dt ./keploy/reports/test-run-* 2>/dev/null | head -n1 || true)
[[ -n "${RUN_DIR:-}" ]] || { echo "::error::No test-run dir found under ./keploy/reports"; exit "${REPLAY_RC:-1}"; }
echo "reports: $RUN_DIR"
all_passed=true found=false
for rpt in "$RUN_DIR"/test-set-*-report.yaml; do
  [[ -f "$rpt" ]] || continue
  found=true
  status=$(awk '/^status:/{print $2; exit}' "$rpt")
  echo "$(basename "$rpt"): ${status:-<missing>}"
  [[ "$status" == "PASSED" ]] || all_passed=false
done
[[ "$found" == true ]] || { echo "::error::No test report files found in $RUN_DIR"; exit 1; }
[[ "$all_passed" == true ]] || { echo "::error::Some tests FAILED"; exit 1; }
echo "All ingress tests PASSED"
endsec

section "Check async-egress verdict"
# The engine prints e.g.: async egress verdict {"served": 2, "shape_flags": 0, "not_exercised": 6, "held": 0}
verdict=$(grep -aoE '"served": [0-9]+, "shape_flags": [0-9]+, "not_exercised": [0-9]+' test_logs.txt | tail -n1 || true)
[[ "$verdict" =~ \"served\":\ ([0-9]+),\ \"shape_flags\":\ ([0-9]+) ]] \
  || { echo "::error::No 'async egress verdict' line in replay log — async lane was not evaluated"; exit 1; }
served="${BASH_REMATCH[1]}"; flags="${BASH_REMATCH[2]}"
echo "verdict -> $verdict"
if [[ "$served" -lt 1 ]]; then
  echo "::error::async engine served 0 watch polls — the async lane was never exercised at replay"; exit 1
fi
if [[ "$flags" -ne 0 ]]; then
  echo "::error::async engine reported ${flags} shape drift(s) — the volatile 'version' param should have matched cleanly"; exit 1
fi
echo "Async-egress engine served ${served} watch poll(s) with no shape drift."

# httppoll: the poll must have been HELD until its resolve testcase (not served
# on arrival). held >= 1 is the replay-side proof of the feature.
if [[ "$SCENARIO" == "httppoll" ]]; then
  held=$(grep -aoE '"held": [0-9]+' test_logs.txt | tail -n1 | grep -oE '[0-9]+' || true)
  [[ "${held:-0}" -ge 1 ]] || { echo "::error::httppoll: async engine held 0 polls — the poll was not held until its resolve testcase"; exit 1; }
  echo "httppoll: async engine held ${held} poll(s) until their resolve testcase."
fi
endsec

echo "async-config-poll e2e passed — scenario=${SCENARIO} (ingress PASSED + async lane exercised, no drift)."
exit 0
