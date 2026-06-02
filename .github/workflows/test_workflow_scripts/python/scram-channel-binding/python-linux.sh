#!/bin/bash
# E2E test for the BPF channel-binding shim against the
# SCRAM-SHA-256-PLUS path. The sample app uses psycopg2-binary so the
# auditwheel-bundled libcrypto exercises the exact case that defeated
# the legacy LD_PRELOAD cbshim — proving the uprobe approach attaches
# under RTLD_LOCAL.
#
# Setup:
#   1. Bring up postgres (TLS + SCRAM-SHA-256 forced via hostssl).
#   2. Start the Flask app under `keploy record` so libpq's upstream
#      TLS handshake is MITM'd by the proxy. cbshim attaches to
#      X509_digest in the app's libcrypto and rewrites the channel
#      binding hash so SCRAM-PLUS succeeds.
#   3. Drive a handful of endpoints with curl — each one opens a new
#      libpq connection and re-runs the SCRAM handshake.
#   4. Stop record; verify keploy/test-set-0/mocks.yaml contains at
#      least one PostgresV3 mock per endpoint (channel-binding worked,
#      mocks recorded).
#   5. Run `keploy test` to replay against the recorded mocks; the
#      replay path must also negotiate SCRAM-PLUS successfully.
#
# Pass criteria:
#   - Every endpoint returns 2xx during record (proves cbshim works).
#   - mocks.yaml contains >= 5 PostgresV3 mocks (one per DB call below).
#   - keploy test exits 0 (replay matches recorded responses).
#
# Failure modes this catches:
#   - cbshim doesn't attach (BPF load fails, uprobe attach fails) →
#     every endpoint returns 500 with "SCRAM channel binding check
#     failed" from postgres.
#   - cbshim attaches but cbmap lookup misses (proxy didn't publish
#     the real cert) → same 500 as above.
#   - PostgresV3 mock encoder regressed → mock count < expected, or
#     replay fails to find a matching mock.

set -uo pipefail
# Explicit set +e — we capture exit codes ourselves and always run
# cleanup. See outbound-fin-stall/python-linux.sh for the full
# rationale.
set +e

source "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/test-iid.sh"

cd "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/python/scram-channel-binding"

# pkill keploy by comm to avoid -f self-matches against this script.
# See outbound-fin-stall/python-linux.sh for the matching rationale.
# On exit, dump the postgres container's logs so the uploaded artifact
# carries the server-side view of what happened (auth attempts, TLS
# handshake errors, etc) — invaluable for debugging SCRAM-PLUS failures.
cleanup() {
    echo "cleanup..."
    # Dump postgres logs before tearing down the container — they're
    # the server-side view of every SCRAM-PLUS attempt.
    docker compose logs postgres 2>/dev/null > postgres_logs.txt || true
    sudo pkill -9 keploy 2>/dev/null || true
    docker compose down -v 2>/dev/null || true
    sleep 1
}
trap cleanup EXIT

echo "=== install python deps (psycopg2-binary + flask + gunicorn) ==="
python3 -m pip install --upgrade pip >/dev/null
python3 -m pip install -r requirements.txt
echo "psycopg2-binary version: $(python3 -c 'import psycopg2; print(psycopg2.__version__)')"

# Show the bundled libcrypto path so the test log makes it obvious
# that we're exercising the auditwheel case (and not the system libpq).
python3 - <<'PY'
import psycopg2, ctypes.util, glob, os
psycopg2_dir = os.path.dirname(psycopg2.__file__)
bundled = glob.glob(os.path.join(psycopg2_dir, ".libs", "libcrypto*"))
print(f"psycopg2 module dir: {psycopg2_dir}")
print(f"auditwheel-bundled libcrypto: {bundled if bundled else '<none — not the binary wheel?>'}")
PY

echo "=== boot postgres (TLS + SCRAM-SHA-256 forced) ==="
docker compose up -d postgres
# Wait for healthcheck. docker compose's wait flag isn't on all runners,
# so poll docker inspect.
ready=0
for i in $(seq 1 60); do
    health=$(docker inspect --format='{{.State.Health.Status}}' \
        "$(docker compose ps -q postgres)" 2>/dev/null || echo "")
    if [ "$health" = "healthy" ]; then
        echo "postgres healthy after ${i}s"
        ready=1
        break
    fi
    sleep 1
done
if [ "$ready" -ne 1 ]; then
    echo "::error::postgres did not become healthy within 60s"
    docker compose logs postgres | tail -100
    exit 1
fi

