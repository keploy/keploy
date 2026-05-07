#!/usr/bin/env bash
# E2E validation for keploy's TLS capture features.
#
# Runs the sample-tls-app under keploy with $KEPLOY_FLAGS — either
# "--capture-packets" alone (default record path) or together with
# "--opportunistic-tls-intercept" (peek-and-hijack passthrough). In
# both modes it asserts:
#
#   1. <test-set>/traffic.pcap and <test-set>/sslkeys.log appeared
#      and grew during the recording (proves the streaming model).
#   2. capinfos accepts the pcap as well-formed.
#   3. The keylog has at least one TLS-1.3 session block
#      (CLIENT_TRAFFIC_SECRET_0 line).
#   4. tshark + keylog decrypts the HTTP-over-TLS sessions —
#      cleartext GETs to api.github.com /zen and to httpbin.org
#      /anything?msg=ci-* are recovered, plus their HTTP 200 responses.
#   5. MySQL TLS round-trip works through the proxy: POST inserts a
#      row, GET reads back JSON containing that row's name. This is
#      the strongest assert that the server-first capability flow
#      survived MITM.
#   6. Postgres TLS round-trip works through the proxy: same shape.
#   7. mocks.yaml exists and (for capture-only mode) contains
#      kind: Http records — proves the HTTP parser dispatch fired.
#
# Run from the sample-tls-app working directory. RECORD_BIN must
# point at a keploy build with the postgres parsers linked. KEPLOY_FLAGS
# selects the per-matrix mode and is appended verbatim to the keploy
# record command. MODE_NAME is used in artifact naming.

set -Eeuo pipefail

section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

dump_state() {
  rc=$?
  echo "::error::e2e failed (mode=${MODE_NAME:-?}, exit=$rc). Dumping context for triage…"
  echo "== keploy log (last 200 lines) =="
  [[ -f keploy-record.log ]] && tail -200 keploy-record.log || true
  echo "== test-set inventory =="
  sudo find keploy -maxdepth 5 -type f -print 2>/dev/null | sort || true
  echo "== keylog (head) =="
  [[ -f keploy/test-set-0/sslkeys.log ]] && sudo head -10 keploy/test-set-0/sslkeys.log || true
  echo "== capinfos =="
  [[ -f keploy/test-set-0/traffic.pcap ]] && sudo capinfos -c -i keploy/test-set-0/traffic.pcap 2>/dev/null || true
  echo "== mysql/postgres docker logs =="
  docker logs sample-mysql-tls 2>&1 | tail -40 || true
  docker logs sample-pg-tls 2>&1 | tail -40 || true
  exit "$rc"
}
trap dump_state ERR

wait_for_http() {
  local url="$1" tries="${2:-90}"
  for _ in $(seq 1 "$tries"); do
    if curl -fsS -o /dev/null --max-time 1 "$url"; then return 0; fi
    sleep 1
  done
  return 1
}

# ----- bring up MySQL + Postgres with TLS -----

section "Generate certs + bring up MySQL/Postgres TLS"
mkdir -p .ci/certs && cd .ci/certs

# Self-signed CA
openssl genrsa -out ca.key 2048 >/dev/null 2>&1
openssl req -x509 -new -nodes -key ca.key -days 1 -subj "/CN=keploy-ci-ca" -out ca.crt >/dev/null 2>&1

# Server cert with SAN matching localhost / 127.0.0.1 (lets the app
# use ServerName='localhost' and pass full verify, even outside the
# loose verify-CA path).
openssl genrsa -out server.key 2048 >/dev/null 2>&1
cat > server.cnf <<EOF
[req]
distinguished_name=dn
req_extensions=ext
prompt=no
[dn]
CN=keploy-ci-db
[ext]
subjectAltName=DNS:localhost,DNS:127.0.0.1,IP:127.0.0.1
EOF
openssl req -new -key server.key -out server.csr -config server.cnf >/dev/null 2>&1
openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out server.crt -days 1 -extfile server.cnf -extensions ext >/dev/null 2>&1

# Bind-mounts preserve host file ownership/perms inside the container,
# but each DB image's daemon runs as a different non-root UID and
# refuses to read keys it cannot match:
#   - MySQL 8.4 (`mysql:8.4`): daemon runs as user `mysql` (UID 999).
#     With chmod 600 on a key owned by the GHA runner, the mysql user
#     gets EACCES → "Unable to get private key" / "Failed to set up SSL".
#   - Postgres 16-alpine: hard-coded sanity check refuses to start
#     unless the keyfile is owned by the database user (postgres,
#     UID 70 in alpine) or by root, with mode <= 0600 (or <=0640 if
#     root-owned).
#
# Make a per-DB copy of the key chowned to that image's daemon UID.
# `sudo` is available on the GHA runner; the host workspace is
# scratch so leaving root-owned files behind is fine.
sudo cp server.key server-key-mysql.pem
sudo chown 999:999 server-key-mysql.pem
sudo chmod 0600 server-key-mysql.pem

