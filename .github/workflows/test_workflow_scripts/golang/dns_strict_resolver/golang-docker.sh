#!/usr/bin/env bash

# E2E regression test for the RFC 5452 strict-source-validation DNS path.
#
# Covers the cgroup/recvmsg{4,6} SNAT fix (keploy/keploy#4093 /
# keploy/ebpf#97 / issue keploy/keploy#4092). Runs in docker mode — the
# same topology as the Flipkart production setup where the original bug
# was observed (sample container → CoreDNS container over a bridge
# network). Unlike bare-Linux loopback, cgroup/recvmsg4 reliably fires
# for the sample's unconnected-UDP reads in this topology, which is why
# we test here and not in golang_linux.yml.
#
# Assertions: every /resolve call must return source_mismatches == 0
# with a non-empty ips array. source_mismatches > 0 is the exact
# pre-fix symptom (reply source not SNAT-ed back to the advertised
# nameserver, strict client rejects it, retransmits until timeout).

set -Eeuo pipefail

NETWORK=dns-strict-resolver-net
SUBNET=172.30.0.0/16
COREDNS_IP=172.30.0.10
COREDNS_SECONDARY_IP=172.30.0.11
COREDNS_NAME=dns-strict-resolver-coredns
COREDNS_SECONDARY_NAME=dns-strict-resolver-coredns-secondary
SAMPLE_NAME=dns-strict-resolver
CURL_OUT=curl-output.txt

section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

cleanup() {
  docker rm -f "$SAMPLE_NAME" "$COREDNS_NAME" "$COREDNS_SECONDARY_NAME" 2>/dev/null || true
  docker network rm "$NETWORK" 2>/dev/null || true
}
trap cleanup EXIT

check_for_errors() {
  local logfile=$1
  if [ -f "$logfile" ] && grep -q "WARNING: DATA RACE" "$logfile"; then
    echo "::error::Race condition detected in $logfile"
    return 1
  fi
}

dump_diagnostics() {
  echo "::group::keploy record.txt (tail 200)"
  tail -200 record.txt 2>/dev/null || echo "(record.txt missing)"
  echo "::endgroup::"
  echo "::group::CoreDNS primary logs"
  docker logs "$COREDNS_NAME" 2>&1 | tail -40 || true
  echo "::endgroup::"
  echo "::group::CoreDNS secondary logs"
  docker logs "$COREDNS_SECONDARY_NAME" 2>&1 | tail -40 || true
  echo "::endgroup::"
  echo "::group::sample container logs"
  docker logs "$SAMPLE_NAME" 2>&1 | tail -40 || true
  echo "::endgroup::"
  echo "::group::docker ps -a"
  docker ps -a || true
  echo "::endgroup::"
  echo "::group::bpf_trace_pipe (filtered)"
  if [ -s bpf-trace.txt ]; then
    grep -E 'recvmsg4|sendmsg4-dns' bpf-trace.txt | tail -200 || true
    echo "--- tail ---"
    tail -100 bpf-trace.txt || true
  else
    echo "(bpf-trace.txt missing or empty)"
  fi
  echo "::endgroup::"
}

check_curl_output() {
  if [ ! -s "$CURL_OUT" ]; then
    echo "::error::$CURL_OUT is empty — curl.sh produced no output"
    dump_diagnostics
    return 1
  fi
  if grep -Eq '"source_mismatches":[1-9]' "$CURL_OUT"; then
    echo "::error::source_mismatches > 0 (pre-fix Keploy behaviour). Output:"
    cat "$CURL_OUT"
    dump_diagnostics
    return 1
  fi
  if grep -Eq '"txid_mismatches":[1-9]' "$CURL_OUT"; then
    echo "::error::txid_mismatches > 0. Output:"
    cat "$CURL_OUT"
    dump_diagnostics
    return 1
  fi
  if grep -q '"passed":false' "$CURL_OUT"; then
    echo "::error::DNS regression suite failed. Output:"
    cat "$CURL_OUT"
    dump_diagnostics
    return 1
  fi
  if ! grep -q '"passed":true' "$CURL_OUT"; then
    echo "::error::DNS regression suite did not report passed=true. Output:"
    cat "$CURL_OUT"
    dump_diagnostics
    return 1
  fi
  if grep -q '"error":"no accepted reply' "$CURL_OUT"; then
    echo "::error::/resolve timed out waiting for a source-matching reply. Output:"
    cat "$CURL_OUT"
    dump_diagnostics
    return 1
  fi
  if ! grep -q '"ips":\["' "$CURL_OUT"; then
    echo "::error::No /resolve call returned any IPs. Output:"
    cat "$CURL_OUT"
    dump_diagnostics
    return 1
  fi
  echo "curl output looks clean."
}

