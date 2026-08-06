#!/usr/bin/env bash
# E2E validation for keploy's TLS capture features.
#
# Runs the sample-tls-app under keploy with $KEPLOY_FLAGS — either
# "--capture-packets" alone (default record path), or together with
# "--opportunistic-tls-intercept" (peek-and-hijack passthrough), or
# together with "--upstream-tls-verify" (opt-in upstream certificate
# verification), or with BOTH of the latter two (the opportunistic
# hijack's own upstream dial site, which is not reachable from any of
# the other three). In every mode it asserts:
#
#   1. <test-set>/traffic.pcap and <test-set>/sslkeys.log appeared
#      and grew during the recording (proves the streaming model).
#   2. capinfos accepts the pcap as well-formed.
#   3. The keylog has at least one TLS-1.3 session block
#      (CLIENT_TRAFFIC_SECRET_0 line).
#   4. tshark + keylog decrypts the HTTP-over-TLS sessions —
#      cleartext GETs to the two local upstream hosts
#      (quote.keploy.local /zen and echo.keploy.local /anything?msg=ci-*)
#      are recovered, plus their HTTP 200 responses.
#   5. MySQL TLS round-trip works through the proxy: POST inserts a
#      row, GET reads back JSON containing that row's name. This is
#      the strongest assert that the server-first capability flow
#      survived MITM.
#   6. Postgres TLS round-trip works through the proxy: same shape.
#   7. mocks.yaml exists and (for capture-only mode) contains
#      kind: Http records — proves the HTTP parser dispatch fired.
#   8. For the verify-upstream* modes: the agent reported that upstream
#      certificate verification was actually ON. In verify-upstream the
#      mock inventory must still cover all three upstreams (Http, MySQL,
#      Postgres) — see that case below for why the mocks, not the exit
#      code, are the load-bearing assertion. In
#      verify-upstream-opportunistic it is the reverse: the hijack owns
#      the connection, so a verification failure kills the application's
#      socket and the round-trips above are what fail.
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
  echo "== local TLS upstream log =="
  [[ -f tls-upstream.log ]] && tail -40 tls-upstream.log || true
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

# Upstream HTTPS cert — the sample app's /quote and /echo routes used to
# proxy to live public services (api.github.com / httpbin.org), which
# rate-limit and go down for minutes at a time and were the last source
# of flakiness in this job. We now point both routes at a LOCAL TLS
# upstream (ci/tls-upstream, shipped with the sample app) over two
# hostnames so the two-distinct-host shape the assertions expect is
# preserved. The cert is signed by the
# same CI CA that gets installed into the OS trust store below, with
# SANs for both hostnames, so the app's system-pool verification passes
# directly and through keploy's MITM exactly as the public certs did.
#
# The SANs are DNS-ONLY, deliberately. This cert used to also carry
# DNS:localhost and IP:127.0.0.1, and that IP SAN was actively harmful:
# the app reaches these hostnames over loopback, so keploy's destination
# address is 127.0.0.1:7443, and an IP SAN made "verify against the dial
# address" succeed BY ACCIDENT. Every ServerName-plumbing defect on the
# verifying path — keploy checking the destination IP instead of the SNI
# the application sent — therefore passed CI while breaking every
# hostname-addressed upstream in the real world. Without the IP SAN the
# HTTPS leg can only verify if keploy carries the application's SNI
# through to its own dial, which is the property under test. Nothing
# dials this listener by IP or as "localhost" (grep UPSTREAM_PORT: every
# reference is $QUOTE_HOST / $ECHO_HOST), so no other mode is affected.
# The DB cert (server.cnf above) keeps its IP SAN on purpose — MySQL is
# dialled at an IP literal and sends no SNI, which is the OTHER path.
openssl genrsa -out upstream.key 2048 >/dev/null 2>&1
cat > upstream.cnf <<EOF
[req]
distinguished_name=dn
req_extensions=ext
prompt=no
[dn]
CN=keploy-ci-upstream
[ext]
subjectAltName=DNS:quote.keploy.local,DNS:echo.keploy.local
EOF
openssl req -new -key upstream.key -out upstream.csr -config upstream.cnf >/dev/null 2>&1
openssl x509 -req -in upstream.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out upstream.crt -days 1 -extfile upstream.cnf -extensions ext >/dev/null 2>&1

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

