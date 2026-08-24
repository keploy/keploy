#!/usr/bin/env bash

# E2E test for automatic MySQL port detection.
#
# MySQL is a server-speaks-first protocol, so keploy's generic dispatch
# path — which blocks reading the client before dialing upstream —
# deadlocks on it. The historical workaround was a port allowlist
# ([3306, 4000] plus the `mysqlPorts` config key); anything else hung
# the handshake and the app died with
#
#   Lost connection to server at 'handshake: reading initial
#   communication packet'
#
# This test moves the sample's MySQL onto 3307 — a port in NEITHER the
# built-in defaults NOR any config — and asserts the full round trip
# still works:
#
#   record: the server's HandshakeV10 is read off the wire and matched,
#           so the traffic lands as `kind: MySQL` mocks (not Generic).
#   replay: MySQL is shut down entirely, so the port can only come from
#           the recorded mocks' destAddr metadata.
#
# The guard that makes this meaningful is assert_no_mysql_ports_config:
# if anyone ever adds 3307 to the defaults or to the sample's config,
# this test silently stops testing detection — so it fails loudly
# instead.
#
# Phase 2 (endpoint drift) then replays a SECOND time with the app pointed at
# a port nothing has ever seen, while the mocks still say ${MYSQL_PORT}. Port
# recall is an equality test, and the environment does not always agree with
# the recording — a rebuilt compose file, a pooler on one side only, a
# per-environment port. When it disagrees, keploy still detects the connection
# as server-speaks-first and used to discard that verdict for want of a port
# match, leaving the app to block on a greeting that, in replay, only keploy
# could ever send. Phase 2 requires the same tests to pass anyway.
#
# Compat matrix: this behaviour needs BOTH binaries to support it. A
# record binary without detection hangs the handshake and records
# nothing; a replay binary without mock-derived port recall hangs the
# same way with the mocks already in hand. So the cross-version cells
# (record_latest_replay_build / record_build_replay_latest) skip via the
# capability probe below, exactly like the --storage-format json pass
# does, and only record_build_replay_build asserts the behaviour.

set -Eeuo pipefail

MYSQL_PORT=3307
# The port the application is pointed at for the SECOND replay, to simulate an
# environment whose database endpoint differs from the one that was recorded.
DRIFT_PORT=3399

section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

