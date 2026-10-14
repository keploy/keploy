#!/bin/bash
source "$(dirname "${BASH_SOURCE[0]}")/../../go-retry.sh"

# E2E test for CONNECT tunnel support.
# Verifies that Keploy can record and replay HTTP requests that the app
# sends through an HTTP CONNECT proxy (corporate proxy pattern).
#
# Architecture during record:
#   [curl] → [app:8080] --CONNECT→ [keploy proxy] --CONNECT→ [connect-proxy:3128]
#          --TLS→ [local HTTPS upstream:8443]
# Architecture during replay:
#   [keploy replayer] → [app:8080] --CONNECT→ [keploy proxy] → mock response
#
# Nothing here leaves the runner. The upstream used to be https://httpbin.org,
# so the lane's verdict was httpbin's health, not keploy's: when httpbin reset
# or stalled the handshake, the lane went red on PRs that had nothing to do
# with it, or went green having recorded nothing through the tunnel at all.

set -Eeuo pipefail

echo "RECORD_BIN=$RECORD_BIN"
echo "REPLAY_BIN=$REPLAY_BIN"

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh

# The upstream's name is under .test (RFC 6761), so it can never resolve
# publicly, and it is deliberately not in /etc/hosts or DNS: only the CONNECT
# proxy knows where it lives, the way a corporate proxy reaches hosts its
# clients cannot resolve. The request can therefore only succeed through the
# tunnel. It must not be localhost or an IP literal either: Go's
# ProxyFromEnvironment never proxies those, and the tunnel would be skipped.
UPSTREAM_HOST="upstream.connect-tunnel.test"
UPSTREAM_PORT=8443
TARGET_URL="https://${UPSTREAM_HOST}:${UPSTREAM_PORT}/get"
UPSTREAM_CA_FILE=/tmp/connect-upstream-ca.crt
UPSTREAM_CA_TRUST_PATH=/usr/local/share/ca-certificates/keploy-connect-tunnel-e2e.crt

cleanup() {
    local pid
    for pid in "${PROXY_PID:-}" "${UPSTREAM_PID:-}"; do
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            kill "$pid" 2>/dev/null || true
            wait "$pid" 2>/dev/null || true
        fi
    done
    # Its key died with the upstream, but a trust anchor should not outlive
    # the run that needed it. --fresh, because a plain update-ca-certificates
    # drops the CA from the bundle yet leaves its /etc/ssl/certs links dangling.
    if [ -n "${UPSTREAM_CA_INSTALLED:-}" ]; then
        sudo rm -f "$UPSTREAM_CA_TRUST_PATH" || true
        sudo update-ca-certificates --fresh >/dev/null 2>&1 || true
    fi
}
trap cleanup EXIT

# ── Cleanup ──
if [ -f "./keploy.yml" ]; then
    rm ./keploy.yml
fi
rm -rf keploy/

# ── Start a local HTTPS upstream ──
# Serves https://$UPSTREAM_HOST:$UPSTREAM_PORT/get with a certificate issued by
# a CA it mints at startup. Only the CA certificate is written out, for this
# lane to trust; the CA's private key never leaves the process.
cat > /tmp/connect-upstream.go <<'EOF'
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"time"
)