# ----- bring up the local HTTPS upstream for /quote and /echo -----
#
# Two hostnames, both resolving to loopback, served by one TLS-1.3
# process (cert SANs cover both). keploy intercepts loopback the same
# way it does for the MySQL/Postgres containers below, so these calls
# still cross the proxy as real outbound TLS.
QUOTE_HOST=quote.keploy.local
ECHO_HOST=echo.keploy.local
UPSTREAM_PORT=7443

section "Start local TLS upstream (replaces api.github.com / httpbin.org)"
echo "127.0.0.1 ${QUOTE_HOST} ${ECHO_HOST}" | sudo tee -a /etc/hosts >/dev/null
# The upstream fixture ships with the sample app (ci/tls-upstream); build
# and run it from the checked-out app repo. Pre-build so $! is the server
# PID itself (not a `go run` wrapper), which the teardown kill relies on.
go build -o tls-upstream ./ci/tls-upstream
./tls-upstream .ci/certs/upstream.crt .ci/certs/upstream.key 127.0.0.1 "$UPSTREAM_PORT" \
  > tls-upstream.log 2>&1 &
UPSTREAM_PID=$!

# Wait for the TLS listener to accept before we hand its URLs to the app.
for _ in $(seq 1 30); do
  if curl -fsS -o /dev/null --max-time 1 --cacert .ci/certs/ca.crt \
       "https://${QUOTE_HOST}:${UPSTREAM_PORT}/zen"; then
    break
  fi
  if ! kill -0 "$UPSTREAM_PID" 2>/dev/null; then
    echo "::error::local TLS upstream exited during startup"
    cat tls-upstream.log || true
    false
  fi
  sleep 1
done
echo "== local TLS upstream sanity =="
curl -fsS --cacert .ci/certs/ca.crt "https://${ECHO_HOST}:${UPSTREAM_PORT}/anything?msg=probe"
echo
endsec

# ----- run keploy record with the matrix flags -----

section "Start keploy record (mode=${MODE_NAME})"
echo "  flags: $KEPLOY_FLAGS"
sudo rm -rf keploy
export MYSQL_DSN="root:ci_root_pw@tcp(127.0.0.1:3306)/app?parseTime=true"
export POSTGRES_DSN="postgres://app:ci_pg_pw@localhost:5433/app?sslmode=verify-ca"
# Point the HTTP-over-TLS routes at the local upstream instead of the
# flaky public services. The app defaults to api.github.com/httpbin.org
# when these are unset, so this only affects CI.
export QUOTE_URL="https://${QUOTE_HOST}:${UPSTREAM_PORT}/zen"
export ECHO_URL="https://${ECHO_HOST}:${UPSTREAM_PORT}/anything"

go build -o sample-tls-app .

# shellcheck disable=SC2086
sudo -E env PATH="$PATH" MYSQL_DSN="$MYSQL_DSN" POSTGRES_DSN="$POSTGRES_DSN" \
  QUOTE_URL="$QUOTE_URL" ECHO_URL="$ECHO_URL" \
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

# HTTP routes — outbound TLS to the LOCAL upstream.
#
# /quote and /echo now proxy to the local TLS-1.3 upstream started
# above (QUOTE_URL / ECHO_URL), not to api.github.com / httpbin.org.
# That removes the only remaining flaky dependency in this job — the
# public services rate-limited and went down for minutes at a time,
# which no amount of retrying can ride out. The retry wrapper is kept
# as cheap insurance against the local listener being a beat slow to
# accept right after the app starts; a genuine keploy-side break (the
# proxy dropping the outbound TLS connection → the app returns 502)
# still surfaces once the small retry budget is exhausted.
retry_curl() {
  curl -fsS --retry 5 --retry-delay 1 --retry-max-time 30 --retry-all-errors "$@"
}