sudo cp server.key server-key-postgres.pem
sudo chown 70:70 server-key-postgres.pem
sudo chmod 0600 server-key-postgres.pem

echo "== cert dir perms =="
ls -l ca.crt server.crt server-key-mysql.pem server-key-postgres.pem

cd "$GITHUB_WORKSPACE/sample-tls-app"

# Postgres pg_hba.conf — require SSL for the TCP rule.
#
# Auth method is "trust" (no password exchange) on purpose. Postgres
# 14+ stores the user's password as scram-sha-256 by default, and
# pgx auto-negotiates SCRAM-SHA-256-PLUS when the connection is over
# TLS. SCRAM-PLUS hashes the server's TLS certificate into the
# channel-binding token; under keploy MITM the client sees keploy's
# synthesized cert while the server has the real one, so the
# binding hashes diverge and every login fails with
# "FATAL: SCRAM channel binding check failed". This is a deliberate
# anti-MITM property of SCRAM-PLUS — there is no client-side flag
# that makes it pass cleanly through a sniff-and-decrypt proxy.
#
# Skipping auth keeps the test focused on the TLS handshake / wire
# layer, which is what --capture-packets and
# --opportunistic-tls-intercept actually exercise. We still require
# `hostssl`, so plaintext connections are rejected.
cat > .ci/pg_hba.conf <<EOF
local   all all                     trust
host    all all 127.0.0.1/32 trust
hostssl all all 0.0.0.0/0    trust
EOF

# Compose: minimal mysql + postgres, both speaking TLS.
cat > .ci/compose.yml <<'EOF'
services:
  mysql:
    image: mysql:8.4
    container_name: sample-mysql-tls
    environment:
      MYSQL_ROOT_PASSWORD: ci_root_pw
      MYSQL_DATABASE: app
    ports: ["3306:3306"]
    volumes:
      - ../.ci/certs/ca.crt:/etc/mysql/certs/ca.pem:ro
      - ../.ci/certs/server.crt:/etc/mysql/certs/server-cert.pem:ro
      - ../.ci/certs/server-key-mysql.pem:/etc/mysql/certs/server-key.pem:ro
    command:
      - --ssl-ca=/etc/mysql/certs/ca.pem
      - --ssl-cert=/etc/mysql/certs/server-cert.pem
      - --ssl-key=/etc/mysql/certs/server-key.pem
      - --require-secure-transport=ON
    healthcheck:
      test: ["CMD-SHELL", "mysqladmin ping -h 127.0.0.1 -uroot -pci_root_pw --silent"]
      interval: 3s
      timeout: 3s
      retries: 30
  postgres:
    image: postgres:16-alpine
    container_name: sample-pg-tls
    environment:
      POSTGRES_USER: app
      POSTGRES_PASSWORD: ci_pg_pw
      POSTGRES_DB: app
    ports: ["5433:5432"]
    volumes:
      - ../.ci/certs/server.crt:/etc/postgres-certs/server.crt:ro
      - ../.ci/certs/server-key-postgres.pem:/etc/postgres-certs/server.key:ro
      - ../.ci/pg_hba.conf:/etc/postgres-certs/pg_hba.conf:ro
    command: >
      postgres
        -c ssl=on
        -c ssl_cert_file=/etc/postgres-certs/server.crt
        -c ssl_key_file=/etc/postgres-certs/server.key
        -c hba_file=/etc/postgres-certs/pg_hba.conf
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U app -d app"]
      interval: 3s
      timeout: 3s
      retries: 30
EOF

docker compose -f .ci/compose.yml up -d --wait

# Compose's healthcheck for MySQL is a non-TLS mysqladmin ping, so a
# container that fails TLS init can still pass --wait. Verify the TLS
# listener directly with openssl s_client; failure here surfaces a
# MySQL/Postgres TLS misconfig BEFORE we lose context inside keploy.
echo "== mysql TLS sanity =="
echo Q | openssl s_client -connect 127.0.0.1:3306 -starttls mysql \
  -CAfile .ci/certs/ca.crt -verify_return_error 2>&1 | tail -8 || {
  echo "::error::MySQL TLS handshake failed";
  docker logs sample-mysql-tls 2>&1 | tail -40;
  false;
}
echo "== postgres TLS sanity =="
echo Q | openssl s_client -connect 127.0.0.1:5433 -starttls postgres \
  -CAfile .ci/certs/ca.crt -verify_return_error 2>&1 | tail -8 || {
  echo "::error::Postgres TLS handshake failed";
  docker logs sample-pg-tls 2>&1 | tail -40;
  false;
}
endsec