die() {
  rc=$?
  echo "::error::Pipeline failed (exit=$rc). Dumping context…"
  echo "== docker ps =="; docker ps || true
  echo "== mysql logs (last 200 lines) =="; docker compose logs --tail 200 mysql || true
  echo "== keploy tree (depth 4) =="; find ./keploy -maxdepth 4 -type f -print 2>/dev/null | sort | head -n 20 || true
  echo "== *.txt logs (last 100 lines) =="; for f in ./*.txt; do [[ -f "$f" ]] && { echo "--- $f ---"; tail -n 100 "$f"; }; done
  exit "$rc"
}
trap die ERR

# mysql_auto_detect_supported: true when this binary ships automatic
# MySQL port detection. Probed via the generated default config, which
# only carries the disableMysqlAutoDetect key on binaries that have the
# feature. Generated into a throwaway dir so the sample's own keploy.yml
# is never touched.
mysql_auto_detect_supported() {
  local bin="$1"
  local probe_dir
  probe_dir=$(mktemp -d)
  ( cd "$probe_dir" && "$bin" config --generate >/dev/null 2>&1 ) || { rm -rf "$probe_dir"; return 1; }
  local rc=1
  grep -q "disableMysqlAutoDetect" "$probe_dir/keploy.yml" 2>/dev/null && rc=0
  rm -rf "$probe_dir"
  return "$rc"
}

# mysql_port_drift_supported: true when this binary can serve MySQL mocks on a
# port the recording never saw. There is no config key for that behaviour, so
# probe the binary for the warning it emits on that path — deliberately the
# same string the assertion below greps for, so the probe cannot quietly drift
# away from what is being asserted.
mysql_port_drift_supported() {
  local bin="$1"
  [[ -f "$bin" ]] || return 1
  grep -aq "serving MySQL mocks on a port the recording never saw" "$bin"
}

# Move the sample off 3306 without touching the samples repo: rewrite
# the compose publish port and both JDBC URLs in place.
relocate_mysql_port() {
  section "Relocate MySQL to :${MYSQL_PORT} (off the default list)"
  sed -i "s|\"3306:3306\"|\"${MYSQL_PORT}:3306\"|" docker-compose.yml
  sed -i "s|jdbc:mysql://localhost:3306/|jdbc:mysql://localhost:${MYSQL_PORT}/|g" \
    src/main/resources/application.properties

  grep -q "\"${MYSQL_PORT}:3306\"" docker-compose.yml \
    || { echo "::error::compose port rewrite failed"; exit 1; }
  grep -q "localhost:${MYSQL_PORT}" src/main/resources/application.properties \
    || { echo "::error::JDBC URL rewrite failed"; exit 1; }

  echo "compose:"; grep -n "3306\|${MYSQL_PORT}" docker-compose.yml
  echo "jdbc:"; grep -n "jdbc-url" src/main/resources/application.properties
  endsec
}

# The whole point of this test is that NOTHING tells keploy about
# ${MYSQL_PORT}. If a config or the built-in default list ever starts
# covering it, the test would keep passing while testing nothing — so
# assert both here.
assert_no_mysql_ports_config() {
  local port="${1:-$MYSQL_PORT}"
  section "Assert ${port} is not pre-configured anywhere"

  # 1. The built-in default list in the source. Without this check,
  #    adding ${port} to defaultMysqlPorts would make this test
  #    green while it silently stopped exercising detection.
  local defaults_src="$GITHUB_WORKSPACE/pkg/agent/proxy/proxy.go"
  if [[ -f "$defaults_src" ]]; then
    local defaults_line
    defaults_line=$(grep -E "^var defaultMysqlPorts" "$defaults_src" || true)
    echo "built-in defaults: ${defaults_line:-<not found>}"
    if [[ -z "$defaults_line" ]]; then
      echo "::error::could not find defaultMysqlPorts in $defaults_src — the guard below cannot run; update this script if the variable was renamed"
      exit 1
    fi
    if grep -qE "(^|[^0-9])${port}([^0-9]|$)" <<<"$defaults_line"; then
      echo "::error::${port} is now a built-in default MySQL port — this test no longer exercises auto-detection. Pick a different port."
      exit 1
    fi
  else
    echo "::error::$defaults_src not found; cannot verify the built-in default port list"
    exit 1
  fi

  # 2. The generated config.
  if [[ -f keploy.yml ]]; then
    if grep -E "^\s*mysqlPorts:" keploy.yml | grep -q "${port}"; then
      echo "::error::keploy.yml lists ${port} in mysqlPorts — this test no longer exercises auto-detection"
      exit 1
    fi
    if grep -qE "^\s*disableMysqlAutoDetect:\s*true" keploy.yml; then
      echo "::error::auto-detection is disabled in keploy.yml — this test cannot pass by design"
      exit 1
    fi
    echo "keploy.yml mysql settings:"; grep -nE "mysqlPorts|disableMysqlAutoDetect" keploy.yml || echo "(none — using defaults)"
  else
    echo "no keploy.yml yet (will be generated with defaults)"
  fi
  endsec
}

wait_for_mysql() {
  section "Wait for MySQL readiness on :${MYSQL_PORT}"
  for _ in {1..120}; do
    if docker compose exec -T mysql mysql -uroot -prootpass -e "SELECT 1" >/dev/null 2>&1; then
      echo "MySQL is ready."
      endsec; return 0
    fi
    sleep 1
  done
  echo "::error::MySQL did not become ready in time"
  endsec; return 1
}

# A hung MySQL handshake shows up exactly here: the app never finishes
# booting because its connection pool is stuck waiting for a greeting.
# So this timing out IS the regression signal.
wait_for_app() {
  section "Wait for app HTTP port"
  for _ in {1..60}; do
    if curl -sS http://localhost:8080/api/oms -o /dev/null 2>/dev/null; then
      echo "App is responding."
      endsec; return 0
    fi
    sleep 1
  done
  echo "::error::App did not start in time — a stuck MySQL handshake on :${MYSQL_PORT} is the likely cause (auto-detection regression)"
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
    echo "Maven build failed on attempt ${attempt}/3; retrying."
    [[ "$attempt" -lt 3 ]] && sleep $((attempt * 10))
  done
  echo "::error::Maven build failed after 3 attempts. See mvn_build.log."
  return 1
}

send_request() {
  local kp_pid="$1"
  wait_for_app

  echo "=== Query both databases ==="
  curl -sS http://localhost:8080/api/query-both || true
  echo "=== Query OMS only ==="
  curl -sS http://localhost:8080/api/oms || true
  echo "=== Query Camunda only ==="
  curl -sS http://localhost:8080/api/camunda || true

  sleep 10
  echo "Sending SIGINT to keploy (pid $kp_pid) for graceful shutdown"
  sudo kill -INT "$kp_pid" 2>/dev/null || true
}

# --- Main ---

source "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/test-iid.sh"

if ! mysql_auto_detect_supported "${RECORD_BIN:-keploy}" \
  || ! mysql_auto_detect_supported "${REPLAY_BIN:-keploy}"; then
  # REQUIRE_MYSQL_AUTO_DETECT is set by the matrix cell where BOTH
  # binaries are built from this commit. There, "unsupported" can only
  # mean the feature regressed or the probe broke — never a version
  # skew — so skipping would turn real coverage into a silent green.
  # The cross-version cells leave it unset and skip legitimately.
  if [[ "${REQUIRE_MYSQL_AUTO_DETECT:-}" == "true" ]]; then
    echo "::error::both binaries are built from this commit, yet the capability probe"
    echo "::error::reports no automatic MySQL port detection. Either the feature"
    echo "::error::regressed, or 'keploy config --generate' no longer emits"
    echo "::error::disableMysqlAutoDetect. Not skipping — this cell must assert."
    exit 1
  fi
  echo "Skipping: automatic MySQL port detection requires BOTH the record and"
  echo "replay binaries to support it. Detection happens at record time and the"
  echo "port is recalled from mocks at replay time, so a mixed-version cell"
  echo "cannot exercise it. This is expected for the cross-version matrix cells."
  exit 0
fi

sudo rm -rf keploy/ keploy.yml

relocate_mysql_port

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

assert_no_mysql_ports_config

section "Record (MySQL on :${MYSQL_PORT}, nothing configured)"
"$RECORD_BIN" record -c "java -jar $JAR_NAME" > autoPort_record.txt 2>&1 &
KEPLOY_PID=$!
send_request "$KEPLOY_PID"
set +e
wait "$KEPLOY_PID"
RECORD_RC=$?
set -e
echo "Record exit code: $RECORD_RC"
# No data-race grep here: every cell of this job uses the `build-no-race`
# artifact, so the race detector is not compiled in and the check could
# never fire. Race coverage for this code path lives in the Go unit
# tests (pkg/agent/proxy/mysql_detect_test.go, run under -race).
endsec

# The config is generated on the first record run — re-assert now that
# it exists, so a bad default can't sneak the test past.
assert_no_mysql_ports_config

section "Assert MySQL was auto-detected during record"
if ! grep -q "detected MySQL wire protocol on a non-default port" autoPort_record.txt; then
  echo "::error::keploy did not report detecting MySQL on :${MYSQL_PORT}"
  tail -n 120 autoPort_record.txt
  exit 1
fi
grep "detected MySQL wire protocol on a non-default port" autoPort_record.txt

MOCKS_FILE=$(ls -1 ./keploy/test-set-*/mocks.yaml 2>/dev/null | head -n1 || true)
if [[ -z "$MOCKS_FILE" ]]; then
  echo "::error::No mocks.yaml recorded"
  exit 1
