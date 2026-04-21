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

BPF_TRACE_OUT="bpf-trace.txt"

dump_diagnostics() {
  echo "::group::keploy record.txt (tail 300)"
  tail -300 record.txt 2>/dev/null || echo "(record.txt missing)"
  echo "::endgroup::"
  echo "::group::bpf trace_pipe (filtered: sendmsg4/recvmsg4/pid_filter)"
  # The hook we care about emits [sendmsg4-dns] and [recvmsg4]; pid_filter
  # emits [connect]: ... lines for every match/unmatch decision. Show any
  # of those, plus plain "dns"/"udp" for broader context.
  grep -E "recvmsg4|sendmsg4-dns|connect]:|target namespace|Matched|Unmatched" "$BPF_TRACE_OUT" 2>/dev/null | tail -200 || echo "(bpf trace empty)"
  echo "::endgroup::"
}

check_curl_output() {
  echo "Checking curl output for source-address mismatches and errors..."
  if [ ! -s "$CURL_OUT" ]; then
    echo "::error::$CURL_OUT is empty — curl.sh produced no output"
    dump_diagnostics
    return 1
  fi

  # Any non-zero source_mismatches indicates the recvmsg SNAT hook did
  # not run. This is the exact pre-fix failure mode.
  if grep -Eq '"source_mismatches":[1-9]' "$CURL_OUT"; then
    echo "::error::One or more /resolve calls had source_mismatches > 0 (pre-fix Keploy behaviour). Output:"
    cat "$CURL_OUT"
    dump_diagnostics
    return 1
  fi

  # "error" key is only emitted when /resolve returns 502 (strict check exhausted retries).
  if grep -q '"error":"no accepted reply' "$CURL_OUT"; then
    echo "::error::/resolve timed out waiting for a source-matching reply. Output:"
    cat "$CURL_OUT"
    dump_diagnostics
    return 1
  fi

  # Sanity: every /resolve call should have produced at least one IP.
  if ! grep -q '"ips":\["' "$CURL_OUT"; then
    echo "::error::No /resolve call returned any IPs. Output:"
    cat "$CURL_OUT"
    dump_diagnostics
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

rm -rf keploy/ record.txt test.txt "$CURL_OUT" "$BPF_TRACE_OUT"
sudo rm -f /tmp/keploy-logs.txt

section "Build App"
go mod tidy
go build -o dns-strict-resolver
endsec

# Start BPF trace_pipe capture in parallel so we can see bpf_printk output
# from our cgroup/sendmsg4+recvmsg4 hooks (they currently print when they
# store orig_dst and when recvmsg4 fires / SNATs). This is scoped to the
# test window: we kill it right after recording stops.
sudo sh -c 'echo > /sys/kernel/debug/tracing/trace' 2>/dev/null || true
sudo sh -c 'cat /sys/kernel/debug/tracing/trace_pipe' >"$BPF_TRACE_OUT" 2>&1 &
TRACE_PID=$!

# Record
section "Start Recording"
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

# Stop trace_pipe capture.
sudo kill -INT "$TRACE_PID" 2>/dev/null || true
wait "$TRACE_PID" 2>/dev/null || true

echo "Recording stopped."
endsec

# Replay
section "Start Replay"
sudo -E env PATH=$PATH "$REPLAY_BIN" test -c "./dns-strict-resolver" --delay 10 --generateGithubActions=false 2>&1 | tee test.txt || true
check_for_errors "test.txt"
check_test_report
endsec
