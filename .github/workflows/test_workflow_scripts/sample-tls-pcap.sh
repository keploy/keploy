#!/usr/bin/env bash
# E2E validation for keploy --capture-packets.
#
# Runs the sample-tls-app under `keploy record --capture-packets`,
# drives two real HTTPS calls (api.github.com /zen, httpbin.org
# /anything?msg=...), stops keploy, and asserts that:
#
#   1. <test-set>/traffic.pcap and <test-set>/sslkeys.log were
#      streamed to disk while the recording was live.
#   2. The pcap is a well-formed pcap file (capinfos accepts it).
#   3. The keylog has at least one TLS-1.3 session block
#      (CLIENT_HANDSHAKE_TRAFFIC_SECRET / CLIENT_TRAFFIC_SECRET_0).
#   4. tshark can decrypt the pcap using the keylog and recover the
#      cleartext HTTP requests for both upstream hosts.
#   5. The HTTP parser dispatch path fired (mocks.yaml contains
#      `kind: Http` records, not just `kind: DNS` / generic blobs) —
#      catches regressions where the parser stops being chosen and
#      every TLS session falls through to generic passthrough.
#
# Run from the sample-tls-app working directory. RECORD_BIN must
# point at a keploy build with the postgres parsers linked (i.e.
# produced by the prepare_and_run.yml build job which calls
# setup-private-parsers before `go build`). The HTTP parser is part
# of OSS keploy directly so it is always available regardless.

set -Eeuo pipefail

section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

dump_state() {
  rc=$?
  echo "::error::e2e failed (exit=$rc). Dumping context for triage…"
  echo "== keploy log (last 200 lines) =="
  [[ -f keploy-record.log ]] && tail -200 keploy-record.log || true
  echo "== test-set listing =="
  find keploy -maxdepth 4 -type f -print 2>/dev/null | sort || true
  echo "== keylog (head) =="
  [[ -f keploy/test-set-0/sslkeys.log ]] && sudo head -20 keploy/test-set-0/sslkeys.log || true
  echo "== capinfos =="
  [[ -f keploy/test-set-0/traffic.pcap ]] && sudo capinfos -c -i keploy/test-set-0/traffic.pcap 2>/dev/null || true
  exit "$rc"
}
trap dump_state ERR

wait_for_http() {
  local url="$1" tries="${2:-60}"
  for _ in $(seq 1 "$tries"); do
    if curl -fsS -o /dev/null --max-time 1 "$url"; then return 0; fi
    sleep 1
  done
  return 1
}

drive_traffic() {
  section "Drive HTTPS traffic through keploy proxy"
  if ! wait_for_http "http://localhost:8080/" 90; then
    echo "::error::sample-tls-app did not become healthy on :8080"
    return 1
  fi
  curl -fsS http://localhost:8080/quote >/dev/null
  echo "good! /quote returned"
  curl -fsS 'http://localhost:8080/echo?msg=ci-pcap-validate' >/dev/null
  echo "good! /echo returned"
  # Give the streaming endpoint a beat to flush the last frames.
  sleep 2
  endsec
}

# ----- run keploy record -----

section "Start keploy record --capture-packets"
# A prior run leaves a root-owned keploy/ behind. Use sudo so the
# wipe succeeds whether or not anything from a previous local run
# is sitting in the way.
sudo rm -rf keploy
sudo -E env PATH="$PATH" "$RECORD_BIN" record \
  -c "go run ." \
  --capture-packets \
  > keploy-record.log 2>&1 &
KEPLOY_SUDO_PID=$!
endsec

drive_traffic

section "Stop keploy gracefully"
# pkill -INT only matches the actual `keploy record` argv (not
# zsh/bash shells with that string in their command line) because it
# matches against the kernel's stored argv[0]+argv. The agent
# subprocess argv contains "keploy agent ..." so it is not signalled
# directly — its lifecycle is owned by the parent.
sudo pkill -INT -f "keploy record -c go run \." || true
# Wait up to 30 s for the recorder to flush + exit.
for _ in $(seq 1 30); do
  if ! sudo pgrep -f "keploy record -c go run \." >/dev/null; then break; fi
  sleep 1
done
endsec

# ----- assertions -----