fi

# Detection is only real if the traffic was parsed AS MySQL. Landing in
# the Generic parser would still produce mocks, so assert the kind.
MYSQL_MOCKS=$(grep -c "^kind: MySQL" "$MOCKS_FILE" || true)
if [[ "${MYSQL_MOCKS:-0}" -lt 1 ]]; then
  echo "::error::No 'kind: MySQL' mocks in $MOCKS_FILE — traffic was not routed to the MySQL parser"
  grep -o "^kind: .*" "$MOCKS_FILE" | sort | uniq -c || true
  exit 1
fi
echo "Recorded $MYSQL_MOCKS MySQL mocks"

# And the destAddr must carry the non-default port — that metadata is
# what replay reads back to recover the port.
if ! grep -q "destAddr: .*:${MYSQL_PORT}" "$MOCKS_FILE"; then
  echo "::error::No mock records destAddr on :${MYSQL_PORT}; replay would have nothing to recall"
  grep -o "destAddr: .*" "$MOCKS_FILE" | sort -u || true
  exit 1
fi
grep -o "destAddr: .*" "$MOCKS_FILE" | sort -u
endsec

section "Shutdown MySQL before replay"
docker compose down || true
echo "MySQL is gone — replay must serve every query from mocks, and can only"
echo "know :${MYSQL_PORT} is MySQL by recalling it from those mocks."
endsec

section "Replay"
set +e
"$REPLAY_BIN" test -c "java -jar $JAR_NAME" \
  --delay 20 --api-timeout 60 \
  2>&1 | tee test_logs.txt
REPLAY_RC=$?
set -e
echo "Replay exit code: $REPLAY_RC"
endsec

section "Assert MySQL port was recalled from mocks during replay"
if ! grep -q "recovered MySQL ports from recorded mocks" test_logs.txt; then
  echo "::error::replay did not recover the MySQL port from mocks"
  tail -n 120 test_logs.txt
  exit 1
