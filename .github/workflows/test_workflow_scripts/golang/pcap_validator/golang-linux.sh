#!/usr/bin/env bash
# E2E validation for Keploy's debug .kpcap capture.
#
# Records the pcap-validator sample (samples-go/pcap-validator), which
# listens on :8080 (plain HTTP) and :8443 (HTTPS) and persists every
# /touch label into Postgres + MongoDB. The orchestrator drives one
# request at each listener with a unique marker token in the JSON body
# and then asserts:
#
#   • HTTP  marker is plaintext in INCOMING kpcap (HTTP body)
#   • HTTP  marker is plaintext in OUTGOING kpcap (PG/Mongo wire)
#   • HTTPS marker is NOT plaintext in INCOMING kpcap (Keploy does not
#     terminate ingress TLS)
#   • HTTPS marker IS plaintext in OUTGOING kpcap (the app decrypts it
#     and forwards the label to PG/Mongo over plaintext wire, which
#     Keploy's outgoing proxy captures)
#
# Sourced by .github/workflows/pcap_validator.yml after the workflow
# has cd'd into samples-go/pcap-validator. Expects:
#   $RECORD_BIN          path to the keploy binary under test
#   $GITHUB_WORKSPACE    path to the keploy repo checkout

set -Eeuo pipefail

echo "$RECORD_BIN"

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh
echo "iid.sh executed"

VERIFY_DIR="$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/golang/pcap_validator/verify"

# DB_TLS=false (default): unencrypted PG/Mongo deps. Tests the basic
#                          path — Keploy captures plaintext dep traffic
#                          straight off the wire.
# DB_TLS=true:             PG with ssl=on, Mongo with --tlsMode requireTLS.
#                          Tests that Keploy's PG/Mongo parsers MITM
#                          upstream TLS — the marker submitted via
#                          HTTPS must still appear plaintext in the
#                          outgoing kpcap. Requires private parsers
#                          (keploy/integrations) to be loaded; the
#                          workflow gates this row accordingly.
DB_TLS="${DB_TLS:-false}"
case "$DB_TLS" in
  true)
    COMPOSE_ARGS=(-f docker-compose.tls.yml)
    APP_DATABASE_URL='postgres://postgres:postgres@localhost:5432/pcapdemo?sslmode=require'
    APP_MONGO_URI='mongodb://localhost:27017/?tls=true&tlsInsecure=true'
    ;;
  *)
    COMPOSE_ARGS=()
    APP_DATABASE_URL='postgres://postgres:postgres@localhost:5432/pcapdemo?sslmode=disable'
    APP_MONGO_URI='mongodb://localhost:27017'
    ;;
esac
echo "DB_TLS=$DB_TLS"

# --- Helpers ---
section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

check_for_errors() {
  local logfile=$1
  echo "Checking for errors in $logfile..."
  if [ -f "$logfile" ]; then
    if grep -q "WARNING: DATA RACE" "$logfile"; then
      echo "::error::Race condition detected in $logfile"
      cat "$logfile"
      return 1
    fi
    if grep -q "ERROR" "$logfile"; then
      echo "::warning::Errors found in $logfile (review the log above)"
    fi
  fi
}

cleanup() {
  rc=$?
  echo "::group::Cleanup (rc=$rc)"
  pgrep -f pcap-validator >/dev/null && sudo pkill -f pcap-validator || true
  pgrep keploy            >/dev/null && sudo pkill keploy            || true
  docker compose "${COMPOSE_ARGS[@]}" down -v 2>/dev/null || true
  echo "Record log:" ; sed -n '1,200p' record.log 2>/dev/null || true
  echo "::endgroup::"
  exit "$rc"
}
trap cleanup EXIT

# Build go binary with bounded retry + GOPROXY fallback. Same shape as
# http_pokeapi/golang-linux.sh — proxy.golang.org occasionally TLS-times
# out on first contact from CI runners and sinks otherwise-unrelated PRs.
build_go_app() {
  local attempt=1
  local max_attempts=4
  local sleep_sec=5
  local go_proxy="proxy.golang.org,direct"
  while [ "$attempt" -le "$max_attempts" ]; do
    if GOPROXY="$go_proxy" go build -o pcap-validator .; then
      return 0
    fi
    if [ "$attempt" -ge "$max_attempts" ]; then
      echo "::error::go build for pcap-validator failed after ${max_attempts} attempts"
      return 1
    fi
    if [ "$attempt" -eq 2 ]; then
      go_proxy="direct"
    fi
    echo "go build attempt ${attempt} failed; retrying in ${sleep_sec}s"
    sleep "$sleep_sec"
    sleep_sec=$((sleep_sec * 2))
    attempt=$((attempt + 1))
  done
}