// connect-upstream <listen-addr> <server-name> <ca-cert-out>
func main() {
	if len(os.Args) != 4 {
		log.Fatal("usage: connect-upstream <listen-addr> <server-name> <ca-cert-out>")
	}
	listenAddr, serverName, caOut := os.Args[1], os.Args[2], os.Args[3]
	notBefore, notAfter := time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour)

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	check(err)
	caTmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: "keploy connect-tunnel e2e CA"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		// Even while the runner trusts it, this CA can vouch for no name
		// but the upstream's.
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         []string{serverName},
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	check(err)
	caCert, err := x509.ParseCertificate(caDER)
	check(err)

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	check(err)
	leafTmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: serverName},
		DNSNames:     []string{serverName},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	check(err)

	mux := http.NewServeMux()
	// Shaped like httpbin's /get: the sample reports the "url" it gets back,
	// which makes this response's arrival visible in the recorded test case.
	mux.HandleFunc("/get", func(w http.ResponseWriter, r *http.Request) {
		log.Printf("upstream served %s %s for Host %s", r.Method, r.URL.Path, r.Host)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"url": "https://" + r.Host + r.URL.Path})
	})
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}},
		},
	}

	ln, err := net.Listen("tcp", listenAddr)
	check(err)
	// Written only once the socket is bound, and renamed into place, so the
	// file appearing is the readiness signal: the lane never has to probe the
	// TLS port with a connection that is not a TLS handshake.
	check(os.WriteFile(caOut+".tmp", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o644))
	check(os.Rename(caOut+".tmp", caOut))
	log.Printf("HTTPS upstream for %s listening on %s", serverName, listenAddr)
	log.Fatal(srv.ServeTLS(ln, "", ""))
}

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	check(err)
	return n
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
EOF

go build -o /tmp/connect-upstream /tmp/connect-upstream.go
rm -f "$UPSTREAM_CA_FILE"
/tmp/connect-upstream "127.0.0.1:${UPSTREAM_PORT}" "$UPSTREAM_HOST" "$UPSTREAM_CA_FILE" &
UPSTREAM_PID=$!
echo "HTTPS upstream started (PID: $UPSTREAM_PID)"

for attempt in {1..20}; do
    if [ -s "$UPSTREAM_CA_FILE" ]; then
        echo "HTTPS upstream is ready on :$UPSTREAM_PORT"
        break
    fi
    if ! kill -0 "$UPSTREAM_PID" 2>/dev/null; then
        echo "::error::HTTPS upstream exited before it became ready"
        exit 1
    fi
    sleep 1
done
if [ ! -s "$UPSTREAM_CA_FILE" ]; then
    echo "::error::HTTPS upstream failed to start on port $UPSTREAM_PORT"
    exit 1
fi

# Trust the upstream's CA in the OS trust store, which is where every verifier
# of the upstream's certificate reads its roots: curl below, the app itself
# (Go's crypto/x509 system roots) whenever it reaches the upstream without
# keploy in between, and keploy's agent, which verifies the upstream against
# the system roots because keploy.yml below turns record.upstreamTls.verify on.
# No -k and no InsecureSkipVerify anywhere: the chain has to be genuinely valid.
sudo install -m 0644 "$UPSTREAM_CA_FILE" "$UPSTREAM_CA_TRUST_PATH"
UPSTREAM_CA_INSTALLED=1
sudo update-ca-certificates >/dev/null

# ── Start a local CONNECT proxy ──
# Avoid apt/tinyproxy in CI. GitHub-hosted apt mirrors can stall, while Go is
# already provisioned for this workflow.
#
# It reaches exactly one upstream: CONNECT to the authority in its first
# argument is dialled at the address in its second, and every other target
# gets 403. An app request aimed anywhere else therefore fails instead of
# quietly depending on a public host, and the recording checks below fail the
# lane. (keploy's own telemetry also honours HTTPS_PROXY and is refused here
# too, which keploy tolerates.)
cat > /tmp/connect-proxy.go <<'EOF'
package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func main() {
	if len(os.Args) != 3 {
		log.Fatal("usage: connect-proxy <connect-authority> <dial-address>")
	}
	allowed, dialAddr := os.Args[1], os.Args[2]
	ln, err := net.Listen("tcp", "127.0.0.1:3128")
	if err != nil {
		log.Fatal(err)
	}
	log.Println("connect proxy listening on 127.0.0.1:3128")
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Println(err)
			continue
		}
		go handle(conn, allowed, dialAddr)
	}
}

