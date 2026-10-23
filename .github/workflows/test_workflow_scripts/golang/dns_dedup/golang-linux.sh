#!/usr/bin/env bash
source "$(dirname "${BASH_SOURCE[0]}")/../../go-retry.sh"

# E2E test for DNS mock deduplication.
#
# Validates that Keploy properly deduplicates DNS mocks when a domain
# returns different IPs on each lookup (e.g., AWS SQS round-robin).
# Without dedup, 30+ lookups to the same domain would create 30+ DNS mocks
# instead of a single deduplicated one.

set -Eeuo pipefail

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
    latest_report_dir=$(ls -td ./keploy/reports/test-run-* 2>/dev/null | head -n 1 || true)
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
        echo "::error::No mocks.yaml found — DNS deduplication cannot be verified. Ensure recording produced mocks."
        return 1
    fi

    # Count DNS mock entries. With dedup, a domain resolved 30+ times
    # should produce only a handful of mocks, not 30+.
    local dns_mock_count
    dns_mock_count=$(grep -c 'kind: DNS' "$mocks_dir/mocks.yaml" 2>/dev/null || true)
    : "${dns_mock_count:=0}"
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

# keploy record passes the resolver's answer to a DNS lookup on to the app
# unchanged, and keeps no mock of an answer that did not resolve it: its log
# line is the user's only trace of why the app could not connect. It says so
# once a recording for each (name, qtype, answer), at INFO, and says nothing
# at INFO of a lookup that resolved, whatever search-list walk the runner's
# resolver made of it. A native agent relays the DNS of every process on the
# host, so only the lines about the names this lane's app looks up (and the
# runner's search-list candidates of them) are the lane's to judge; any other
# is printed as a warning.
UNRESOLVED_MSG="did not resolve the app's lookup"

# unresolved_info_lines LOG: the fields of each INFO line LOG has of a lookup
# that did not resolve, one JSON object per line.
unresolved_info_lines() {
  { grep -F "$UNRESOLVED_MSG" "$1" || true; } | { grep -F "INFO" || true; } | sed 's/^[^{]*{/{/' | { grep '^{' || true; }
}

# lines_about LINES NAME...: the lines whose query is one of the NAMEs (each
# with its trailing dot) or a search-list candidate of one (NAME.<domain>.).
lines_about() {
  local lines=$1
  shift
  printf '%s\n' "$lines" | jq -c --args 'select((.query | ascii_downcase) as $q | any($ARGS.positional[]; . as $n | $q | startswith($n)))' "$@"
}

# warn_other_lines LINES NAME...: print, as a warning, the lines about other
# processes' lookups.
warn_other_lines() {
  local lines=$1 other
  shift
  other=$(printf '%s\n' "$lines" | jq -c --args 'select((.query | ascii_downcase) as $q | all($ARGS.positional[]; . as $n | $q | startswith($n) | not))' "$@")
  if [ -n "$other" ]; then
    echo "::warning::INFO lines of lookups this lane's app does not make (another process on the runner): $other"
  fi
}

# check_no_unresolved_line LOG NAME...: a recording whose app looked up only
# NAMEs, which all resolve.
check_no_unresolved_line() {
  local log=$1 lines ours
  shift
  lines=$(unresolved_info_lines "$log")
  if ! ours=$(lines_about "$lines" "$@"); then
    echo "::error::could not read the INFO lines of unresolved lookups in $log: $lines"
    return 1
  fi
  if [ -n "$ours" ]; then
    echo "::error::$log says, at INFO, that lookups which resolved did not: $ours"
    return 1
  fi
  warn_other_lines "$lines" "$@"
  echo "No INFO line of an unresolved lookup of $* in $log"
}

# expected_no_such_name_step NAME: the next step keploy gives a name no
# resolver has, by how the runner's resolver walks it, from the resolv.conf
# keploy reads too: asked as given before its search domains (its dots reach
# ndots), the line says so once keploy saw it fail in each of them; asked
# after them, or with none, it is said plainly.
expected_no_such_name_step() {
  local name=$1 ndots search dots step
  ndots=$(awk '$1 == "options" { for (i = 2; i <= NF; i++) if ($i ~ /^ndots:/) { v = $i; sub(/^ndots:/, "", v); n = v } } END { print (n == "" ? 1 : n) }' /etc/resolv.conf)
  search=$(awk '$1 == "search" || $1 == "domain" { s = ""; for (i = 2; i <= NF; i++) if ($i != ".") s = s " " $i } END { print s }' /etc/resolv.conf)
  dots=$(printf '%s' "$name" | tr -cd . | wc -c)
  step="the resolver says no such name exists: check the host name the app is configured with"
  if [ -n "${search// /}" ] && [ "$dots" -ge "$ndots" ]; then
    step="$step; the resolver then asked for it in each of its search domains, and none of them resolved it"
  fi
  printf '%s' "$step"
}