fi
grep "recovered MySQL ports from recorded mocks" test_logs.txt
endsec

# check_reports asserts every test set in the most recent run passed.
# Parameterised because this script replays twice — once on the recorded
# endpoint, once after moving it.
check_reports() {
  local label="$1" replay_rc="$2" previous="${3:-}"
  section "Check reports (${label})"
  local run_dir
  run_dir=$(latest_run_dir)
  # A replay that dies before writing a report would otherwise leave the
  # PREVIOUS phase's directory as the newest one, and its PASSED statuses would
  # be read as this phase's.
  if [[ -n "$previous" && "$run_dir" == "$previous" ]]; then
    echo "::error::the ${label} replay produced no report of its own; the newest"
    echo "::error::run directory is still the one from the previous phase."
    endsec
    exit 1
  fi
  if [[ -z "${run_dir:-}" ]]; then
    echo "::error::No test-run directory found under ./keploy/reports (${label})"
    endsec
    [[ $replay_rc -ne 0 ]] && exit "$replay_rc" || exit 1
  fi
  echo "Using reports from: $run_dir"

  local all_passed=true found_any=false status
  for rpt in "$run_dir"/test-set-*-report.yaml; do
    [[ -f "$rpt" ]] || continue
    found_any=true
    status=$(awk '/^status:/{print $2; exit}' "$rpt")
    echo "Test status for $(basename "$rpt"): ${status:-<missing>}"
    [[ "$status" == "PASSED" ]] || all_passed=false
  done
  endsec

  if [[ "$found_any" == "false" ]]; then
    echo "::error::No test report files found in $run_dir (${label})"
    exit 1
  fi
  if [[ "$all_passed" != "true" ]]; then
    echo "::error::Some tests failed during replay (${label})"
    exit 1
  fi
  if [[ $replay_rc -ne 0 ]]; then
    echo "Replay exited with code $replay_rc but all tests passed (${label}). Ignoring exit code."
  fi
}

latest_run_dir() {
  ls -1dt ./keploy/reports/test-run-* 2>/dev/null | head -n1 || true
}

# reports_all_passed answers the same question as check_reports without
# exiting, so a caller can branch on the outcome. It takes the run directory
# that existed BEFORE the replay: a replay whose app never started writes no
# report at all, and without that argument this would read the previous
# phase's report and cheerfully conclude everything passed.
reports_all_passed() {
  local previous="$1" run_dir status
  run_dir=$(latest_run_dir)
  [[ -n "${run_dir:-}" ]] || return 1
  [[ "$run_dir" != "$previous" ]] || return 1
  local found=false
  for rpt in "$run_dir"/test-set-*-report.yaml; do
    [[ -f "$rpt" ]] || continue
    found=true
    status=$(awk '/^status:/{print $2; exit}' "$rpt")
    [[ "$status" == "PASSED" ]] || return 1
  done
  [[ "$found" == "true" ]]
}

check_reports "recorded endpoint" "$REPLAY_RC"

# ---------------------------------------------------------------------------
# Phase 2: endpoint drift
# ---------------------------------------------------------------------------
#
# Everything above replays against the same endpoint that was recorded, so the
# port can be recalled from the mocks' destAddr metadata. That recall is an
# equality test, and it carries an assumption stated in the PR that introduced
# it: "container IPs change between record and replay, ports don't."
#
# They do. A reconstructed environment — `keploy cloud replay` rebuilding a
# compose file from a stored manifest, a pooler present in one environment and
# not the other, a per-environment port — hands the application a different
# endpoint than the recording saw. Detection still fires there: the client
# opens the connection and says nothing, which is the MySQL signature and the
# only server-speaks-first protocol keploy records. It was the port equality
# check that discarded that evidence, and declining is not a neutral outcome —
# in replay keploy IS the server, so nothing else will ever send the greeting.
# The application blocks until its own timeout, every test touching the
# dependency reports status_code 0, and the entire recorded mock set goes
# unused while the recording itself is perfectly good. That is silent: no error
# is printed, the tests simply fail as if the application were broken.
#
# So: point the application at a port nothing has ever seen, with MySQL still
# shut down and the mocks still recorded on :${MYSQL_PORT}, and require the
# same tests to pass anyway.
if ! mysql_port_drift_supported "${REPLAY_BIN:-keploy}"; then
  if [[ "${REQUIRE_MYSQL_AUTO_DETECT:-}" == "true" ]]; then
    echo "::error::the replay binary is built from this commit yet does not report"
    echo "::error::endpoint-drift inference. Either the feature regressed or the"
    echo "::error::probe string changed. Not skipping — this cell must assert."
    exit 1
  fi
  echo "Skipping the endpoint-drift phase: the replay binary predates it."
  echo "MySQL auto-port-detection e2e passed: recorded and replayed on :${MYSQL_PORT} with no port configuration."
  exit 0