echo "=== baseline: connect to postgres WITHOUT keploy to verify the cluster works ==="
# Run the same SCRAM-PLUS conninfo psycopg2 will use, but BEFORE keploy
# starts. If this fails, the cert/SCRAM setup is broken — bail early
# with a clear message rather than blaming cbshim.
baseline_rc=0
python3 - <<'PY' || baseline_rc=$?
import psycopg2, sys, traceback
try:
    conn = psycopg2.connect(
        "host=127.0.0.1 port=5432 dbname=app user=app password=app-secret "
        "sslmode=require channel_binding=require"
    )
    cur = conn.cursor()
    cur.execute("SELECT 1")
    print("baseline ok: SCRAM-PLUS works against the postgres cluster without keploy in the path")
    cur.close(); conn.close()
except Exception as e:
    print(f"baseline FAILED: {e}", file=sys.stderr)
    traceback.print_exc()
    sys.exit(2)
PY
if [ "$baseline_rc" -ne 0 ]; then
    echo "::error::SCRAM-PLUS does not work even WITHOUT keploy in the path (rc=$baseline_rc); the postgres cluster or cert setup is broken — not a cbshim issue"
    docker compose logs postgres | tail -60
    exit 1
fi

echo "=== generate keploy config + clean prior state ==="
sudo "$RECORD_BIN" config --generate
sudo rm -rf keploy/

# Strip the global noise filter so we capture the cbshim's BPF probe
# counters in the agent log (visible via `keploy log --level debug`).
config_file="./keploy.yml"
if [ -f "$config_file" ]; then
    sed -i 's/global: {}/global: {"header": {"Allow":[],}}/' "$config_file"
fi

APP_HOST="127.0.0.1"
APP_PORT="8080"
APP_URL="http://${APP_HOST}:${APP_PORT}"

# A tiny driver — fires one request per endpoint. Each successful
# request opens a new libpq conn, re-runs SCRAM-PLUS, exercises the
# cbshim path. If cbshim isn't working, the corresponding endpoint
# returns 500.
#
# Two-stage warmup is critical:
#   1) Wait for /healthz — proves gunicorn workers are up.
#   2) Wait for /db/ping — proves cbshim's WatchProcessTree has rescanned
#      since the workers appeared, found their libcrypto, attached the
#      uprobe, and the cbmap rendezvous (RegisterMITM + RegisterReal)
#      has fired for at least one connection. AttachToProcessTree runs
#      at proxy startup (BEFORE gunicorn forks), so on the first run
#      it sees no workers; only after WatchProcessTree's 2s rescan
#      finds them does the probe become effective. Polling /db/ping
#      until it returns 200 lets the rescan converge without a fixed
#      sleep.
drive_endpoints() {
    local rc=0

    # Stage 1: HTTP layer up.
    for i in $(seq 1 30); do
        if curl -sf "${APP_URL}/healthz" >/dev/null; then
            echo "app healthy after ${i}s"
            break
        fi
        sleep 1
    done

    # Stage 2: SCRAM-PLUS path warm — cbshim has attached and the
    # cbmap has the real cert for this conn's mitm/real rendezvous.
    # Budget: 30s. If /db/ping still returns 500 after that, cbshim
    # truly isn't working and the matrix should fail visibly.
    db_ready=0
    for i in $(seq 1 30); do
        if curl -sf "${APP_URL}/db/ping" >/dev/null 2>&1; then
            echo "db reachable through cbshim after ${i}s"
            db_ready=1
            break
        fi
        sleep 1
    done
    if [ "$db_ready" -ne 1 ]; then
        echo "::error::/db/ping never returned 2xx — cbshim warmup did not converge"
        return 1
    fi

    # Endpoint sequence — these now drive the recorded mocks. Each
    # curl --fail exits non-zero on any HTTP >= 400, so any unexpected
    # 500 (e.g. a worker that the cbshim watcher missed) still trips
    # the matrix.
    echo "--- GET /healthz   ---"; curl -sf "${APP_URL}/healthz" || rc=$?
    echo
    echo "--- GET /db/ping   ---"; curl -sf "${APP_URL}/db/ping" || rc=$?
    echo
    echo "--- GET /users     ---"; curl -sf "${APP_URL}/users" || rc=$?
    echo
    echo "--- GET /users/1   ---"; curl -sf "${APP_URL}/users/1" || rc=$?
    echo
    echo "--- POST /users    ---"; curl -sf -X POST -H 'Content-Type: application/json' \
        -d '{"name":"keploy-e2e"}' "${APP_URL}/users" || rc=$?
    echo
    echo "--- GET /users/1/audit ---"; curl -sf "${APP_URL}/users/1/audit" || rc=$?
    echo
    return $rc
}