func handle(client net.Conn, allowed, dialAddr string) {
	defer client.Close()

	reader := bufio.NewReader(client)
	req, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	if req.Method != http.MethodConnect {
		fmt.Fprint(client, "HTTP/1.1 405 Method Not Allowed\r\n\r\n")
		return
	}

	target := req.Host
	if !strings.Contains(target, ":") {
		target += ":443"
	}
	if target != allowed {
		log.Printf("refusing CONNECT to %s: only %s is reachable through this proxy", target, allowed)
		fmt.Fprint(client, "HTTP/1.1 403 Forbidden\r\n\r\n")
		return
	}

	upstream, err := net.DialTimeout("tcp", dialAddr, 10*time.Second)
	if err != nil {
		fmt.Fprint(client, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
		return
	}
	defer upstream.Close()

	fmt.Fprint(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
	errc := make(chan error, 2)
	go func() {
		_, err := io.Copy(upstream, reader)
		errc <- err
	}()
	go func() {
		_, err := io.Copy(client, upstream)
		errc <- err
	}()
	<-errc
}
EOF

go build -o /tmp/connect-proxy /tmp/connect-proxy.go
/tmp/connect-proxy "${UPSTREAM_HOST}:${UPSTREAM_PORT}" "127.0.0.1:${UPSTREAM_PORT}" &
PROXY_PID=$!
echo "CONNECT proxy started (PID: $PROXY_PID)"

# Verify proxy is listening
for attempt in {1..20}; do
    if (echo > /dev/tcp/127.0.0.1/3128) >/dev/null 2>&1; then
        echo "CONNECT proxy is ready on :3128"
        break
    fi
    if ! kill -0 "$PROXY_PID" 2>/dev/null; then
        echo "::error::CONNECT proxy exited before it became ready"
        exit 1
    fi
    sleep 1
done
if ! (echo > /dev/tcp/127.0.0.1/3128) >/dev/null 2>&1; then
    echo "::error::CONNECT proxy failed to start on port 3128"
    exit 1
fi

# Prove the route and the certificate chain before keploy is involved: the same
# request the app will make, through the proxy, verified against the system
# trust store.
if ! curl -fsS --max-time 10 --proxy http://127.0.0.1:3128 "$TARGET_URL"; then
    echo "::error::$TARGET_URL is not reachable through the CONNECT proxy with a verified certificate chain"
    exit 1
fi
echo ""

# ── Build the app ──
go_retry build -o connect-tunnel
echo "Go binary built."

# ── Generate keploy config with noise rules ──
config_file="./keploy.yml"
  # The app's Date header changes on every response — mark it as noise.
  # Keploy's config now carries only the settings that DIFFER from its
  # defaults, so patching a default value out of the generated file with
  # `sed` silently patched nothing: the noise rule vanished and every
  # replay diffed on the fields it was meant to mask. Write what this
  # test needs instead of editing what the generator happened to print.
  #
  # record.upstreamTls.verify: keploy terminates the app's TLS inside the
  # tunnel and opens its own TLS session to the upstream, so keploy's agent is
  # the only party that ever sees the upstream's certificate. It skips that
  # check by default, which would let this lane pass against a chain nothing
  # had verified. The app (Go's default client) verifies its upstream, so
  # verifying on its behalf is exactly as strict as the app, not stricter.
  cat > "$config_file" <<'KEPLOY_CFG'
record:
  upstreamTls:
    verify: true
test:
  globalNoise:
      global: {"header": {"Date":[], "Content-Length":[]}}
KEPLOY_CFG

stop_recording() {
    local rec_pid
    rec_pid="$(pgrep -n -f "$(basename "${RECORD_BIN:-keploy}") record" || true)"
    if [ -n "$rec_pid" ]; then
        sudo kill -INT "$rec_pid" 2>/dev/null || true
        (
            sleep 15
            if kill -0 "$rec_pid" 2>/dev/null; then
                echo "===== FORCE KILLING KEPLOY RECORD PID $rec_pid ====="
                sudo kill -9 "$rec_pid" 2>/dev/null || true
            fi
        ) &
    fi
}

# ── Helper: send requests to the app ──
send_request() {
    sleep 6
    # Wait for app to be ready
    app_ready=false
    for i in {1..30}; do
        if curl -fsS --max-time 5 http://127.0.0.1:8080/health > /dev/null 2>&1; then
            app_ready=true
            break
        fi
        sleep 1
    done
    if [ "$app_ready" = false ]; then
        echo "::error::App failed to become ready on :8080 after 30s"
        stop_recording
        return 1
    fi

    echo "App is ready, sending requests..."

    # 1. Health check (no external deps)
    curl -fsS --max-time 10 http://127.0.0.1:8080/health || { stop_recording; return 1; }
    echo ""

    # 2. Request via CONNECT tunnel
    if ! curl -sS --max-time 15 http://127.0.0.1:8080/via-proxy; then
        echo "::warning::CONNECT tunnel request failed during record; report validation will decide if this is expected for the selected binary"
    fi
    echo ""

    # Wait for keploy to finish recording
    sleep 7
    stop_recording
    echo "Sent SIGINT to keploy record process"
}

# ── Record phase (2 iterations for dedup testing) ──
for i in 1 2; do
    app_name="connect-tunnel_${i}"
    send_request &
    REQ_PID=$!
    if ! timeout --kill-after=30s 8m env HTTP_PROXY=http://127.0.0.1:3128 HTTPS_PROXY=http://127.0.0.1:3128 TARGET_URL="$TARGET_URL" \
        "$RECORD_BIN" record -c "./connect-tunnel" --generateGithubActions=false 2>&1 | tee "${app_name}.txt"; then
        echo "::error::connect-tunnel recording iteration $i failed or timed out"
        stop_recording
        exit 1
    fi

    if grep "ERROR" "${app_name}.txt" | grep "Keploy" | grep -v "tinyproxy\|WARNING\|CONNECT\|connection refused\|no matching.*mock"; then
        echo "::error::Error found in recording iteration $i"
        cat "${app_name}.txt"
        exit 1
    fi
    if grep -q "WARNING: DATA RACE" "${app_name}.txt"; then
        echo "::error::Race condition detected in recording"
        cat "${app_name}.txt"
        exit 1
    fi
    sleep 5
    if ! wait "$REQ_PID"; then
        echo "::error::Request driver failed in recording iteration $i"
        exit 1
    fi
    echo "Recorded test cases for iteration $i"
done

# shellcheck disable=SC1091
source "${GITHUB_WORKSPACE:-${PWD%/samples-*}}/.github/workflows/test_workflow_scripts/json-pass-helpers.sh"

if json_pass_supported; then
    for i in 1 2; do
        app_name="connect-tunnel_json_${i}"
        send_request &
        REQ_PID=$!
        if ! timeout --kill-after=30s 8m env HTTP_PROXY=http://127.0.0.1:3128 HTTPS_PROXY=http://127.0.0.1:3128 TARGET_URL="$TARGET_URL" \
            "$RECORD_BIN" record --storage-format json -c "./connect-tunnel" --generateGithubActions=false 2>&1 | tee "${app_name}.txt"; then
            echo "::error::connect-tunnel json recording iteration $i failed or timed out"
            stop_recording
            exit 1
        fi

        if grep "ERROR" "${app_name}.txt" | grep "Keploy" | grep -v "tinyproxy\|WARNING\|CONNECT\|connection refused\|no matching.*mock"; then
            echo "::error::Error found in json recording iteration $i"
            cat "${app_name}.txt"
            exit 1
        fi
        if grep -q "WARNING: DATA RACE" "${app_name}.txt"; then
            echo "::error::Race condition detected in json recording"
            cat "${app_name}.txt"
            exit 1
        fi
        sleep 5
        if ! wait "$REQ_PID"; then
            echo "::error::Request driver failed in json recording iteration $i"
            exit 1
        fi
        echo "Recorded json test cases for iteration $i"
    done
fi

echo "Recording complete. Test sets:"
ls -la keploy/*/tests/ 2>/dev/null || echo "No test sets found"
ls -la keploy/*/mocks/ 2>/dev/null || echo "No mocks found"

# ── keploy's agent must have verified the upstream, not skipped the check ──
# record.upstreamTls.verify fails OPEN by design: an agent that cannot load its
# trust anchors logs why and records without verifying, so a green recording
# alone does not show the chain was checked. The agent logs this line once it
# has resolved its trust pool and turned verification on.
for rec_log in connect-tunnel_*.txt; do
    if ! grep -q "upstream TLS certificate verification is enabled" "$rec_log"; then
        echo "::error::$rec_log: keploy's agent never reported upstream TLS verification enabled — record.upstreamTls.verify did not reach it, or loading its trust anchors failed and it fell back to skipping the check"
        grep -n -i "upstream tls" "$rec_log" || echo "(no upstream TLS lines in $rec_log)"
        exit 1
    fi
done

# ── Every test set must hold the tunnelled HTTPS exchange ──
# Without this, an upstream that failed the handshake recorded /via-proxy as a
# 502 with no mock (or, when it stalled, recorded no /via-proxy test at all),
# and the variants that use the released binary still passed on /health
# alone: green without the CONNECT tunnel ever being exercised.
# TARGET_URL is echoed back only in the upstream's own response body, so it
# appears in a mock only when keploy decrypted and captured the exchange
# inside the tunnel, and in a test case only when the app received it.
test_sets_checked=0
for test_set in ./keploy/test-set-*/; do
    [ -d "$test_set" ] || continue
    test_sets_checked=$((test_sets_checked + 1))
    if ! grep -rqsF "$TARGET_URL" "${test_set}tests/"; then
        echo "::error::$(basename "$test_set") has no /via-proxy test case that received the upstream's response"
        ls -la "${test_set}" "${test_set}tests/" 2>/dev/null || true
        exit 1
    fi
    if ! grep -qsF "$TARGET_URL" "${test_set}"mocks.*; then
        echo "::error::$(basename "$test_set") has no mock of the HTTPS exchange inside the CONNECT tunnel"
        ls -la "${test_set}" 2>/dev/null || true
        exit 1
    fi
    echo "$(basename "$test_set"): recorded the tunnelled HTTPS exchange with $TARGET_URL"
done
if [ "$test_sets_checked" -eq 0 ]; then
    echo "::error::No test sets were recorded"
    exit 1
fi

# ── Stop CONNECT proxy and upstream before replay ──
# This ensures replay uses mocks, not the real proxy or upstream.
echo "Stopping CONNECT proxy and HTTPS upstream for replay..."
kill "$PROXY_PID" "$UPSTREAM_PID" 2>/dev/null || true
wait "$PROXY_PID" 2>/dev/null || true
wait "$UPSTREAM_PID" 2>/dev/null || true
sleep 2

# ── Replay phase ──
# Allow non-zero exit from replay (some tests may fail with latest binary).
# We validate results from the report files below.
set +e
timeout --kill-after=30s 8m env HTTP_PROXY=http://127.0.0.1:3128 HTTPS_PROXY=http://127.0.0.1:3128 TARGET_URL="$TARGET_URL" \
    "$REPLAY_BIN" test -c "./connect-tunnel" --delay 7 --generateGithubActions=false 2>&1 | tee test_logs.txt
replay_rc=${PIPESTATUS[0]}
set -e
if [ "$replay_rc" -eq 124 ] || [ "$replay_rc" -eq 137 ]; then
    echo "::error::connect-tunnel replay timed out"
    exit 1
fi

if grep "ERROR" "test_logs.txt" | grep "Keploy" | grep -v "tinyproxy\|WARNING\|CONNECT\|connection refused\|no matching.*mock"; then
    echo "::error::Error found in replay"
    cat "test_logs.txt"
    exit 1
fi

if grep -q "WARNING: DATA RACE" "test_logs.txt"; then
    echo "::error::Race condition detected in replay"
    cat "test_logs.txt"
    exit 1
fi

# ── Determine expected behavior ──
# CONNECT tunnel support only exists in the build from this branch.
# When either record or replay uses the "latest" release binary,
# the /via-proxy test (which depends on CONNECT) is expected to fail.
# Only the /health test (no CONNECT dependency) must always pass.
both_build=false
case "${RECORD_BIN:-}" in
    */build/keploy|*/build-no-race/keploy)
        case "${REPLAY_BIN:-}" in
            */build/keploy|*/build-no-race/keploy)
                both_build=true
                ;;
        esac
        ;;