check_test_report() {
  if [ ! -d "./keploy/reports" ]; then
    echo "::error::Test report directory not found"
    return 1
  fi
  local latest_report_dir
  latest_report_dir=$(ls -td ./keploy/reports/test-run-* 2>/dev/null | head -n 1 || true)
  if [ -z "$latest_report_dir" ]; then
    echo "::error::No test run directory found in ./keploy/reports/"
    return 1
  fi
  local all_passed=true
  for report_file in "$latest_report_dir"/test-set-*-report.yaml; do
    [ -e "$report_file" ] || { echo "No report files found."; all_passed=false; break; }
    local test_set_name test_status
    test_set_name=$(basename "$report_file" -report.yaml)
    test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
    echo "Status for ${test_set_name}: $test_status"
    if [ "$test_status" != "PASSED" ]; then
      all_passed=false
      echo "::error::Test set ${test_set_name} failed with status: ${test_status}"
    fi
  done
  [ "$all_passed" = true ] || return 1
  echo "All tests passed in reports."
}

wait_for_sample() {
  echo "Waiting for $SAMPLE_NAME /health to respond..."
  for i in {1..60}; do
    if curl -sf "http://localhost:8086/health" >/dev/null; then
      echo "sample healthy after ${i}s"; return 0
    fi
    sleep 1
  done
  echo "::error::$SAMPLE_NAME never became healthy"
  echo "::group::docker ps"
  docker ps -a || true
  echo "::endgroup::"
  dump_diagnostics
  return 1
}

# Keploy spawns a "keploy-v3-<rand>" sidecar container whose own
# /etc/resolv.conf comes from the Docker daemon (127.0.0.11). The
# daemon's embedded resolver doesn't know the fixture domains that
# only live in the CoreDNS zone file, so Keploy's DNS forwarder ends
# up returning NXDOMAIN for alpha/beta/gamma.keploy.test. Point its
# resolv.conf at the fixture CoreDNS directly so the forwarder walks
# through to CoreDNS and gets the real A records. Runs only while
# Keploy is up; the container is thrown away at the end of the job
# anyway, so we're not leaving stale state behind.
point_keploy_at_fixture_dns() {
  local target_ip="${1:-$COREDNS_IP}"
  local kp=""
  for i in {1..30}; do
    kp=$(docker ps --format '{{.Names}}' | grep -m1 '^keploy-v3-' || true)
    if [ -n "$kp" ]; then
      break
    fi
    sleep 1
  done
  if [ -z "$kp" ]; then
    echo "::warning::keploy-v3-* sidecar never appeared; skipping resolv.conf override"
    return 0
  fi
  echo "Pointing $kp resolv.conf at fixture CoreDNS $target_ip"
  if ! docker exec "$kp" sh -c "printf 'nameserver %s\n' '$target_ip' > /etc/resolv.conf" 2>/dev/null; then
    echo "::warning::failed to rewrite /etc/resolv.conf inside $kp"
    return 0
  fi
  docker exec "$kp" cat /etc/resolv.conf 2>/dev/null || true
}

send_request() {
  section "Sending Requests"
  if ! wait_for_sample; then
    endsec
    exit 1
  fi
  echo "Running curl.sh..."
  chmod +x ./curl.sh
  # Aim DNS queries at fixture CoreDNS containers rather than
  # /etc/resolv.conf. /suite uses fixture-only keploy.test domains, and
  # SECONDARY_NAMESERVER lets it exercise one unconnected UDP socket
  # sending to more than one upstream.
  NAMESERVER="${COREDNS_IP}:53" SECONDARY_NAMESERVER="${COREDNS_SECONDARY_IP}:53" ./curl.sh 2>&1 | tee "$CURL_OUT" || true
  endsec
}

# --- Main ---