# check_unresolved_line_said LOG NAME RESOLVED: the app looked NAME up three
# times, and RESOLVED, which resolves, once: one INFO line for NAME's A lookup
# and at most one for each other qtype, saying the resolver has no such name,
# which resolver answered, and the next step the runner's resolver walk
# makes true; none for RESOLVED or for any search-list candidate of either.
check_unresolved_line_said() {
  local log=$1 name=$2 resolved=$3 lines own others count repeated step wrong
  lines=$(unresolved_info_lines "$log")
  echo "INFO lines of unresolved lookups in $log:"
  printf '%s\n' "$lines"
  echo "The runner's /etc/resolv.conf:"
  cat /etc/resolv.conf
  if ! own=$(printf '%s\n' "$lines" | jq -c --arg n "$name." 'select((.query | ascii_downcase) == $n)') ||
    ! others=$(lines_about "$lines" "$name." "$resolved." | jq -c --arg n "$name." 'select((.query | ascii_downcase) != $n)'); then
    echo "::error::could not read the INFO lines of unresolved lookups in $log"
    return 1
  fi
  if [ -n "$others" ]; then
    echo "::error::$log says, at INFO, that a lookup which resolved, or a search-list candidate, did not: $others"
    return 1
  fi
  warn_other_lines "$lines" "$name." "$resolved."
  count=$(printf '%s\n' "$own" | jq -s '[.[] | select(.qtype == "A")] | length')
  if [ "$count" != 1 ]; then
    echo "::error::$log has $count INFO lines for the A lookups of $name, which never resolved; want 1"
    return 1
  fi
  repeated=$(printf '%s\n' "$own" | jq -s -r 'group_by(.qtype) | map(select(length > 1) | .[0].qtype) | .[]')
  if [ -n "$repeated" ]; then
    echo "::error::$log says the $repeated lookups of $name more than once; want once a recording"
    return 1
  fi
  step=$(expected_no_such_name_step "$name")
  wrong=$(printf '%s\n' "$own" | jq -c --arg s "$step" 'select(.answer != "NXDOMAIN" or ((.upstream // "") == "") or .next_step != $s)')
  if [ -n "$wrong" ]; then
    echo "::error::$log says of $name: $wrong; want answer NXDOMAIN, the resolver that gave it, and the next step \"$step\""
    return 1
  fi
  echo "The unresolved lookups of $name are said once per qtype, with the next step \"$step\", and nothing else the app looked up is"
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
go_retry mod tidy
go_retry build -o dns-dedup
endsec

# Generate keploy config with noise for DNS-dependent fields.
# The resolve-many response contains per-iteration "new" flags and "unique_ip_sets"
# counts that depend on which IPs the DNS returns — these differ between
# live recording and mocked replay.
section "Generate Config"
# Keploy's config now carries only the settings that DIFFER from its
# defaults, so patching a default value out of the generated file with
# `sed` silently patched nothing: the noise rule vanished and every
# replay diffed on the fields it was meant to mask. Write what this
# test needs instead of editing what the generator happened to print.
cat > ./keploy.yml <<'KEPLOY_CFG'
test:
    globalNoise:
        global: {"body": {"unique_ip_sets":[],"results":[]}}
KEPLOY_CFG
endsec

# Record
section "Start Recording"
echo "Starting Recording..."
sudo -E env PATH=$PATH "$RECORD_BIN" record -c "./dns-dedup" --generateGithubActions=false >record.txt 2>&1 &
KEPLOY_PID=$!
echo "Keploy record started with PID: $KEPLOY_PID"
sleep 5
endsec

send_request

section "Stop Recording"
echo "Stopping Keploy record process (PID: $KEPLOY_PID)..."
sudo kill -INT "$KEPLOY_PID" 2>/dev/null || true
wait "$KEPLOY_PID" 2>/dev/null || true
check_for_errors "record.txt"
echo "Recording stopped."
endsec

# Verify DNS dedup
section "Verify DNS Dedup"
check_dns_dedup
check_no_unresolved_line "record.txt" "sqs.us-east-1.amazonaws.com." "google.com."
endsec

# A recording of its own (its own keploy directory, removed after, so the
# replay below sees only the recordings above): the app looks up a name no
# resolver has (RFC 6761's .invalid) three times, then one that resolves.
# Only this branch's build has the line: the released keploy records nothing
# of it.
unresolved_recording() {
  local name=$1 resolved=$2 unresolved_dir ready=false i
  unresolved_dir=$(mktemp -d)
  sudo -E env PATH=$PATH "$RECORD_BIN" record -c "./dns-dedup" --path "$unresolved_dir" --generateGithubActions=false >record_unresolved.txt 2>&1 &
  KEPLOY_PID=$!
  echo "Keploy record (unresolved lookup) started with PID: $KEPLOY_PID"
  # unresolved_abort MESSAGE: stop the recording, show its log, remove its
  # directory.
  unresolved_abort() {
    echo "::error::$*"
    sudo kill -INT "$KEPLOY_PID" 2>/dev/null || true
    wait "$KEPLOY_PID" 2>/dev/null || true
    cat record_unresolved.txt || true
    sudo rm -rf "$unresolved_dir"
  }
  for i in {1..60}; do
    if curl -s http://localhost:8086/health >/dev/null; then
      ready=true
      break
    fi
    sleep 1
  done
  [ "$ready" = true ] || { unresolved_abort "the app under keploy record did not answer /health within 60s"; return 1; }
  for i in 1 2 3; do
    # The app answers 500 with the lookup's error: that is the point.
    curl -sS "http://localhost:8086/resolve?domain=$name" || true
    echo
  done
  curl -sS --fail "http://localhost:8086/resolve?domain=$resolved" || { unresolved_abort "$resolved did not resolve through keploy's DNS"; return 1; }
  echo
  sudo kill -INT "$KEPLOY_PID" 2>/dev/null || true
  wait "$KEPLOY_PID" 2>/dev/null || true
  check_for_errors "record_unresolved.txt" || { cat record_unresolved.txt; sudo rm -rf "$unresolved_dir"; return 1; }
  if ! check_unresolved_line_said "record_unresolved.txt" "$name" "$resolved"; then
    cat record_unresolved.txt
    sudo rm -rf "$unresolved_dir"
    return 1
  fi
  sudo rm -rf "$unresolved_dir"
}

section "Unresolved Lookup Is Said Once"
case "${RECORD_BIN:-}" in
  */build/keploy|*/build-no-race/keploy)
    unresolved_recording "no-such-host.example.invalid" "google.com" || exit 1
    ;;
  *)
    echo "Skipping: the record binary is a released keploy, which predates this line ($RECORD_BIN)"
    ;;
