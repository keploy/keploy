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

# Force a mock mismatch: rewrite the recorded request host on the mock side so
# the live outgoing request (to pokeapi.co) no longer matches any recorded
# mock. Done on the mocks only — the test cases (inbound) are untouched.
shopt -s nullglob
mock_files=( ./keploy/test-set-*/mocks.yaml )
shopt -u nullglob
if [ ${#mock_files[@]} -eq 0 ]; then
    echo "::error::No recorded mocks found to mutate under ./keploy/test-set-*/"
    cat record_logs.txt
    exit 1
fi
for mf in "${mock_files[@]}"; do
    sed -i 's#pokeapi\.co#pokeapi-mismatch.invalid#g' "$mf"
    echo "mutated recorded mocks: $mf"
done

# Replay. The mutated mocks no longer match → keploy must report the unmatched
# outgoing call. The replay itself is EXPECTED to not all-pass here.
"$REPLAY_BIN" test -c "./http-pokeapi" --delay 7 --debug --generateGithubActions=false 2>&1 | tee test_logs.txt

if grep "WARNING: DATA RACE" test_logs.txt; then
    echo "::error::Race condition detected in test"
    cat test_logs.txt
    exit 1
fi

# Assert the mismatch surfaced. Primary signal: the console "MOCK MISMATCH"
# report / per-call "Mock mismatch:" heading printed by the replayer.
mismatch_reported=false
if grep -qiE "MOCK MISMATCH|Mock mismatch:" test_logs.txt; then
    echo "✓ replay surfaced the MOCK MISMATCH report"
    mismatch_reported=true
fi

# Secondary signal: the test-set report YAML carries unmatched_calls / match_phase.
shopt -s nullglob
reports=( ./keploy/reports/test-run-*/test-set-*-report.yaml )
shopt -u nullglob
for rf in "${reports[@]}"; do
    if grep -qE "unmatched_calls|match_phase|closest_mock" "$rf"; then
        echo "✓ report $(basename "$rf") carries the structured mismatch fields"
        mismatch_reported=true
    fi
done

if [ "$mismatch_reported" != true ]; then
    echo "::error::replay did NOT surface the mock mismatch — report regressed"
    cat test_logs.txt
    exit 1
fi

echo "mock-mismatch e2e passed: the unmatched outgoing call was reported"
exit 0