section "Reset previous keploy artifacts"
[ -f "./keploy.yml" ] && rm ./keploy.yml
rm -rf keploy/
endsec

section "Build verifier"
( cd "$VERIFY_DIR" && go build -o "$PWD/pcap-verify-bin" . )
mv "$VERIFY_DIR/pcap-verify-bin" ./pcap-verify
endsec

section "Build pcap-validator"
build_go_app
echo "go binary built"
endsec

section "Bring up Postgres + Mongo (DB_TLS=$DB_TLS)"
if [ "$DB_TLS" = "true" ]; then
  docker compose "${COMPOSE_ARGS[@]}" up -d certgen postgres mongo
else
  docker compose "${COMPOSE_ARGS[@]}" up -d postgres mongo
fi
for i in {1..60}; do
  pg_state="$(docker inspect -f '{{.State.Health.Status}}' "$(docker compose "${COMPOSE_ARGS[@]}" ps -q postgres)" 2>/dev/null || echo starting)"
  mg_state="$(docker inspect -f '{{.State.Health.Status}}' "$(docker compose "${COMPOSE_ARGS[@]}" ps -q mongo)"    2>/dev/null || echo starting)"
  if [ "$pg_state" = "healthy" ] && [ "$mg_state" = "healthy" ]; then
    echo "deps healthy (postgres=$pg_state mongo=$mg_state)"
    break
  fi
  if [ "$i" -eq 60 ]; then
    echo "::error::deps did not become healthy (postgres=$pg_state mongo=$mg_state)"
    docker compose "${COMPOSE_ARGS[@]}" ps
    exit 1
  fi
  sleep 1
done
endsec

section "Generate keploy config"
sudo "$RECORD_BIN" config --generate
endsec

section "Start keploy record --debug"
sudo -E DATABASE_URL="$APP_DATABASE_URL" \
        MONGO_URI="$APP_MONGO_URI" \
        MONGO_DATABASE='pcapdemo' \
        "$RECORD_BIN" record \
          -c "./pcap-validator" \
          --debug \
          --generateGithubActions=false \
          2>&1 | tee record.log &
RECORD_PID=$!

for i in {1..60}; do
  if curl -fsS  http://127.0.0.1:8080/healthz  >/dev/null 2>&1 \
  && curl -fsSk https://127.0.0.1:8443/healthz >/dev/null 2>&1; then
    echo "pcap-validator ready"
    break
  fi
  if [ "$i" -eq 60 ]; then
    echo "::error::pcap-validator did not become ready"
    cat record.log
    exit 1
  fi
  sleep 1
done
endsec

section "Drive one HTTP and one HTTPS /touch request with unique markers"
HTTP_MARKER="HTTP_$(openssl rand -hex 6)"
HTTPS_MARKER="HTTPS_$(openssl rand -hex 6)"
echo "HTTP_MARKER=$HTTP_MARKER"
echo "HTTPS_MARKER=$HTTPS_MARKER"

curl -fsS  http://127.0.0.1:8080/touch \
  -H 'content-type: application/json' \
  -d "{\"label\":\"$HTTP_MARKER\"}"
echo

curl -fsSk https://127.0.0.1:8443/touch \
  -H 'content-type: application/json' \
  -d "{\"label\":\"$HTTPS_MARKER\"}"
echo
endsec

section "Stop keploy and let kpcap flush"
sleep 5
sudo pkill keploy || true
wait "$RECORD_PID" 2>/dev/null || true
sleep 2
endsec

check_for_errors record.log

section "Locate kpcap files"
INCOMING_FILE="$(find ./keploy/debug -maxdepth 1 -name 'record-incoming-*.kpcap' 2>/dev/null | head -n1 || true)"
OUTGOING_FILE="$(find ./keploy/debug -maxdepth 1 -name 'record-outgoing-*.kpcap' 2>/dev/null | head -n1 || true)"
if [ -z "$INCOMING_FILE" ] || [ -z "$OUTGOING_FILE" ]; then
  echo "::error::missing kpcap file(s) — incoming='$INCOMING_FILE' outgoing='$OUTGOING_FILE'"
  ls -la ./keploy/debug 2>/dev/null || true
  exit 1
fi
echo "incoming: $INCOMING_FILE  ($(wc -l <"$INCOMING_FILE") lines)"
echo "outgoing: $OUTGOING_FILE  ($(wc -l <"$OUTGOING_FILE") lines)"
endsec

section "Verify"
./pcap-verify \
  -incoming     "$INCOMING_FILE" \
  -outgoing     "$OUTGOING_FILE" \
  -http-marker  "$HTTP_MARKER" \
  -https-marker "$HTTPS_MARKER"
endsec

echo "pcap-validator validation passed"