retry_curl http://localhost:8080/quote >/dev/null
retry_curl "http://localhost:8080/echo?msg=ci-${MODE_NAME}-1" >/dev/null
retry_curl "http://localhost:8080/echo?msg=ci-${MODE_NAME}-2" >/dev/null
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

if [[ "$MODE_NAME" == verify-upstream* ]]; then
  section "Assert the agent actually turned upstream TLS verification ON"
  # --upstream-tls-verify FAILS OPEN by design: if the trust anchors
  # cannot be loaded, proxy.New logs the error, flips upstreamTLSVerify
  # back to false and records exactly as the default path does. That is
  # the correct production behaviour — a misconfigured CA must never turn
  # into silently dropped mocks — but it means a green run proves nothing
  # on its own: this job would just be re-testing capture-only under a
  # different name. So assert the positive log line the agent emits once
  # per process after it has resolved the pool (pkg/agent/proxy/proxy.go,
  # "upstream TLS certificate verification is enabled"). The native agent
  # is a child process whose stdout/stderr are the record command's, so
  # its logs land in keploy-record.log with everything else.
  grep -q "upstream TLS certificate verification is enabled" keploy-record.log || {
    echo "::error::the agent never reported upstream TLS verification enabled — either --upstream-tls-verify did not reach it, or loading the trust anchors failed and verification silently fell back to skip"
    echo "== upstream-TLS lines in keploy-record.log =="
    grep -n -i "upstream tls" keploy-record.log || echo "(none — the flag never reached the agent)"
    exit 1
  }
  echo "good! agent reported upstream TLS certificate verification enabled"
  endsec
fi

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