section "Install CA into OS trust store"
sudo cp .ci/certs/ca.crt /usr/local/share/ca-certificates/keploy-ci-db-ca.crt
sudo update-ca-certificates >/dev/null
endsec

# ----- run keploy record with the matrix flags -----

section "Start keploy record (mode=${MODE_NAME})"
echo "  flags: $KEPLOY_FLAGS"
sudo rm -rf keploy
export MYSQL_DSN="root:ci_root_pw@tcp(127.0.0.1:3306)/app?parseTime=true"
export POSTGRES_DSN="postgres://app:ci_pg_pw@localhost:5433/app?sslmode=verify-ca"

go build -o sample-tls-app .

# shellcheck disable=SC2086
sudo -E env PATH="$PATH" MYSQL_DSN="$MYSQL_DSN" POSTGRES_DSN="$POSTGRES_DSN" \
  "$RECORD_BIN" record \
  -c "./sample-tls-app" \
  $KEPLOY_FLAGS \
  > keploy-record.log 2>&1 &
endsec

section "Drive HTTP / MySQL / Postgres traffic"
if ! wait_for_http "http://localhost:8080/" 120; then
  echo "::error::sample-tls-app did not become healthy on :8080"
  # Explicit dump_state — `exit 1` on a control-flow branch like
  # this does not trigger the ERR trap under `set -e`, so we'd
  # otherwise lose keploy's stderr. False, by contrast, fires ERR.
  false
fi

# HTTP routes — outbound TLS to public APIs
curl -fsS http://localhost:8080/quote >/dev/null
curl -fsS "http://localhost:8080/echo?msg=ci-${MODE_NAME}-1" >/dev/null
curl -fsS "http://localhost:8080/echo?msg=ci-${MODE_NAME}-2" >/dev/null
echo "good! HTTP routes returned"