fi

section "Point the app at :${DRIFT_PORT} (mocks still say :${MYSQL_PORT})"
# The jar carries application.properties on its classpath, so editing the
# source tree after the build changes nothing — the app would keep dialling
# :${MYSQL_PORT} and this phase would assert against a drift that never
# happened. Spring Boot ranks file:./ above classpath:/, so an override next to
# the jar is what actually moves the endpoint, without a second build.
if ! grep -E '^datasource\..*jdbc-url=' src/main/resources/application.properties > jdbc_lines.txt; then
  echo "::error::no datasource.*.jdbc-url lines in the sample's application.properties;"
  echo "::error::the sample renamed its datasource keys and this override no longer applies."
  exit 1
fi
sed "s|localhost:${MYSQL_PORT}|localhost:${DRIFT_PORT}|g" jdbc_lines.txt > application.properties
if ! grep -q "localhost:${DRIFT_PORT}" application.properties; then
  echo "::error::JDBC URL override for :${DRIFT_PORT} was not written"
  exit 1
fi
echo "override (file:./application.properties):"; cat application.properties
echo "Recorded mocks still carry :${MYSQL_PORT}:"
grep -o "destAddr: .*" "$MOCKS_FILE" | sort -u
endsec

# The drift port must be as unknown to keploy as the recorded one, or the
# inference path is never reached and this phase asserts nothing.
assert_no_mysql_ports_config "$DRIFT_PORT"

PRE_DRIFT_RUN_DIR=$(latest_run_dir)

section "Replay after endpoint drift"
set +e
"$REPLAY_BIN" test -c "java -jar $JAR_NAME" \
  --delay 20 --api-timeout 60 \
  2>&1 | tee test_logs_drift.txt
DRIFT_RC=$?
set -e
echo "Replay exit code: $DRIFT_RC"
endsec

section "Assert the mocks were served on the drifted port"
# Passing tests alone would not prove the inference ran — a change that simply
# made the port allowlist permissive would pass too. Require the warning that
# says keploy knowingly served MySQL on a port the recording never saw. It is
# logged at WARN, so unlike the port numbers (DEBUG) it is always present.
# The port travels as a structured field, so match it too: a warning fired on
# some other port would otherwise satisfy this assertion.
if ! grep "serving MySQL mocks on a port the recording never saw" test_logs_drift.txt | grep -q "${DRIFT_PORT}"; then
  # Two very different failures both land here, and the test outcome tells them
  # apart. If the override never took effect the app is still talking to
  # :${MYSQL_PORT}, which IS in the recorded set, so every test passes. If the
  # override took effect and the inference is gone, the app blocks waiting for
  # a greeting only keploy could send, its connection pool fails to initialise,
  # and nothing passes.
  if reports_all_passed "$PRE_DRIFT_RUN_DIR"; then
    echo "::error::the drift warning is absent but every test passed, which means the"
    echo "::error::application was still talking to :${MYSQL_PORT} — the file:./application.properties"
    echo "::error::override did not take effect and this phase exercised nothing."
    echo "::error::Check that Spring Boot still ranks file:./ above classpath:/ and that"
    echo "::error::the datasource keys below still match the sample:"
    cat application.properties
  else
    echo "::error::keploy did not serve the recorded MySQL mocks on :${DRIFT_PORT}."
    echo "::error::The application blocked waiting for a server greeting that, in replay,"
    echo "::error::only keploy could send — the endpoint-drift inference has regressed."
    if [[ "$(latest_run_dir)" == "$PRE_DRIFT_RUN_DIR" ]]; then
      echo "::error::(the drift replay produced no report at all: the app never finished starting)"
    fi
    grep -n "Communications link failure\|Failed to initialize pool" test_logs_drift.txt | head -n 5 || true
  fi
  tail -n 120 test_logs_drift.txt
  exit 1
fi
grep "serving MySQL mocks on a port the recording never saw" test_logs_drift.txt | head -n 3
endsec

check_reports "drifted endpoint :${DRIFT_PORT}" "$DRIFT_RC" "$PRE_DRIFT_RUN_DIR"

echo "MySQL auto-port-detection e2e passed: recorded and replayed on :${MYSQL_PORT} with"
echo "no port configuration, and replayed again with the app moved to :${DRIFT_PORT}."
exit 0
