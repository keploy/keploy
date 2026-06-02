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
cleanup() {
    echo "cleanup..."
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

# A tiny driver — fires one request per endpoint with retries while
# the app warms up. Each successful request opens a new libpq conn,
# re-runs SCRAM-PLUS, exercises the cbshim path. If cbshim isn't
# working, the corresponding endpoint returns 500.
drive_endpoints() {
    local rc=0
    # Wait for the app to start responding to /healthz (gunicorn takes
    # a couple of seconds and keploy adds its own warmup window).
    for i in $(seq 1 30); do
        if curl -sf "${APP_URL}/healthz" >/dev/null; then
            echo "app healthy after ${i}s"
            break
        fi
        sleep 1
    done

    # The actual endpoint sequence. Each `curl --fail` exits non-zero
    # on any HTTP response >= 400, so a SCRAM-PLUS failure (500 from
    # the OperationalError handler) trips this.
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

# gunicorn_conf.py isn't shipped — generate a minimal one so the
# matrix doesn't have to ship one too.
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

echo "=== verify mocks were captured ==="
mocks_file="keploy/test-set-0/mocks.yaml"
if [ ! -s "$mocks_file" ]; then
    echo "::error::mocks file missing or empty: $mocks_file"
    ls -la keploy/ || true
    exit 1
fi

# Each captured postgres mock has `kind: Postgres` (or PostgresV3 in
# newer captures). Five DB-touching endpoints were called above.
pg_mock_count=$(grep -cE '^kind:\s+(Postgres|PostgresV3)$' "$mocks_file" 2>/dev/null || echo 0)
echo "captured postgres mocks: $pg_mock_count"
if [ "$pg_mock_count" -lt 5 ]; then
    echo "::error::expected >= 5 postgres mocks (one per DB-touching endpoint); got $pg_mock_count"
    echo "--- mocks.yaml head ---"
    head -120 "$mocks_file"
    exit 1
fi

echo "=== run keploy test (replay) ==="
sudo -E env PATH="$PATH" \
    PGHOST=127.0.0.1 PGPORT=5432 PGUSER=app PGPASSWORD=app-secret PGDATABASE=app \
    PORT=8080 \
    "$REPLAY_BIN" test \
        -c "gunicorn -c gunicorn_conf.py app:app" \
        --delay 10 \
        --apiTimeout 30 \
    > test_logs.txt 2>&1
test_rc=$?
echo "keploy test exit code: $test_rc"

if [ "$test_rc" -ne 0 ]; then
    echo "::error::keploy test (replay) failed"
    tail -200 test_logs.txt
    exit 1
fi

# Sanity-check: replay's own test-result summary should report all
# passed. The exact format varies by version; grep generously.
if grep -qE 'failed|FAIL' test_logs.txt && ! grep -qE 'failed.*0|FAIL.*0|0 failed' test_logs.txt; then
    echo "::error::keploy test reported failed test cases"
    grep -B2 -A2 -E 'failed|FAIL' test_logs.txt | head -40
    exit 1
fi

echo "PASS: SCRAM-SHA-256-PLUS over keploy MITM works for psycopg2-binary (record + replay)"