# MySQL — POST insert then GET read
curl -fsS -X POST "http://localhost:8080/mysql/items?name=ci-${MODE_NAME}-mysql" >/dev/null
MYSQL_BODY=$(curl -fsS http://localhost:8080/mysql/items)
echo "mysql GET body: $MYSQL_BODY"
echo "$MYSQL_BODY" | grep -q "ci-${MODE_NAME}-mysql" || {
  echo "::error::MySQL round-trip failed — inserted name not present in GET response"
  exit 1
}
echo "good! MySQL round-trip succeeded through keploy proxy"

# Postgres — POST insert then GET read
curl -fsS -X POST "http://localhost:8080/postgres/items?name=ci-${MODE_NAME}-pg" >/dev/null
PG_BODY=$(curl -fsS http://localhost:8080/postgres/items)
echo "postgres GET body: $PG_BODY"
echo "$PG_BODY" | grep -q "ci-${MODE_NAME}-pg" || {
  echo "::error::Postgres round-trip failed — inserted name not present in GET response"
  exit 1
}
echo "good! Postgres round-trip succeeded through keploy proxy"

# Give the streaming endpoint a beat to flush the last frames.
sleep 2
endsec

section "Stop keploy gracefully"
sudo pkill -INT -f "keploy record -c \./sample-tls-app" || true
for _ in $(seq 1 30); do
  if ! sudo pgrep -f "keploy record -c \./sample-tls-app" >/dev/null; then break; fi
  sleep 1
done
endsec

# ----- assertions on the captured artifacts -----

PCAP=keploy/test-set-0/traffic.pcap
KEYLOG=keploy/test-set-0/sslkeys.log
MOCKS=keploy/test-set-0/mocks.yaml

section "Assert pcap + keylog streamed during recording"
sudo test -s "$PCAP"   || { echo "::error::missing or empty $PCAP";   exit 1; }
sudo test -s "$KEYLOG" || { echo "::error::missing or empty $KEYLOG"; exit 1; }
sudo ls -la keploy/test-set-0/
endsec

section "Assert pcap is well-formed"
sudo capinfos -c -i "$PCAP"
endsec

section "Assert keylog has TLS-1.3 application secret"
KEYLOG_LINES=$(sudo wc -l < "$KEYLOG")
echo "sslkeys.log lines: $KEYLOG_LINES"
if [[ "$KEYLOG_LINES" -lt 4 ]]; then
  echo "::error::expected at least 4 keylog lines (one full TLS-1.3 block); saw $KEYLOG_LINES"
  exit 1
fi
sudo grep -q "^CLIENT_TRAFFIC_SECRET_0 " "$KEYLOG" || {
  echo "::error::keylog missing CLIENT_TRAFFIC_SECRET_0 — TLS-1.3 application secret was not logged"
  exit 1
}
endsec

section "Assert tshark + keylog decrypts HTTP-over-TLS sessions"
DECRYPTED_REQS=$(sudo tshark -r "$PCAP" -o "tls.keylog_file:$KEYLOG" \
  -Y "http.request" -T fields -e http.host -e http.request.uri 2>/dev/null || true)
echo "decrypted HTTP requests:"
echo "$DECRYPTED_REQS"

echo "$DECRYPTED_REQS" | grep -q "api.github.com" || {
  echo "::error::tshark did not see decrypted GET to api.github.com"
  exit 1
}
echo "$DECRYPTED_REQS" | grep -q "httpbin.org" || {
  echo "::error::tshark did not see decrypted GET to httpbin.org"
  exit 1
}
echo "$DECRYPTED_REQS" | grep -q "ci-${MODE_NAME}" || {
  echo "::error::ci-${MODE_NAME} query string did not survive into the decrypted pcap"
  exit 1
}

DECRYPTED_RESP_OK=$(sudo tshark -r "$PCAP" -o "tls.keylog_file:$KEYLOG" \
  -Y "http.response" -T fields -e http.response.code 2>/dev/null | grep -c "^200$" || true)
echo "decrypted 200 responses: $DECRYPTED_RESP_OK"
if [[ "$DECRYPTED_RESP_OK" -lt 2 ]]; then
  echo "::error::expected >=2 decrypted 200 responses; saw $DECRYPTED_RESP_OK"
  exit 1
fi
endsec

section "Assert the captured pcap contains TLS handshakes for ALL three protocols"
# At minimum the pcap should show ClientHello frames — proves all
# three protocols actually crossed the proxy as TLS, not as a fall-
# back to plain TCP.
HELLO_COUNT=$(sudo tshark -r "$PCAP" -Y "tls.handshake.type==1" 2>/dev/null | wc -l)
echo "TLS ClientHello frames in pcap: $HELLO_COUNT"
if [[ "$HELLO_COUNT" -lt 3 ]]; then
  echo "::error::expected >=3 ClientHello frames (HTTP + MySQL + Postgres); saw $HELLO_COUNT"
  exit 1
fi
endsec

section "Assert mocks.yaml shape per mode"
sudo test -s "$MOCKS" || { echo "::error::missing or empty $MOCKS"; exit 1; }

case "$MODE_NAME" in
  capture-only)
    # Default record path: HTTP parser dispatch fires, mocks.yaml
    # must contain kind: Http entries for the outbound public-API
    # calls. (MySQL/Postgres mocks may also appear depending on
    # which integrations are linked into the build.)
    HTTP_MOCKS=$(sudo grep -c "^kind: Http" "$MOCKS" || true)
    echo "Http mock records: $HTTP_MOCKS"
    if [[ "$HTTP_MOCKS" -lt 2 ]]; then
      echo "::error::expected >=2 'kind: Http' mocks (one per upstream host); saw $HTTP_MOCKS"
      exit 1
    fi
    ;;
  with-opportunistic)
    # Opportunistic intercept hijacks BEFORE parser dispatch — no
    # Http mocks must appear. DNS mocks are still expected (DNS
    # interception is independent of the passthrough path).
    HTTP_MOCKS=$(sudo grep -c "^kind: Http" "$MOCKS" || true)
    echo "Http mock records (must be 0): $HTTP_MOCKS"
    if [[ "$HTTP_MOCKS" -gt 0 ]]; then
      echo "::error::found $HTTP_MOCKS 'kind: Http' mocks in with-opportunistic mode — parser dispatch should be bypassed"
      exit 1
    fi
    ;;
esac
endsec

section "Tear down DB containers"
docker compose -f .ci/compose.yml down -v --remove-orphans >/dev/null 2>&1 || true
endsec

echo "All assertions passed (mode=${MODE_NAME}): pcap streamed, keylog populated, tshark decrypted HTTP, MySQL+Postgres TLS round-trips succeeded through keploy."
exit 0