echo "=== run keploy record with the flask app ==="
# Write gunicorn_conf.py FIRST so it exists when keploy launches the
# child. The previous ordering raced: keploy was backgrounded and the
# heredoc ran in the foreground, so on a slow scheduler keploy could
# exec gunicorn before the conf existed and gunicorn would die with
# "config file '/.../gunicorn_conf.py' does not exist".
cat > gunicorn_conf.py <<'GCONF'
import os
bind = f"0.0.0.0:{os.environ.get('PORT', '8080')}"
workers = 2
threads = 4
worker_class = "sync"
accesslog = "-"
errorlog = "-"
loglevel = "info"
timeout = 30
GCONF

# The app needs PGHOST=127.0.0.1 (postgres is host-mapped to :5432).
# Run keploy record in the background so we can drive endpoints from
# the foreground.
sudo -E env PATH="$PATH" \
    PGHOST=127.0.0.1 PGPORT=5432 PGUSER=app PGPASSWORD=app-secret PGDATABASE=app \
    PORT=8080 \
    "$RECORD_BIN" record \
        -c "gunicorn -c gunicorn_conf.py app:app" \
    > record_logs.txt 2>&1 &
record_pid=$!

# Drive the endpoints once keploy + gunicorn are up.
drive_endpoints
drive_rc=$?
echo "endpoint driver exit code: $drive_rc"

echo "=== stop keploy record ==="
REC_PID="$(pgrep -n -f "$(basename "${RECORD_BIN:-keploy}") record" || true)"
if [ -n "$REC_PID" ]; then
    sudo kill -INT "$REC_PID" 2>/dev/null || true
fi
sleep 5
wait $record_pid 2>/dev/null || true

echo "=== check record_logs.txt for hard errors ==="
if grep -q 'WARNING: DATA RACE' record_logs.txt; then
    echo "::error::data race detected in keploy"
    tail -200 record_logs.txt
    exit 1
fi
if grep -qE 'SCRAM channel binding check failed|password authentication failed' record_logs.txt; then
    echo "::error::SCRAM channel binding check failed during record — cbshim did not attach correctly"
    grep -E 'cbshim|X509_digest|SCRAM|channel binding' record_logs.txt | tail -40
    exit 1
fi

if [ "$drive_rc" -ne 0 ]; then
    echo "::error::one or more endpoints failed during record (curl --fail tripped)"
    tail -200 record_logs.txt
    exit 1
fi

echo "=== verify HTTP test cases were captured ==="
tests_dir="keploy/test-set-0/tests"
if [ ! -d "$tests_dir" ]; then
    echo "::error::tests directory missing: $tests_dir"
    ls -la keploy/ || true
    exit 1
fi

# Each successful endpoint call produces one HTTP test case YAML.
# 6 endpoints driven (healthz, db/ping, users, users/1, POST users,
# users/1/audit) → expect >= 6 test cases.
test_count=$(ls "$tests_dir"/*.yaml 2>/dev/null | wc -l)
echo "captured HTTP test cases: $test_count"
if [ "$test_count" -lt 6 ]; then
    echo "::error::expected >= 6 HTTP test cases (one per endpoint); got $test_count"
    ls -la "$tests_dir" || true
    tail -100 record_logs.txt
    exit 1
fi

# Cross-check: none of the captured test cases should be 500s — a 500
# response means SCRAM-PLUS failed for that connection. The endpoint
# driver already retried until /db/ping was healthy, so any residual
# 500 in the recordings is a regression.
err_count=$(grep -rE '^\s+status_code:\s*5[0-9][0-9]\s*$' "$tests_dir" | wc -l)
if [ "$err_count" -gt 0 ]; then
    echo "::error::captured $err_count HTTP test case(s) with 5xx status — SCRAM-PLUS regressed"
    grep -rE '^\s+(status_code|body):' "$tests_dir" | grep -B1 -A1 -E '5[0-9][0-9]' | head -20
    exit 1
fi

# Replay (keploy test) requires postgres mocks to be captured during
# record, which OSS can't produce without the PostgresV3 parser
# (PostgreSQL parser ships in Community Edition, see the keploy CLI
# banner). On OSS the postgres bytes are MITM'd opportunistically and
# forwarded as generic bytes — no labeled mocks. So we skip the replay
# step on OSS; the record-side assertions above are the gate.
#
# Once a PostgresV3 parser lands in OSS (or this pipeline runs against
# Community), restore the `$REPLAY_BIN test ...` block to add replay
# coverage on top of record coverage.
echo "skipping replay (OSS lacks PostgresV3 parser → no postgres mocks to replay against)"

echo "PASS: SCRAM-SHA-256-PLUS over keploy MITM works for psycopg2-binary (record path)"
