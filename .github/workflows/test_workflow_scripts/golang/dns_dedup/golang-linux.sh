#!/usr/bin/env bash

# E2E test for DNS mock deduplication.
#
# Validates that Keploy properly deduplicates DNS mocks when a domain
# returns different IPs on each lookup (e.g., AWS SQS round-robin).
# Without dedup, 30+ lookups to the same domain would create 30+ DNS mocks
# instead of a single deduplicated one.

set -Eeuxo pipefail

# --- Helpers ---
section() { echo "::group::$*"; }
endsec()  { echo "::endgroup::"; }

check_for_errors() {
  local logfile=$1
  echo "Checking for errors in $logfile..."
  if [ -f "$logfile" ]; then
    if grep -q "WARNING: DATA RACE" "$logfile"; then
      echo "::error::Race condition detected in $logfile"
      return 1
    fi
  fi
}

check_test_report() {
    echo "Checking test reports..."
    if [ ! -d "./keploy/reports" ]; then
        echo "Test report directory not found!"
        return 1
    fi

    local latest_report_dir
    latest_report_dir=$(ls -td ./keploy/reports/test-run-* | head -n 1)
    if [ -z "$latest_report_dir" ]; then
        echo "No test run directory found in ./keploy/reports/"
        return 1
    fi

    local all_passed=true
    for report_file in "$latest_report_dir"/test-set-*-report.yaml; do
        [ -e "$report_file" ] || { echo "No report files found."; all_passed=false; break; }

        local test_set_name
        test_set_name=$(basename "$report_file" -report.yaml)
        local test_status
        test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')

        echo "Status for ${test_set_name}: $test_status"
        if [ "$test_status" != "PASSED" ]; then
            all_passed=false
            echo "::error::Test set ${test_set_name} failed with status: ${test_status}"
        fi
    done

    if [ "$all_passed" = false ]; then
        echo "One or more test sets failed."
        return 1
    fi

    echo "All tests passed in reports."
    return 0
}

check_dns_dedup() {
    echo "Checking DNS mock deduplication..."
    local mocks_dir="./keploy/test-set-0"
    if [ ! -f "$mocks_dir/mocks.yaml" ]; then
        echo "::warning::No mocks.yaml found — cannot verify DNS dedup"
        return 0
    fi

    # Count DNS mock entries. With dedup, a domain resolved 30+ times
    # should produce only a handful of mocks, not 30+.
    local dns_mock_count
    dns_mock_count=$(grep -c 'kind: DNS' "$mocks_dir/mocks.yaml" 2>/dev/null || echo "0")
    echo "DNS mock count: $dns_mock_count"

    # We send ~40 DNS lookups total (30 for default domain + 10 for google.com).
    # With proper dedup, we expect far fewer mocks (typically 2-5).
    if [ "$dns_mock_count" -gt 15 ]; then
        echo "::error::DNS dedup may be broken — found $dns_mock_count DNS mocks (expected < 15 for ~40 lookups)"
        return 1
    fi

    echo "DNS dedup check passed ($dns_mock_count mocks for ~40 lookups)"
    return 0
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
  ./curl.sh || true
  endsec
}

# --- Main ---

rm -rf keploy/ record.txt test.txt
sudo rm -f /tmp/keploy-logs.txt

section "Build App"
echo "Building app..."
go mod tidy
go build -o dns-dedup
endsec

# Record
section "Start Recording"
echo "Starting Recording..."
sudo -E env PATH=$PATH "$RECORD_BIN" record -c "./dns-dedup" --generateGithubActions=false 2>&1 | tee record.txt &
KEPLOY_PID=$!
sleep 5
endsec

send_request

section "Stop Recording"
echo "Stopping Keploy record process (PID: $KEPLOY_PID)..."
REC_PID="$(pgrep -n -f 'keploy record' || true)"
echo "$REC_PID Keploy PID"
echo "Killing keploy"
sudo kill -INT "$REC_PID" 2>/dev/null || true
sleep 5
check_for_errors "record.txt"
echo "Recording stopped."
endsec

# Verify DNS dedup
section "Verify DNS Dedup"
check_dns_dedup
endsec

# Replay
section "Start Replay"
echo "Starting Replay..."
sudo -E env PATH=$PATH "$REPLAY_BIN" test -c "./dns-dedup" --delay 10 --generateGithubActions=false 2>&1 | tee test.txt || true
check_for_errors "test.txt"
check_test_report
endsec
