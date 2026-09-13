#!/usr/bin/env bash
source "$(dirname "${BASH_SOURCE[0]}")/../../go-retry.sh"
# safer bash. -e also arrives from the step shell: golang_linux.yml SOURCES
# this file from a `run:` step with no `shell:` key, i.e. GitHub's `bash -e {0}`.
set -Eeuo pipefail

source "${GITHUB_WORKSPACE:-${PWD%/samples-*}}/.github/workflows/test_workflow_scripts/docker-build-retry.sh"
git fetch origin
git checkout origin/add-ssl-mysql

# ----- helpers -----
section()  { echo "::group::$*"; }
endsec()   { echo "::endgroup::"; }
dump_logs() {
  # `local`: this file is SOURCED, so an undeclared assignment here writes a
  # variable in the calling step shell.
  local rc=$?
  echo "Dumping logs and artifacts (exit code: $rc)"
  section "Record logs"
  cat urlShort_*.txt || true
  endsec
  section "Replay logs"
  cat test_logs.txt || true
  endsec

  exit "$rc"
}
trap dump_logs EXIT

wait_for_mysql() {
  section "Wait for MySQL readiness"
  # ping until mysqld accepts connections
  # `local`, and not `i`: the main flow drives recording with `for i in 1 2`,
  # and an undeclared `i` in a helper writes the caller's copy. The counter is
  # never read in this loop; it only bounds the wait.
  local attempt
  # shellcheck disable=SC2034  # a bare bound; the loop body never reads it
  for attempt in {1..60}; do
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
  # `local`, and not `i`: send_request is called from inside the main flow's
  # `for i in 1 2` record loop, and an undeclared `i` here left the caller's
  # counter at this loop's final value — so the pass-2 confirmation echo
  # printed the health-poll count instead of the pass number (1 when the app
  # was healthy on the first poll, 60 when it never became healthy). The
  # counter is never read in this loop; it only bounds the wait.
  local attempt
  # shellcheck disable=SC2034  # a bare bound; the loop body never reads it
  for attempt in {1..60}; do
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
}

