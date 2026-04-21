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

NETWORK=keploy-network
SAMPLE_NAME=dns-strict-resolver
CURL_OUT=curl-output.txt

section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

cleanup() {
  docker rm -f "$SAMPLE_NAME" 2>/dev/null || true
  # Leave keploy-network in place — other jobs may be using it in parallel
  # and `docker network rm` fails if any container is still attached.
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
  echo "::group::sample container logs"
  docker logs "$SAMPLE_NAME" 2>&1 | tail -40 || true
  echo "::endgroup::"
  echo "::group::docker ps -a"
  docker ps -a || true
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

send_request() {
  section "Sending Requests"
  if ! wait_for_sample; then
    endsec
    exit 1
  fi
  echo "Running curl.sh..."
  chmod +x ./curl.sh
  ./curl.sh 2>&1 | tee "$CURL_OUT" || true
  endsec
}

# --- Main ---

rm -rf keploy/ record.txt test.txt "$CURL_OUT"
sudo rm -f /tmp/keploy-logs.txt
cleanup

section "Build sample image"
docker build -t "$SAMPLE_NAME:test" .
endsec

section "Network"
# keploy-network is the conventional network name keploy looks for
# in docker mode (see gin_mongo/golang-docker.sh). Idempotent create —
# a prior matrix job may have left it in place.
docker network inspect "$NETWORK" >/dev/null 2>&1 || docker network create "$NETWORK"
endsec

section "Start Recording"
# Docker mode: -c "docker run ..." + --container-name lets keploy detect
# the sample's cgroup and attach the eBPF programs there (unlike
# golang_linux.yml where non-docker loopback UDP doesn't reach
# cgroup/recvmsg4).
"$RECORD_BIN" record\
  -c "docker run -p 8086:8086 --rm --net $NETWORK --name $SAMPLE_NAME $SAMPLE_NAME:test" \
  --container-name "$SAMPLE_NAME" \
  --generateGithubActions=false \
  >record.txt 2>&1 &
KEPLOY_PID=$!
echo "Keploy record started (pid=$KEPLOY_PID)"
endsec

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
docker rm -f "$SAMPLE_NAME" 2>/dev/null || true
echo "Recording stopped."
endsec

section "Start Replay"
"$REPLAY_BIN" test \
  -c "docker run -p 8086:8086 --rm --net $NETWORK --name $SAMPLE_NAME $SAMPLE_NAME:test" \
  --container-name "$SAMPLE_NAME" \
  --delay 15 \
  --generateGithubActions=false 2>&1 | tee test.txt || true
check_for_errors test.txt
check_test_report
endsec