esac
endsec

# shellcheck disable=SC1091
source "${GITHUB_WORKSPACE:-${PWD%/samples-*}}/.github/workflows/test_workflow_scripts/json-pass-helpers.sh"

if json_pass_supported; then
    section "Start Recording (json)"
    echo "Starting json-format recording..."
    sudo -E env PATH=$PATH "$RECORD_BIN" record --storage-format json -c "./dns-dedup" --generateGithubActions=false >record_json.txt 2>&1 &
    KEPLOY_PID=$!
    echo "Keploy record (json) started with PID: $KEPLOY_PID"
    sleep 5
    endsec

    send_request

    section "Stop Recording (json)"
    echo "Stopping Keploy record (json) process (PID: $KEPLOY_PID)..."
    sudo kill -INT "$KEPLOY_PID" 2>/dev/null || true
    wait "$KEPLOY_PID" 2>/dev/null || true
    check_for_errors "record_json.txt"
    echo "Recording (json) stopped."
    endsec
fi

# Replay
section "Start Replay"
echo "Starting Replay..."
sudo -E env PATH=$PATH "$REPLAY_BIN" test -c "./dns-dedup" --delay 10 --generateGithubActions=false 2>&1 | tee test.txt || true
check_for_errors "test.txt"
check_test_report
endsec

if json_pass_supported; then
    # Second replay pass with --storage-format json — exercises the json
    # write path AND the cross-format read path (yaml + json fixtures
    # coexist in keploy/ now).
    section "Start Replay (json)"
    echo "Starting json-format Replay..."
    sudo -E env PATH=$PATH "$REPLAY_BIN" test --storage-format json -c "./dns-dedup" --delay 10 --generateGithubActions=false 2>&1 | tee test_json.txt || true
    check_for_errors "test_json.txt"
    if ! json_scan_reports; then
        cat test_json.txt
        exit 1
    fi
    endsec
fi
