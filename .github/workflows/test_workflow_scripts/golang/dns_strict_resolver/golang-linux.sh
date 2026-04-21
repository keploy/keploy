#!/usr/bin/env bash

# E2E test for the RFC 5452 strict-source-validation DNS path.
#
# Exercises the cgroup/recvmsg{4,6} SNAT fix (keploy/keploy#4093 /
# keploy/ebpf#97 / issue keploy/keploy#4092). The dns-strict-resolver
# sample sends DNS queries over an unconnected UDP socket and discards
# any reply whose source does not match the advertised nameserver. Under
# a buggy Keploy build the reply arrives from <agent_ip>:<keploy_dns_port>
# instead of the real nameserver, the sample's source check rejects it,
# and /resolve returns HTTP 502 with "source_mismatches" > 0 — the
# userspace equivalent of the production "Temporary failure in name
# resolution" / EAI_AGAIN symptom. We fail CI in that case.

set -Eeuo pipefail

# --- Helpers ---
section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

check_for_errors() {
  local logfile=$1
  echo "Checking for errors in $logfile..."
  if [ -f "$logfile" ] && grep -q "WARNING: DATA RACE" "$logfile"; then
    echo "::error::Race condition detected in $logfile"
    return 1
  fi
}

# curl.sh responses are appended to this file; we parse it after the run.
CURL_OUT="curl-output.txt"

dump_keploy_log() {
  local label="${1:-record.txt}"
  local path="${2:-record.txt}"
  if [ ! -s "$path" ]; then
    echo "::warning::$label is empty or missing"
    return 0
  fi
  echo "::group::$label (tail, filtered)"
  # Keep output from drowning CI logs: show attach-related lines first,
  # then the last 200 lines for context.
  echo "--- attach/hook/dns lines ---"
  grep -iE "attach|recvmsg|sendmsg|dns|cgroup|bpf|proxy.*info|register.*client|dns_port|EAI_AGAIN" "$path" | tail -80 || true
  echo "--- last 200 lines ---"
  tail -200 "$path" || true
  echo "::endgroup::"
}

check_curl_output() {
  echo "Checking curl output for source-address mismatches and errors..."
  if [ ! -s "$CURL_OUT" ]; then
    echo "::error::$CURL_OUT is empty — curl.sh produced no output"
    dump_keploy_log "record.txt (empty curl output)" record.txt
    return 1
  fi

  # Any non-zero source_mismatches indicates the recvmsg SNAT hook did
  # not run. This is the exact pre-fix failure mode.
  if grep -Eq '"source_mismatches":[1-9]' "$CURL_OUT"; then
    echo "::error::One or more /resolve calls had source_mismatches > 0 (pre-fix Keploy behaviour). Output:"
    cat "$CURL_OUT"
    dump_keploy_log "record.txt (source_mismatches > 0)" record.txt
    return 1
  fi

  # "error" key is only emitted when /resolve returns 502 (strict check exhausted retries).
  if grep -q '"error":"no accepted reply' "$CURL_OUT"; then
    echo "::error::/resolve timed out waiting for a source-matching reply. Output:"
    cat "$CURL_OUT"
    dump_keploy_log "record.txt (no accepted reply)" record.txt
    return 1
  fi

  # Sanity: every /resolve call should have produced at least one IP.
  if ! grep -q '"ips":\["' "$CURL_OUT"; then
    echo "::error::No /resolve call returned any IPs. Output:"
    cat "$CURL_OUT"
    dump_keploy_log "record.txt (no IPs)" record.txt
    return 1
  fi

  echo "curl output looks clean (no source mismatches, no timeouts, IPs present)."
}

check_test_report() {
  echo "Checking test reports..."
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

  if [ "$all_passed" = false ]; then
    return 1
  fi
  echo "All tests passed in reports."
}

send_request() {
  section "Sending Requests"
  echo "Waiting for app to start..."
  for i in {1..30}; do
    if curl -s http://localhost:8086/health >/dev/null; then
      echo "App is healthy"
      break
    fi
    sleep 1
  done

  echo "Running curl.sh..."
  chmod +x ./curl.sh
  # Tee each response so check_curl_output can grep it. curl.sh is allowed
  # to fail (`|| true`) so that HTTP 502s don't abort the run before we
  # have a chance to diagnose them.
  ./curl.sh 2>&1 | tee "$CURL_OUT" || true
  endsec
}

# --- Main ---

rm -rf keploy/ record.txt test.txt "$CURL_OUT"
sudo rm -f /tmp/keploy-logs.txt

section "Build App"
go mod tidy
go build -o dns-strict-resolver
endsec

# Record
section "Start Recording"
# --debug here so attach failures / BPF-hook lifecycle are visible if the
# check_curl_output step later fails.
sudo -E env PATH=$PATH "$RECORD_BIN" record -c "./dns-strict-resolver" --generateGithubActions=false --debug >record.txt 2>&1 &
KEPLOY_PID=$!
echo "Keploy record started with PID: $KEPLOY_PID"
sleep 8
endsec

send_request

section "Verify Record Mode"
check_curl_output
endsec

section "Stop Recording"
REC_PID="$(pgrep -n -f 'keploy record' || true)"
echo "Killing keploy record (PID: $REC_PID)"
sudo kill -INT "$REC_PID" 2>/dev/null || true
sleep 5
check_for_errors "record.txt"
echo "Recording stopped."
endsec

# Replay
section "Start Replay"
sudo -E env PATH=$PATH "$REPLAY_BIN" test -c "./dns-strict-resolver" --delay 10 --generateGithubActions=false 2>&1 | tee test.txt || true
check_for_errors "test.txt"
check_test_report
endsec