PCAP=keploy/test-set-0/traffic.pcap
KEYLOG=keploy/test-set-0/sslkeys.log
MOCKS=keploy/test-set-0/mocks.yaml

section "Assert artifacts exist"
sudo test -s "$PCAP"   || { echo "::error::missing or empty $PCAP";   exit 1; }
sudo test -s "$KEYLOG" || { echo "::error::missing or empty $KEYLOG"; exit 1; }
sudo test -s "$MOCKS"  || { echo "::error::missing or empty $MOCKS";  exit 1; }
sudo ls -la keploy/test-set-0/
endsec

section "Assert pcap is well-formed"
sudo capinfos -c -i "$PCAP"
endsec

section "Assert keylog has TLS-1.3 session secrets"
# Stdlib emits at least CLIENT_HANDSHAKE_TRAFFIC_SECRET +
# CLIENT_TRAFFIC_SECRET_0 (and server pairs) for every TLS-1.3
# session it terminates. With both halves of an MITM'd connection
# being logged, two outbound HTTPS calls produce >=4 sessions.
KEYLOG_LINES=$(sudo wc -l < "$KEYLOG")
echo "sslkeys.log lines: $KEYLOG_LINES"
if [[ "$KEYLOG_LINES" -lt 4 ]]; then
  echo "::error::expected at least 4 keylog lines (>=1 full TLS-1.3 session block); saw $KEYLOG_LINES"
  exit 1
fi
sudo grep -q "^CLIENT_TRAFFIC_SECRET_0 " "$KEYLOG" || {
  echo "::error::keylog missing CLIENT_TRAFFIC_SECRET_0 — TLS-1.3 application traffic secret was not logged"
  exit 1
}
endsec

section "Assert HTTP parser fired (not generic passthrough)"
HTTP_MOCKS=$(sudo grep -c "^kind: Http" "$MOCKS" || true)
echo "Http mock records: $HTTP_MOCKS"
if [[ "$HTTP_MOCKS" -lt 2 ]]; then
  echo "::error::expected >=2 'kind: Http' mocks (one per upstream host); saw $HTTP_MOCKS"
  echo "  → HTTP parser dispatch may have regressed; mocks fell through to generic"
  exit 1
fi
endsec

section "Decrypt pcap with tshark + keylog and verify cleartext"
DECRYPTED_REQUESTS=$(sudo tshark -r "$PCAP" -o "tls.keylog_file:$KEYLOG" \
  -Y "http.request" -T fields -e http.host -e http.request.uri 2>/dev/null || true)
echo "decrypted HTTP requests:"
echo "$DECRYPTED_REQUESTS"

# Both upstream hosts must appear in the decrypted output. If
# either is missing, the keylog did not contain the master secret
# for that session — i.e. one of the KeyLogWriter sites is wired
# to nil or a stale writer, regression would be caught here.
echo "$DECRYPTED_REQUESTS" | grep -q "api.github.com" || {
  echo "::error::tshark did not see decrypted GET to api.github.com — keylog wiring missing for that session"
  exit 1
}
echo "$DECRYPTED_REQUESTS" | grep -q "httpbin.org" || {
  echo "::error::tshark did not see decrypted GET to httpbin.org — keylog wiring missing for that session"
  exit 1
}
echo "$DECRYPTED_REQUESTS" | grep -q "/anything?msg=ci-pcap-validate" || {
  echo "::error::query string from /echo did not survive into the decrypted pcap"
  exit 1
}

DECRYPTED_RESPONSES=$(sudo tshark -r "$PCAP" -o "tls.keylog_file:$KEYLOG" \
  -Y "http.response" -T fields -e http.response.code 2>/dev/null || true)
OK_COUNT=$(echo "$DECRYPTED_RESPONSES" | grep -c "^200$" || true)
echo "decrypted 200 responses: $OK_COUNT"
if [[ "$OK_COUNT" -lt 2 ]]; then
  echo "::error::expected >=2 decrypted 200 responses; saw $OK_COUNT"
  exit 1
fi
endsec

echo "All assertions passed: pcap was streamed, keylog populated, tshark decrypted both halves of the MITM TLS sessions, HTTP parser dispatch fired."
exit 0