run_record_iteration() {
  local idx="$1"
  local extra_flags="${2:-}"
  local label="${extra_flags:+_json}"
  local app_name="urlShort_${idx}${label}"

  echo "Record iteration $idx${label:+ (json)}"

  sudo rm -f /tmp/keploy-logs.txt

  # A plain redirect, NOT `| tee`. A pipeline's $! is its LAST element, so this
  # used to hold tee's pid and the recorder's own was never available — which is
  # why the stop below had to guess with `pgrep keploy`. Measured: with the tee
  # pipeline $! names tee, so signalling it never reached the recorder at all
  # — the recorder instead died on its next stdout write, from SIGPIPE, which
  # Go does not suppress on fd 1/2 (measured with a Go writer: exit 141, and its
  # shutdown handler never ran). An ungraceful stop that skips the flush path.
  # Six other lanes already record this way — spring_petclinic,
  # express_mongoose, mysql_dual_conn, tidb_stmt_cache, mysql_auto_port and
  # async_config_poll — all redirect to a file and signal $!.
  # extra_flags is intentionally unquoted so the empty default expands
  # to nothing; the json pass passes "--storage-format json".
  # shellcheck disable=SC2086
  "$RECORD_BIN" record $extra_flags -c "./urlShort" --generateGithubActions=false \
    > "${app_name}.txt" 2>&1 &
  local KEPLOY_PID=$!

  # Drive traffic + stop keploy
  send_request "$KEPLOY_PID"

  # tee used to stream this into the job log as it was written; a redirect only
  # reaches the log when something prints it, so print it.
  cat "${app_name}.txt" || true

  # Wait for keploy exit and capture code
  section "Stop Recording"
  sleep 10
  echo "Stopping Keploy record process (PID: $KEPLOY_PID)..."
  # Signal the recorder we started, by pid. `pgrep keploy` is gone from here: it
  # named BOTH the CLI and the root agent — the agent is this same binary
  # re-run under sudo (agent.go's GetCurrentBinaryPath into NewAgentCommand), so
  # both carry comm `keploy`, which golang/mock_mismatch/golang-linux.sh relies
  # on by name. It also matches by unanchored comm, so any other keploy* process
  # on the host was in its blast radius. Signalling by pid removes both.
  # `|| true` on the kill because keploy may have exited on its own first —
  # pgrep-then-kill was a TOCTOU race whose failure carried no information, and
  # errexit checking it aborted runs whose recording had already succeeded.
  # The kill is not what establishes the outcome; `wait` is. KEPLOY_PID is this
  # shell's own child, so wait returns only once it is really gone and yields
  # its status — no /proc inspection and no privilege question, which is what
  # the pgrep form needed because it signalled processes it did not start.
  sudo kill "$KEPLOY_PID" 2>/dev/null || true
  local rc=0
  wait "$KEPLOY_PID" || rc=$?
  # Reported, not gated: this lane has never failed on the recorder's exit
  # status, and a SIGTERM'd process legitimately reports one.
  echo "Record exit code: $rc"
  sleep 30
  echo "Recording stopped."
  endsec

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
# The calling job sets these from `steps.<id>.outputs.path`. These are the only
# unguarded script-external expansions in this file, so they look like what
# `set -u` buys — they are not: -u fires only on an UNSET name, and a variable
# that is set to the empty string sails straight through it (measured). An
# empty or stale path is otherwise first reported ten lines below, by sudo,
# after `rm -rf keploy/` has already run. Asserted here instead.
: "${RECORD_BIN:?not set by the calling job (steps.record.outputs.path)}"
: "${REPLAY_BIN:?not set by the calling job (steps.replay.outputs.path)}"
for _b in "$RECORD_BIN" "$REPLAY_BIN"; do
  # `command -v`, not `[ -x ]`: it accepts an absolute path and a bare PATH
  # name alike, where `[ -x ]` rejects a bare name outright (measured on `git`).
  # The `[ -e ]` fallback is deliberate and must not be dropped: this script
  # also runs these binaries under sudo, so the calling user's execute bit is
  # not the right question — only "names nothing at all" is. That is exactly
  # what an empty or stale path does, and both are rejected here.
  command -v -- "$_b" >/dev/null 2>&1 || [ -e "$_b" ] || {
    echo "::error::not a usable binary path: $_b"; exit 1; }
done
echo "RECORD_BIN: $RECORD_BIN"
echo "REPLAY_BIN : $REPLAY_BIN"
"$RECORD_BIN" --version 2>/dev/null || true
"$REPLAY_BIN" --version 2>/dev/null || true
# Clean slate per run
rm -rf keploy/ keploy.yml || true
 # Generate config

sudo rm -f /tmp/keploy-logs.txt

sudo "$RECORD_BIN" config --generate
sed -i 's/global: {}/global: {"body": {"updated_at":[]}}/' ./keploy.yml
go_retry build -o urlShort
endsec

section "Start MySQL"
# Capture first, match second — deliberately NOT `docker ps … | grep -q …`, the
# shape json-pass-helpers.sh documents. That pipeline conflated two different
# facts into one status: `grep -q` exits at its first match and can SIGPIPE a
# writer that still has bytes pending, and a `docker ps` that fails outright
# prints nothing and exits non-zero. pipefail promotes either to the pipeline,
# so `if !` read "container absent" while it was PRESENT, took the create
# branch, and `docker run --name mysql-container` then failed on a healthy run.
# Measured: the real docker CLI emits its whole --format output in ONE write(),
# and a single write that fits the 64KiB pipe buffer completes before the reader
# can exit — so with the real CLI that route needs an implausibly long container
# list. Any producer that writes incrementally takes it at this lane's output
# size (measured: 61 wrong branches in 200 trials). A capture has no reader to
# race with at all, which is why this is a capture and not a -c/-q swap.
# The explicit rc check is here because an `if` condition is exempt from
# errexit: a `docker ps` that fails has not established that the container is
# absent, so the run must stop and say so rather than branch on a value it
# never got.
if ! running_containers=$(docker ps --format '{{.Names}}'); then
  echo "::error::docker ps failed; cannot determine whether mysql-container is running"
  exit 1
fi
# exact-line match, equivalent to `grep -qxF mysql-container`
if [[ $'\n'"$running_containers"$'\n' != *$'\n'mysql-container$'\n'* ]]; then
  if [[ "${ENABLE_SSL:-false}" == "true" ]]; then
    echo "Starting MySQL with SSL/TLS via Docker Compose"
    docker_compose_pull_retry
    docker compose up -d
    wait_for_mysql
  else
    echo "Starting MySQL with standard Docker run (no SSL)"
    sed -i 's/MYSQL_SSL_MODE=production/MYSQL_SSL_MODE=false/' .env
    cat .env
    docker_pull_retry mysql:latest
    docker run --name mysql-container -e MYSQL_ROOT_PASSWORD=password -e MYSQL_DATABASE=uss \
      -p 3306:3306 --rm -d mysql:latest
    wait_for_mysql
  fi
fi
endsec

for i in 1 2; do
  run_record_iteration "$i"
  echo "Recorded test case and mocks for iteration $i"
done

# shellcheck disable=SC1091
source "${GITHUB_WORKSPACE:-${PWD%/samples-*}}/.github/workflows/test_workflow_scripts/json-pass-helpers.sh"

if json_pass_supported; then
  # State-bleed guard before json record pass. The yaml iterations left
  # rows in mysql-container; capturing the same traffic again over a
  # non-empty DB records different responses (auto-increment past N,
  # duplicate-key errors, etc.) which then fail when replayed against a
  # fresh DB. Recreate the container so json record sees the same empty
  # starting state the yaml pass had.
  section "Reset MySQL before json record pass"
  docker rm -f mysql-container || true
  if [[ "${ENABLE_SSL:-false}" == "true" ]]; then
    docker_compose_pull_retry
    docker compose up -d
    wait_for_mysql
  else
    docker_pull_retry mysql:latest
    docker run --name mysql-container -e MYSQL_ROOT_PASSWORD=password -e MYSQL_DATABASE=uss \
      -p 3306:3306 --rm -d mysql:latest
    wait_for_mysql
  fi
  endsec

  for i in 1 2; do
    run_record_iteration "$i" "--storage-format json"
    echo "Recorded json test case and mocks for iteration $i"
  done
fi

section "Shutdown MySQL before test mode"
# Stop MySQL container - Keploy should use mocks for database interactions
docker stop mysql-container || true
docker rm mysql-container || true
echo "MySQL stopped - Keploy should now use mocks for database interactions"
endsec

section "Replay"
# Run replay but DON'T crash the step; capture rc and print logs.
sudo rm -f /tmp/keploy-logs.txt

# An `if` condition is exempt from errexit, so the step survives a failing
# replay long enough to say WHICH test set failed. Deliberately not
# `set +e; …; set -e`, the idiom go-retry.sh argues against by name: a window
# is a shell-wide state that a later edit can silently widen, and the one this
# replaces already covered the `sudo rm -f` above, hiding its failures while
# the identical `sudo rm -f` calls elsewhere in this file ran bare.
# This matters now in a way it did not before: with the pragma at the top of
# this file actually executing, pipefail makes the pipeline carry the replay's
# own status instead of tee's 0, so a failing replay would otherwise kill the
# step right here — losing "Replay exit code", the per-test-set verdict read
# from the report files below, and the ::error:: that names the outcome.
# PIPESTATUS[0], not $?: it names the replay's own status whatever tee does,
# and it survives into the else branch because nothing runs in between. No
# `|| true` — that would not merely swallow the failure, it would reset
# PIPESTATUS before it could be read.
# Other failures still abort the step under errexit: wait_for_mysql's
# `return 1` and run_record_iteration's DATA RACE `return 1` are both called
# bare. (Named, not cited by line: line numbers go stale, comments must not.)
if "$REPLAY_BIN" test -c "./urlShort" --delay 7 --generateGithubActions=false \
  2>&1 | tee test_logs.txt
then
  REPLAY_RC=0
else
  REPLAY_RC=${PIPESTATUS[0]}
fi
echo "Replay exit code: $REPLAY_RC"
cat test_logs.txt || true
endsec

# If replay failed, still try to read reports to say which set failed
section "Check reports"
# Newest test-run dir (don’t assume test-run-0), picked in bash: no pipeline,
# so pipefail has nothing to promote and no status is discarded. The replaced
# form was `ls … | head -n1 || true`, where `ls` legitimately exits 2 when no
# test-run dir exists; with pipefail live that `|| true` became the only thing
# keeping the ::error:: below reachable, while reading as removable dead weight.
# With no test-run dir the glob stays literal (nullglob is off), fails -d, and
# leaves RUN_DIR empty — which the check below already reports by name.
RUN_DIR=""
for _d in ./keploy/reports/test-run-*; do
  [[ -d "$_d" ]] || continue
  if [[ -z "$RUN_DIR" || "$_d" -nt "$RUN_DIR" ]]; then RUN_DIR="$_d"; fi
done
if [[ -z "${RUN_DIR:-}" ]]; then
  echo "::error::No test-run directory found under ./keploy/reports"
  [[ $REPLAY_RC -ne 0 ]] && exit "$REPLAY_RC" || exit 1
fi

echo "Using reports from: $RUN_DIR"
all_passed=true
# found_any, because the glob falls through to `continue` when RUN_DIR holds no
# yaml reports at all — leaving all_passed=true and passing the lane on a run
# that scanned nothing. json_scan_reports and mysql_dual_conn both guard this.
found_any=false
for rpt in "$RUN_DIR"/test-set-*-report.yaml; do
  [[ -f "$rpt" ]] || continue
  found_any=true
  status=$(awk '/^status:/{print $2; exit}' "$rpt")
  echo "Test status for $(basename "$rpt"): ${status:-<missing>}"
  if [[ "$status" != "PASSED" ]]; then
    all_passed=false
  fi
done
endsec

if [[ "$found_any" != "true" ]]; then
  echo "::error::No test-set report files found in $RUN_DIR"
  exit 1
fi

if [[ "$all_passed" != "true" || $REPLAY_RC -ne 0 ]]; then
  echo "::error::Some yaml tests failed or replay exited non-zero"
  exit 1
fi

if json_pass_supported; then
  section "Replay (json)"
  sudo rm -f /tmp/keploy-logs.txt
  # Same shape and same reasons as REPLAY_RC above.
  if "$REPLAY_BIN" test --storage-format json -c "./urlShort" --delay 7 --generateGithubActions=false \
    2>&1 | tee test_logs_json.txt
  then
    REPLAY_RC_JSON=0
  else
    REPLAY_RC_JSON=${PIPESTATUS[0]}
  fi
  echo "Replay (json) exit code: $REPLAY_RC_JSON"
  cat test_logs_json.txt || true
  endsec

  # Scan first, fail second, mirroring the yaml branch's "still try to read
  # reports to say which set failed". With REPLAY_RC_JSON now live, exiting on
  # it before the scan would drop the per-test-set diagnostic on exactly the
  # runs that need it.
  json_ok=true
  json_scan_reports || json_ok=false
  if [[ "$json_ok" != "true" || $REPLAY_RC_JSON -ne 0 ]]; then
    echo "::error::Json replay failed or exited non-zero (rc=$REPLAY_RC_JSON)"
    cat test_logs_json.txt
    exit 1
  fi
  echo "All tests passed (yaml + json)"
else
  echo "All tests passed (yaml only — json pass skipped for compat-matrix cell)"
fi
exit 0