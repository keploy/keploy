#!/bin/bash

# E2E for the mock-mismatch report. Reuses the http-pokeapi sample (it makes
# mockable outgoing HTTP calls), records it, then MUTATES the recorded mocks so
# the live outgoing request no longer matches any mock on replay. Unlike the
# other suites, success here is INVERTED: we assert that replay SURFACES the
# mismatch (the "MOCK MISMATCH" report with an unmatched outgoing call), not
# that every test passed.

echo "$RECORD_BIN"
echo "$REPLAY_BIN"

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh
echo "iid.sh executed"

git fetch origin

if [ -f "./keploy.yml" ]; then
    rm ./keploy.yml
fi
rm -rf keploy/

build_go_app() {
  local attempt=1
  local max_attempts=4
  local sleep_sec=5
  while [ "$attempt" -le "$max_attempts" ]; do
    if GOPROXY="proxy.golang.org,direct" go build -o http-pokeapi; then
      return 0
    fi
    if [ "$attempt" -ge "$max_attempts" ]; then
      echo "::error::go build for http-pokeapi failed after ${max_attempts} attempts"
      return 1
    fi
    echo "go build attempt ${attempt} failed; retrying in ${sleep_sec}s…"
    sleep "$sleep_sec"
    sleep_sec=$((sleep_sec * 2))
    attempt=$((attempt + 1))
  done
}
build_go_app
echo "go binary built"

sudo "$RECORD_BIN" config --generate
config_file="./keploy.yml"
sed -i 's/global: {}/global: {"body": {"updated_at":[]}}/' "$config_file"

send_request() {
    sleep 6
    app_started=false
    while [ "$app_started" = false ]; do
        if curl -X GET http://localhost:8080/api/locations; then
            app_started=true
        fi
        sleep 3
    done
    echo "App started"
    response=$(curl -s -X GET http://localhost:8080/api/locations)
    location=$(echo "$response" | jq -r ".location[0]")
    curl -s -X GET "http://localhost:8080/api/locations/$location"
    sleep 7
    pid=$(pgrep keploy)
    echo "$pid Keploy PID"
    echo "Killing Keploy"
    sudo kill "$pid"
}

# Record one iteration of test cases + mocks.
send_request &
"$RECORD_BIN" record -c "./http-pokeapi" --generateGithubActions=false 2>&1 | tee record_logs.txt
if grep "WARNING: DATA RACE" record_logs.txt; then
    echo "::error::Race condition detected in recording"
    cat record_logs.txt
    exit 1
fi
wait
echo "Recorded test cases and mocks"

# Force a mock mismatch: rewrite the recorded request PATH on the mock side.
# HTTP schema matching compares the request URL path (not host), so changing
# the recorded "/api/v2/..." path means the live outgoing request to
# "/api/v2/..." no longer matches any recorded mock -> the matcher reports an
# unmatched outgoing call. Only the mocks are touched; the test cases (inbound
# requests) are untouched, so the inbound path still replays normally.
shopt -s nullglob
mock_files=( ./keploy/test-set-*/mocks.yaml )
shopt -u nullglob
if [ ${#mock_files[@]} -eq 0 ]; then
    echo "::error::No recorded mocks found to mutate under ./keploy/test-set-*/"
    cat record_logs.txt
    exit 1
fi
mutated_any=false
for mf in "${mock_files[@]}"; do
    sed -i 's#/api/v2/#/api/v0-mismatch/#g' "$mf"
    if grep -q '/api/v0-mismatch/' "$mf"; then
        echo "mutated recorded request path in: $mf"
        mutated_any=true
    fi
done
if [ "$mutated_any" != true ]; then
    echo "::error::mutation changed no recorded request path — sample/mock layout changed; e2e can't force a mismatch"
    head -50 "${mock_files[0]}"
    exit 1
fi

# Replay. The mutated mocks no longer match → keploy must report the unmatched
# outgoing call. The replay itself is EXPECTED to not all-pass here.
"$REPLAY_BIN" test -c "./http-pokeapi" --delay 7 --debug --generateGithubActions=false 2>&1 | tee test_logs.txt

if grep "WARNING: DATA RACE" test_logs.txt; then
    echo "::error::Race condition detected in test"
    cat test_logs.txt
    exit 1
fi

# Assert the SPECIFIC forced mismatch surfaced — an HTTP unmatched outgoing
# call on the mutated /api/v2/ path. A generic banner (e.g. a routine DNS miss)
# must NOT satisfy this, otherwise the suite could pass while the HTTP mismatch
# was actually dropped. The per-call heading is "Mock mismatch: [HTTP] <METHOD>
# <path>" and the live path is the unmutated /api/v2/... the app requested.
mismatch_reported=false
if grep -qE "Mock mismatch: \[HTTP\][^]]*/api/v2/" test_logs.txt; then
    echo "✓ replay reported the forced HTTP /api/v2/ mock mismatch"
    mismatch_reported=true
fi

# Equivalent specific check on the test-set report yaml: a SINGLE unmatched_calls
# item whose protocol is HTTP *and* whose actual_summary references /api/v2/.
# actual_summary appears only inside unmatched_calls items, and each item's
# protocol line precedes its actual_summary, so tracking the most recent
# protocol per item binds both fields to the same entry (independent greps
# could otherwise match different items).
shopt -s nullglob
reports=( ./keploy/reports/test-run-*/test-set-*-report.yaml )
shopt -u nullglob
for rf in "${reports[@]}"; do
    if awk '
        /protocol:/        { httpItem = ($0 ~ /HTTP/) }
        /actual_summary:/  { if (httpItem && $0 ~ /\/api\/v2\//) { found = 1 } }
        END                { exit found ? 0 : 1 }
    ' "$rf"; then
        echo "✓ $(basename "$rf") has a single HTTP /api/v2/ unmatched_calls entry"
        mismatch_reported=true
    fi
done

if [ "$mismatch_reported" != true ]; then
    echo "::error::replay did NOT report the forced HTTP /api/v2/ mock mismatch"
    echo "--- mismatch/unmatched lines in test_logs.txt ---"
    grep -iE "mismatch|unmatched|no matching" test_logs.txt | head -20
    echo "--- tail of test_logs.txt ---"
    tail -40 test_logs.txt
    exit 1
fi

echo "mock-mismatch e2e passed: the forced HTTP /api/v2/ unmatched call was reported"
exit 0