esac

echo "Both binaries are build (CONNECT-capable): $both_build"

# ── Validate test reports ──
if [ "$both_build" = true ]; then
    # Full validation: all test sets must pass.
    all_passed=true
    for report_file in ./keploy/reports/test-run-0/test-set-*-report.yaml; do
        [ -e "$report_file" ] || { echo "No report files found!"; all_passed=false; break; }

        test_set_name=$(basename "$report_file" -report.yaml)
        test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')

        echo "Status for ${test_set_name}: $test_status"
        if [ "$test_status" != "PASSED" ]; then
            all_passed=false
            echo "::error::${test_set_name} did not pass"
        fi
    done

    if [ "$all_passed" != true ]; then
        echo "::error::Some tests failed. Dumping logs..."
        cat test_logs.txt
        exit 1
    fi

    if json_pass_supported; then
        set +e
        timeout --kill-after=30s 8m env HTTP_PROXY=http://127.0.0.1:3128 HTTPS_PROXY=http://127.0.0.1:3128 TARGET_URL="$TARGET_URL" \
            "$REPLAY_BIN" test --storage-format json -c "./connect-tunnel" --delay 7 --generateGithubActions=false 2>&1 | tee test_logs_json.txt
        replay_rc_json=${PIPESTATUS[0]}
        set -e
        if [ "$replay_rc_json" -eq 124 ] || [ "$replay_rc_json" -eq 137 ]; then
            echo "::error::connect-tunnel json replay timed out"
            exit 1
        fi
        if grep "ERROR" "test_logs_json.txt" | grep "Keploy" | grep -v "tinyproxy\|WARNING\|CONNECT\|connection refused\|no matching.*mock"; then
            echo "::error::Error found in json replay"
            cat "test_logs_json.txt"
            exit 1
        fi
        if grep -q "WARNING: DATA RACE" "test_logs_json.txt"; then
            echo "::error::Race condition detected in json replay"
            cat "test_logs_json.txt"
            exit 1
        fi
        if ! json_scan_reports; then
            cat test_logs_json.txt
            exit 1
        fi
        echo "All CONNECT tunnel tests passed (yaml + json)!"
    else
        echo "All CONNECT tunnel tests passed (yaml only — json pass skipped)"
    fi
    exit 0
else
    # Partial validation: at least /health tests must pass.
    # /via-proxy failures are expected when latest binary lacks CONNECT support.
    echo "Latest binary lacks CONNECT support — validating /health tests only."
    health_passed=false
    if grep -q "test passed" test_logs.txt 2>/dev/null; then
        health_passed=true
    fi
    # Check that the test report exists and has at least 1 pass.
    for report_file in ./keploy/reports/test-run-0/test-set-*-report.yaml; do
        [ -e "$report_file" ] || continue
        pass_count=$(grep -c 'status: PASSED' "$report_file" 2>/dev/null || echo "0")
        if [ "$pass_count" -gt 0 ]; then
            health_passed=true
        fi
    done

    if [ "$health_passed" = true ]; then
        echo "Health tests passed (CONNECT tests expected to fail with latest binary)."
        exit 0
    else
        echo "::error::Even health tests failed — unexpected."
        cat test_logs.txt
        exit 1
    fi
fi