echo "$DECRYPTED_REQS" | grep -q "$QUOTE_HOST" || {
  echo "::error::tshark did not see decrypted GET to $QUOTE_HOST"
  exit 1
}
echo "$DECRYPTED_REQS" | grep -q "$ECHO_HOST" || {
  echo "::error::tshark did not see decrypted GET to $ECHO_HOST"
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

section "Assert the captured pcap contains the HTTP-over-TLS ClientHellos"
# tshark only dissects a ClientHello when the TCP stream BEGINS with a
# TLS record. That holds for the HTTP-over-TLS sessions, so we assert a
# ClientHello carrying each upstream host's SNI actually crossed the
# proxy (proving the traffic went out as TLS, not a fall-back to plain
# TCP). We key on the SNI rather than a raw frame count: the app shares
# one keep-alive client, so the two /echo calls collapse onto a single
# connection — a count-based check is at the mercy of connection reuse
# and was the real reason this step flaked (it silently leaned on the
# old public endpoints NOT reusing connections to reach its threshold).
#
# MySQL/Postgres are deliberately NOT checked here: their streams open
# with the protocol's STARTTLS preamble (MySQL server greeting /
# Postgres SSLRequest) before the embedded ClientHello, so tshark's TLS
# dissector can't latch onto them on keploy's proxy port. Their TLS is
# proven instead by the openssl s_client sanity above and the
# round-trip asserts (POST→GET through the proxy) earlier in this run.
HELLO_SNIS=$(sudo tshark -r "$PCAP" -Y "tls.handshake.type==1" \
  -T fields -e tls.handshake.extensions_server_name 2>/dev/null | sort -u)
HELLO_COUNT=$(sudo tshark -r "$PCAP" -Y "tls.handshake.type==1" 2>/dev/null | wc -l)
echo "TLS ClientHello frames in pcap: $HELLO_COUNT"
echo "ClientHello SNIs:"; echo "$HELLO_SNIS"
echo "$HELLO_SNIS" | grep -q "$QUOTE_HOST" || {
  echo "::error::no captured ClientHello carried SNI $QUOTE_HOST — /quote did not cross the proxy as TLS"
  exit 1
}
echo "$HELLO_SNIS" | grep -q "$ECHO_HOST" || {
  echo "::error::no captured ClientHello carried SNI $ECHO_HOST — /echo did not cross the proxy as TLS"
  exit 1
}
endsec

section "Assert mocks.yaml shape per mode"
case "$MODE_NAME" in
  capture-only)
    # Default record path: HTTP parser dispatch fires, so mocks.yaml
    # must exist and contain kind: Http entries — one per upstream host.
    # (MySQL/Postgres mocks may also appear depending on which
    # integrations are linked into the build.)
    sudo test -s "$MOCKS" || { echo "::error::missing or empty $MOCKS"; exit 1; }
    HTTP_MOCKS=$(sudo grep -c "^kind: Http" "$MOCKS" || true)
    echo "Http mock records: $HTTP_MOCKS"
    if [[ "$HTTP_MOCKS" -lt 2 ]]; then
      echo "::error::expected >=2 'kind: Http' mocks (one per upstream host); saw $HTTP_MOCKS"
      exit 1
    fi
    ;;
  with-opportunistic)
    # Opportunistic intercept hijacks BEFORE parser dispatch, so NO Http
    # mocks must appear — that's the whole invariant under test here.
    #
    # mocks.yaml itself may legitimately be absent or empty in this mode:
    # the upstream hosts resolve via /etc/hosts (see the local-upstream
    # setup), and the DB DSNs use 127.0.0.1 / localhost, so the workload
    # issues no outbound DNS — there are no DNS mocks to write, and with
    # parser dispatch bypassed there are no Http mocks either. Treat a
    # missing/empty file as zero Http mocks rather than a failure.
    if sudo test -s "$MOCKS"; then
      HTTP_MOCKS=$(sudo grep -c "^kind: Http" "$MOCKS" || true)
    else
      HTTP_MOCKS=0
    fi
    echo "Http mock records (must be 0): $HTTP_MOCKS"
    if [[ "$HTTP_MOCKS" -gt 0 ]]; then
      echo "::error::found $HTTP_MOCKS 'kind: Http' mocks in with-opportunistic mode — parser dispatch should be bypassed"
      exit 1
    fi
    ;;
  verify-upstream)
    # Same record path as capture-only (parsers still dispatch), except
    # every outbound dial keploy makes now authenticates the REAL
    # upstream instead of skipping. The trust material is already in
    # place: the CI CA was installed into the OS trust store above, and
    # it signed both the DB cert (SANs DNS:localhost, IP:127.0.0.1) and
    # the HTTPS upstream cert (SANs DNS:quote.keploy.local,
    # DNS:echo.keploy.local), so nothing extra needs configuring.
    #
    # The three upstreams cover the two distinct ServerName paths:
    #   - the HTTPS upstreams send SNI, so keploy verifies the exact name
    #     the app asked for — and their cert has NO IP SAN, so this leg
    #     can ONLY pass if keploy carried that SNI through to its own
    #     dial rather than substituting the destination IP;
    #   - MySQL is dialled at an IP literal (tcp(127.0.0.1:3306)) and
    #     RFC 6066 forbids IP literals in SNI, so keploy captures NO
    #     ServerName and must fall back to the dial address and match the
    #     cert's IP:127.0.0.1 SAN. Postgres lands on whichever of the two
    #     paths its driver picks for sslmode=verify-ca; the cert carries
    #     DNS:localhost *and* IP:127.0.0.1, so either is covered.
    #
    # Why the MOCKS are the assertion and not the exit code: a
    # verification error does NOT fail the application. The dest-side
    # handshake fails, the supervisor falls through to raw passthrough,
    # the app's connection continues to work — and the mock is DROPPED.
    # Every round-trip driven above would still return 200 and this job
    # would go green having captured nothing. The mock inventory is the
    # only signal that separates "verified, then recorded" from "failed
    # verification, then silently degraded".
    sudo test -s "$MOCKS" || {
      echo "::error::missing or empty $MOCKS with verification on — every upstream failed to verify and fell through to raw passthrough"
      exit 1
    }
    echo "== recorded mock kinds =="
    sudo grep "^kind: " "$MOCKS" | sort | uniq -c || true

    HTTP_MOCKS=$(sudo grep -c "^kind: Http" "$MOCKS" || true)
    MYSQL_MOCKS=$(sudo grep -c "^kind: MySQL" "$MOCKS" || true)
    # The `Postgres` prefix deliberately covers all three parser
    # generations (Postgres / PostgresV2 / PostgresV3) — which one is
    # linked is a property of the build, not of this feature.
    PG_MOCKS=$(sudo grep -c "^kind: Postgres" "$MOCKS" || true)
    echo "Http mocks: $HTTP_MOCKS, MySQL mocks: $MYSQL_MOCKS, Postgres mocks: $PG_MOCKS"

    if [[ "$HTTP_MOCKS" -lt 2 ]]; then
      echo "::error::expected >=2 'kind: Http' mocks (one per upstream host) with verification on; saw $HTTP_MOCKS — the HTTPS upstream's certificate failed to verify against the CI CA and the sessions degraded to raw passthrough"
      exit 1
    fi
    if [[ "$MYSQL_MOCKS" -lt 1 ]]; then
      echo "::error::no 'kind: MySQL' mocks with verification on — MySQL is dialled at an IP literal so no SNI is sent; this is the ServerName-from-dial-address fallback failing to match the cert's IP:127.0.0.1 SAN"
      exit 1
    fi
    if [[ "$PG_MOCKS" -lt 1 ]]; then
      echo "::error::no 'kind: Postgres*' mocks with verification on — the Postgres dest-side handshake failed to verify and the session fell through to raw passthrough"
      exit 1
    fi
    echo "good! all three upstreams verified and recorded with --upstream-tls-verify"
    ;;
  verify-upstream-opportunistic)
    # --opportunistic-tls-intercept + --upstream-tls-verify. This is the
    # combination that the verify-upstream mode alone cannot reach: the
    # opportunistic path hijacks the connection BEFORE parser dispatch and
    # owns its own upstream tls.Config (proxy/opportunistic_tls.go,
    # hijackAndMITM), which is a completely separate dial site from the
    # one verify-upstream exercises.
    #
    # The load-bearing assertion for this mode is NOT down here — it is the
    # `retry_curl http://localhost:8080/quote` above. On this path a failed
    # upstream verification is not a dropped mock, it is a dropped
    # CONNECTION: hijackAndMITM defers srcConn.Close()/dstConn.Close() and
    # its error propagates out of handleConnection, so the application's
    # socket dies after it had already completed its handshake with keploy
    # and the route returns 502. The upstream cert carries DNS SANs only
    # (see upstream.cnf), so that is exactly what happens if keploy dials
    # with the destination IP as its ServerName instead of the SNI the app
    # sent — the defect this combination exists to catch.
    #
    # Mock shape here is the with-opportunistic shape, not the
    # verify-upstream one: parser dispatch is bypassed, so there must be
    # NO Http mocks. Verification changes which upstreams keploy is
    # willing to talk to, never whether parsers run.
    if sudo test -s "$MOCKS"; then
      HTTP_MOCKS=$(sudo grep -c "^kind: Http" "$MOCKS" || true)
    else
      HTTP_MOCKS=0
    fi
    echo "Http mock records (must be 0): $HTTP_MOCKS"
    if [[ "$HTTP_MOCKS" -gt 0 ]]; then
      echo "::error::found $HTTP_MOCKS 'kind: Http' mocks in verify-upstream-opportunistic mode — parser dispatch should be bypassed"
      exit 1
    fi
    # Belt to the braces of the round-trips above: if the opportunistic
    # upstream handshake had failed, the hijack would have torn the
    # connection down and no application bytes would have reached the
    # upstream at all — so the decrypted-pcap assertion earlier (which
    # greps the ci-${MODE_NAME} query string out of the TLS keylog-decrypted
    # capture) would have found nothing to decrypt.
    echo "good! opportunistic hijack completed against a DNS-SAN-only upstream with verification on"
    ;;
esac
endsec

section "Tear down DB containers + local upstream"
docker compose -f .ci/compose.yml down -v --remove-orphans >/dev/null 2>&1 || true
[[ -n "${UPSTREAM_PID:-}" ]] && kill "$UPSTREAM_PID" 2>/dev/null || true
endsec

echo "All assertions passed (mode=${MODE_NAME}): pcap streamed, keylog populated, tshark decrypted HTTP, MySQL+Postgres TLS round-trips succeeded through keploy."
exit 0