rm -rf keploy/ record.txt test.txt "$CURL_OUT"
sudo rm -f /tmp/keploy-logs.txt
cleanup

section "Build sample image"
docker build -t "$SAMPLE_NAME:test" .
endsec

section "Network + CoreDNS"
# Dedicated network with a known subnet so the CoreDNS fixtures can pin
# stable IPs. The sample queries these IPs directly via ?nameserver= on
# /suite and /resolve; this mirrors the production shape (client → real
# DNS container on a real bridge network) where the recvmsg4 SNAT fix has
# been verified, rather than Docker's embedded DNS path (127.0.0.11).
docker network create --subnet "$SUBNET" "$NETWORK"
docker run -d --rm --name "$COREDNS_NAME" --net "$NETWORK" --ip "$COREDNS_IP" \
  -v "$PWD/coredns:/etc/coredns:ro" \
  coredns/coredns:1.11.3 -conf /etc/coredns/Corefile
docker run -d --rm --name "$COREDNS_SECONDARY_NAME" --net "$NETWORK" --ip "$COREDNS_SECONDARY_IP" \
  -v "$PWD/coredns-secondary:/etc/coredns:ro" \
  coredns/coredns:1.11.3 -conf /etc/coredns/Corefile
sleep 2
endsec

section "Start Recording"
# Docker mode: -c "docker run ..." + --container-name lets keploy detect
# the sample's cgroup and attach the eBPF programs there (unlike
# golang_linux.yml where non-docker loopback UDP doesn't reach
# cgroup/recvmsg4).
# Start a background tail of the kernel's bpf trace ring so any
# bpf_printk from our hooks (the diagnostic build in
# keploy/ebpf@97b4d2b) lands in bpf-trace.txt, which
# dump_diagnostics() tails on failure.
sudo sh -c 'echo > /sys/kernel/debug/tracing/trace || true' 2>/dev/null || true
sudo cat /sys/kernel/debug/tracing/trace_pipe >bpf-trace.txt 2>/dev/null &
TRACE_PID=$!
echo "bpf trace_pipe tail pid=$TRACE_PID"

"$RECORD_BIN" record\
  -c "docker run -p 8086:8086 --rm --net $NETWORK --name $SAMPLE_NAME $SAMPLE_NAME:test" \
  --container-name "$SAMPLE_NAME" \
  --generateGithubActions=false \
  >record.txt 2>&1 &
KEPLOY_PID=$!
echo "Keploy record started (pid=$KEPLOY_PID)"
endsec

point_keploy_at_fixture_dns

send_request

section "Verify Record Mode"
check_curl_output
endsec

section "Stop Recording"
REC_PID="$(pgrep -n -f 'keploy record' || true)"
echo "Killing keploy record (pid=$REC_PID)"
sudo kill -INT "$REC_PID" 2>/dev/null || true
sleep 5
check_for_errors record.txt
# Kill the background bpf trace tail so we stop accumulating output.
if [ -n "${TRACE_PID:-}" ]; then
  sudo kill "$TRACE_PID" 2>/dev/null || true
fi
docker rm -f "$SAMPLE_NAME" 2>/dev/null || true
echo "Recording stopped."
endsec

# Always surface a filtered bpf-trace tail on the happy path too so we
# can see even a passing run's [recvmsg4]/[sendmsg4-dns] events and
# confirm the hook is firing as expected.
echo "::group::bpf_trace_pipe (recvmsg4/sendmsg4, all lines)"
if [ -s bpf-trace.txt ]; then
  grep -E 'recvmsg4|sendmsg4-dns' bpf-trace.txt | tail -200 || true
else
  echo "(bpf-trace.txt empty — is /sys/kernel/debug/tracing readable?)"
fi
echo "::endgroup::"

section "Start Replay"
"$REPLAY_BIN" test \
  -c "docker run -p 8086:8086 --rm --net $NETWORK --name $SAMPLE_NAME $SAMPLE_NAME:test" \
  --container-name "$SAMPLE_NAME" \
  --delay 15 \
  --generateGithubActions=false 2>&1 | tee test.txt || true
# Replay mode serves recorded mocks, so Keploy's DNS forwarder is
# typically not hit. No resolv.conf override needed here.
check_for_errors test.txt
check_test_report
endsec
