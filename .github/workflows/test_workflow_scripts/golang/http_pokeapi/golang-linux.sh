#!/bin/bash
source "$(dirname "${BASH_SOURCE[0]}")/../../go-retry.sh"

echo "$RECORD_BIN"
echo "$REPLAY_BIN"

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh
echo "iid.sh executed"

# Checkout a different branch
git fetch origin
#git checkout native-linux

# Check if there is a keploy-config file, if there is, delete it.
if [ -f "./keploy.yml" ]; then
    rm ./keploy.yml
fi

rm -rf keploy/

workflow_root="${GITHUB_WORKSPACE:-${PWD%/samples-*}}"
pokeapi_stub_addr="127.0.0.1:18080"
pokeapi_stub_dir=$(mktemp -d)
pokeapi_stub_bin="$pokeapi_stub_dir/pokeapi-stub"
pokeapi_stub_log="$PWD/pokeapi_stub.log"
pokeapi_stub_pid=""

stop_pokeapi_stub() {
    if [ -n "$pokeapi_stub_pid" ]; then
        kill "$pokeapi_stub_pid" 2>/dev/null || true
        wait "$pokeapi_stub_pid" 2>/dev/null || true
        pokeapi_stub_pid=""
    fi
    if [ -n "$pokeapi_stub_dir" ] && [ -d "$pokeapi_stub_dir" ]; then
        rm -f "$pokeapi_stub_bin"
        rmdir "$pokeapi_stub_dir"
        pokeapi_stub_dir=""
    fi
}
trap stop_pokeapi_stub EXIT

# Keep recordings deterministic and independent of pokeapi.co. The checked-out
# sample currently has a constant base URL, so point that disposable source
# tree at the local fixture before building it.
pokeapi_client="internal/pokeapi/poke_req.go"
live_base_url='const baseURL = "https://pokeapi.co/api/v2"'
stub_base_url="const baseURL = \"http://${pokeapi_stub_addr}/api/v2\""
if [ "$(grep -Fxc "$live_base_url" "$pokeapi_client")" -ne 1 ]; then
    echo "::error::could not find the expected PokeAPI base URL in $pokeapi_client"
    exit 1
fi
sed -i "s|^${live_base_url}$|${stub_base_url}|" "$pokeapi_client"
if ! grep -Fxq "$stub_base_url" "$pokeapi_client"; then
    echo "::error::failed to point the sample at the local PokeAPI stub"
    exit 1
fi

go build -o "$pokeapi_stub_bin" "$workflow_root/.github/workflows/test_workflow_scripts/golang/http_pokeapi/pokeapi_stub/main.go"
"$pokeapi_stub_bin" -addr "$pokeapi_stub_addr" >"$pokeapi_stub_log" 2>&1 &
pokeapi_stub_pid=$!

pokeapi_stub_ready=false
for _ in {1..20}; do
    if curl --fail --silent --max-time 2 "http://${pokeapi_stub_addr}/healthz" >/dev/null; then
        pokeapi_stub_ready=true
        break
    fi
    if ! kill -0 "$pokeapi_stub_pid" 2>/dev/null; then
        break
    fi
    sleep 1
done
if [ "$pokeapi_stub_ready" != true ]; then
    echo "::error::the local PokeAPI stub did not become ready"
    cat "$pokeapi_stub_log"
    exit 1
fi

# Build go binary. The retry lives in go-retry.sh now; the flake it exists for
# was first seen here — keploy/keploy#4077 run 24631193918/job/72018929505,
# a TLS handshake timeout fetching github.com/go-chi/chi@v1.5.5.
go_retry build -o http-pokeapi
echo "go binary built"

# Update the global noise to updated_at.
config_file="./keploy.yml"
# Keploy's config now carries only the settings that DIFFER from its
# defaults, so patching a default value out of the generated file with
# `sed` silently patched nothing: the noise rule vanished and every
# replay diffed on the fields it was meant to mask. Write what this
# test needs instead of editing what the generator happened to print.
cat > "$config_file" <<'KEPLOY_CFG'
test:
    globalNoise:
        global: {"body": {"updated_at":[]}}
KEPLOY_CFG

stop_keploy() {
    local pids
    pids=$(pgrep keploy || true)
    if [ -n "$pids" ]; then
        echo "$pids Keploy PID"
        echo "Killing Keploy"
        sudo kill $pids || true
    fi
}

request_failed() {
    echo "::error::$1"
    stop_keploy
}

send_request() {
    local index=$1
    local response
    local location
    local pokemon

    sleep 6
    app_started=false
    for _ in {1..20}; do
        if curl --fail --silent --max-time 5 http://localhost:8080/api/locations >/dev/null; then
            app_started=true
            break
        fi
        sleep 1
    done
    if [ "$app_started" != true ]; then
        request_failed "the http-pokeapi sample did not become ready"
        return 1
    fi

    echo "App started"

    if ! response=$(curl --fail-with-body --silent --show-error --max-time 20 http://localhost:8080/api/locations); then
        request_failed "GET /api/locations failed while recording: ${response:-no response body}"
        return 1
    fi

    # Extract any location from the response and reject missing fixture data.
    if ! location=$(printf '%s' "$response" | jq -er ".location[$index] | select(type == \"string\" and length > 0)"); then
        request_failed "GET /api/locations returned no location at index $index"
        return 1
    fi

    if ! response=$(curl --fail-with-body --silent --show-error --max-time 20 "http://localhost:8080/api/locations/$location"); then
        request_failed "GET /api/locations/$location failed while recording: ${response:-no response body}"
        return 1
    fi

    # Validate that the detail response contains the data the sample exposes.
    if ! pokemon=$(printf '%s' "$response" | jq -er ".[$index] | select(type == \"string\" and length > 0)"); then
        request_failed "GET /api/locations/$location returned no Pokemon at index $index"
        return 1
    fi
    echo "Selected $pokemon from $location"

    if ! curl --fail-with-body --silent --show-error --max-time 20 http://localhost:8080/api/greet >/dev/null; then
        request_failed "GET /api/greet failed while recording"
        return 1
    fi

    if ! curl --fail-with-body --silent --show-error --max-time 20 'http://localhost:8080/api/greet?format=html' >/dev/null; then
        request_failed "GET /api/greet?format=html failed while recording"
        return 1
    fi

    if ! curl --fail-with-body --silent --show-error --max-time 20 'http://localhost:8080/api/greet?format=xml' >/dev/null; then
        request_failed "GET /api/greet?format=xml failed while recording"
        return 1
    fi

    # Wait for 7 seconds for Keploy to record the tcs and mocks.
    sleep 7
    stop_keploy
}

record_iterations() {
    local extra_flags="${1:-}"
    local label="${extra_flags:+_json}"
    for i in {1..2}; do
        local app_name="http-pokeapi_${i}${label}"
        send_request "$i" &
        local request_pid=$!
        # shellcheck disable=SC2086
        "$RECORD_BIN" record $extra_flags -c "./http-pokeapi" --generateGithubActions=false 2>&1 | tee "${app_name}.txt"
        if grep "ERROR" "${app_name}.txt"; then
            echo "Error found in pipeline..."
            cat "${app_name}.txt"
            exit 1
        fi
        if grep "WARNING: DATA RACE" "${app_name}.txt"; then
          echo "Race condition detected in recording, stopping pipeline..."
          cat "${app_name}.txt"
          exit 1
        fi
        if ! wait "$request_pid"; then
            echo "::error::request driver failed during recording iteration ${i}${label:+ (json)}"
            cat "$pokeapi_stub_log"
            exit 1
        fi
        sleep 5
        echo "Recorded test case and mocks for iteration ${i}${label:+ (json)}"
    done
}

record_iterations

# shellcheck disable=SC1091
source "${GITHUB_WORKSPACE:-${PWD%/samples-*}}/.github/workflows/test_workflow_scripts/json-pass-helpers.sh"

if json_pass_supported; then
    record_iterations "--storage-format json"
fi

# A replay must be served by Keploy's mocks. Taking the fixture down here also
# turns a missed mock into a connection failure instead of a false pass.
stop_pokeapi_stub
trap - EXIT

# The strict mock-window replay further down runs only for a replay binary
# from a build artifact (download-binary's build/keploy or
# build-no-race/keploy): a published release on the cross-version matrix may
# predate what it checks. The WSL lanes copy their binaries elsewhere and skip
# it.
strict_phase=false
case "${REPLAY_BIN:-}" in
    */build/keploy|*/build-no-race/keploy) strict_phase=true ;;
esac

# Keep the recordings as recorded for that replay. They have no mappings
# file: this build's keploy record writes the test-mock mappings only with
# --sync, and this lane records without it. The default replay below then
# creates one from the mocks each test consumed under the default tiers, where
# get-api-locations-2, which repeats get-api-locations-1's call, consumes that
# test's session mock. That mapping gives both tests one mock, which a strict
# replay lets only one of them consume.
if [ "$strict_phase" = true ]; then
    rm -rf ./strict-run
    mkdir ./strict-run
    cp -a ./keploy ./strict-run/keploy
    # And a copy for the in-order phase further down (KEPLOY_PROVEN=1).
    rm -rf ./proven-order-run
    mkdir ./proven-order-run
    cp -a ./keploy ./proven-order-run/keploy
fi

# Start the go-http app in test mode.
"$REPLAY_BIN" test -c "./http-pokeapi" --delay 7 --debug --generateGithubActions=false 2>&1 | tee test_logs.txt

if grep "ERROR" "test_logs.txt"; then
    echo "Error found in pipeline..."
    cat "test_logs.txt"
    exit 1
fi

if grep "WARNING: DATA RACE" "test_logs.txt"; then
    echo "Race condition detected in test, stopping pipeline..."
    cat "test_logs.txt"
    exit 1
fi

all_passed=true

# Default-format report scan — globbed so json-recorded test-sets
# (which the yaml replay also produces yaml reports for via auto-detect)
# are picked up automatically.
shopt -s nullglob
yaml_reports=( ./keploy/reports/test-run-0/test-set-*-report.yaml )
shopt -u nullglob
if [ ${#yaml_reports[@]} -eq 0 ]; then
    echo "::error::No yaml test-set reports found under ./keploy/reports/test-run-0/"
    cat "test_logs.txt"
    exit 1
fi
for report_file in "${yaml_reports[@]}"; do
    test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
    echo "yaml report $(basename "$report_file"): $test_status"
    if [ "$test_status" != "PASSED" ]; then
        all_passed=false
        break
    fi
done

if [ "$all_passed" != true ]; then
    cat "test_logs.txt"
    exit 1
fi

# The same recordings under strict mock windows. KEPLOY_STRICT_MOCK_WINDOW=1
# gates off DeriveLifetime's lax promotion, so the recorder's HTTP_CLIENT tag
# makes these HTTP mocks per-test, and the agent parks per-test mocks on disk
# with every response of 8 KiB or more (here the 10 KiB-padded response of the
# local location-area fixture) stored apart from its mock, to be loaded
# when it is served. A replay that served such a mock without loading its
# response gave the application no reply, and the test that made the call
# failed.
if [ "$strict_phase" = true ]; then
    KEPLOY_STRICT_MOCK_WINDOW=1 "$REPLAY_BIN" test -c "./http-pokeapi" --path ./strict-run --delay 7 --debug --generateGithubActions=false 2>&1 | tee test_logs_strict.txt
    if grep "ERROR" "test_logs_strict.txt"; then
        echo "::error::Error found in the strict mock-window replay"
        cat "test_logs_strict.txt"
        exit 1
    fi
    if grep "WARNING: DATA RACE" "test_logs_strict.txt"; then
        echo "::error::Race condition detected in the strict mock-window replay"
        cat "test_logs_strict.txt"
        exit 1
    fi
    shopt -s nullglob
    strict_reports=( ./strict-run/keploy/reports/test-run-0/test-set-*-report.yaml )
    shopt -u nullglob
    if [ ${#strict_reports[@]} -eq 0 ]; then
        echo "::error::the strict mock-window replay wrote no yaml test-set report under ./strict-run/keploy/reports/test-run-0/"
        cat "test_logs_strict.txt"
        exit 1
    fi
    # The phase asserts nothing unless the agent really kept a response
    # apart from its mock in every test set. That needs the per-test tier
    # and a body of 8 KiB or more, so a change to the tier rule, to how the
    # lifetime reaches the agent, or to what the sample records shows up
    # here first. The agent logs its mock residency once per test set
    # while its disk store is engaged, so a count of residency lines, or of
    # lines with spilledResponses above 0, other than the number of test-set
    # reports fails the phase.
    residency_lines=$(grep -c 'per-test mocks parked on disk' test_logs_strict.txt || true)
    spilled_sets=$(grep -Ec 'per-test mocks parked on disk.*"spilledResponses": [1-9]' test_logs_strict.txt || true)
    if [ "${spilled_sets:-0}" -ne ${#strict_reports[@]} ] || [ "${residency_lines:-0}" -ne ${#strict_reports[@]} ]; then
        echo "::error::with KEPLOY_STRICT_MOCK_WINDOW=1 the agent kept a response apart from its mock in ${spilled_sets:-0} of ${#strict_reports[@]} test sets (${residency_lines:-0} mock residency lines), so this replay did not serve such a response in every set. The recorded HTTP mocks should be per-test under strict windows (DeriveLifetime's lax promotion is gated off) and each set should record a body of 8 KiB or more: check that rule, that the CLI still sends each mock's lifetime to the agent, and the sizes of the recorded bodies."
        grep -n "mock residency\|on-disk mock" test_logs_strict.txt || true
        exit 1
    fi
    for report_file in "${strict_reports[@]}"; do
        test_status=$(grep 'status:' "$report_file" | head -n 1 | awk '{print $2}')
        echo "strict mock-window report $(basename "$report_file"): $test_status"
        if [ "$test_status" != "PASSED" ]; then
            echo "::error::$(basename "$report_file") is $test_status under strict mock windows. A likely cause is a mock whose response the agent kept on disk being served without it; see the replay log below (test_logs_strict.txt)."
            cat "test_logs_strict.txt"
            exit 1
        fi
    done
else
    echo "REPLAY_BIN ($REPLAY_BIN) is not a build or build-no-race artifact; skipping the strict mock-window replay"
fi

# Response arrays in order under the proven switch (KEPLOY_PROVEN=1, the
# proven model's staging gate). /api/locations answers {"location":[...]}; the
# recorded order of that array is reversed in a copy of the recordings, so the
# app's live answer has the recorded elements in another order. With the switch
# off, test.ignoreOrdering (on by default) compares arrays as multisets and the
# set passes: that replay is the control. With it on, arrays are compared in
# order, so exactly the edited tests fail and every other test still passes.
if [ "$strict_phase" = true ]; then
    reordered_tests=()
    shopt -s nullglob
    for tc in ./proven-order-run/keploy/test-set-*/tests/*.yaml; do
        if [ "$(yq '.spec.req.url' "$tc")" != "http://localhost:8080/api/locations" ]; then
            continue
        fi
        body=$(yq '.spec.resp.body' "$tc")
        if [ "$(echo "$body" | jq '.location | length')" -lt 2 ]; then
            continue
        fi
        REVERSED=$(echo "$body" | jq -c '.location |= reverse') yq -i '.spec.resp.body = strenv(REVERSED)' "$tc"
        # Test names repeat across test sets: key each by its set.
        reordered_tests+=("$(basename "$(dirname "$(dirname "$tc")")")/$(yq '.name' "$tc")")
    done
    shopt -u nullglob
    if [ ${#reordered_tests[@]} -eq 0 ]; then
        echo "::error::no recorded /api/locations test with a location array of two or more elements to reorder; the in-order phase would assert nothing"
        exit 1
    fi
    echo "reversed the recorded location array of: ${reordered_tests[*]}"

    for phase in off on; do
        rm -rf ./proven-order-run/keploy/reports
        if [ "$phase" = on ]; then
            KEPLOY_PROVEN=1 "$REPLAY_BIN" test -c "./http-pokeapi" --path ./proven-order-run --delay 7 --generateGithubActions=false 2>&1 | tee "test_logs_order_${phase}.txt"
        else
            "$REPLAY_BIN" test -c "./http-pokeapi" --path ./proven-order-run --delay 7 --generateGithubActions=false 2>&1 | tee "test_logs_order_${phase}.txt"
        fi
        if grep "WARNING: DATA RACE" "test_logs_order_${phase}.txt"; then
            echo "::error::Race condition detected in the in-order replay (switch ${phase})"
            exit 1
        fi
        shopt -s nullglob
        order_reports=( ./proven-order-run/keploy/reports/test-run-0/test-set-*-report.yaml )
        shopt -u nullglob
        if [ ${#order_reports[@]} -eq 0 ]; then
            echo "::error::the in-order replay (switch ${phase}) wrote no yaml test-set report"
            cat "test_logs_order_${phase}.txt"
            exit 1
        fi
        for report_file in "${order_reports[@]}"; do
            set_id=$(basename "$report_file" -report.yaml)
            while IFS=$'\t' read -r tcid status; do
                want=PASSED
                if [ "$phase" = on ]; then
                    for r in "${reordered_tests[@]}"; do
                        [ "$r" = "$set_id/$tcid" ] && want=FAILED
                    done
                fi
                if [ "$status" != "$want" ]; then
                    echo "::error::$(basename "$report_file"): $tcid is $status with KEPLOY_PROVEN ${phase}; want $want. Reversed: ${reordered_tests[*]}. With the switch off test.ignoreOrdering forgives the reorder; with it on response arrays are compared in order."
                    cat "test_logs_order_${phase}.txt"
                    exit 1
                fi
            done < <(yq -r '.tests[] | [.test_case_id, .status] | @tsv' "$report_file")
        done
        echo "✓ in-order phase, switch ${phase}: every test has the expected verdict"
    done
fi

if json_pass_supported; then
    "$REPLAY_BIN" test --storage-format json -c "./http-pokeapi" --delay 7 --debug --generateGithubActions=false 2>&1 | tee test_logs_json.txt
    if grep "ERROR" "test_logs_json.txt"; then
        echo "Error found in pipeline (json replay)..."
        cat "test_logs_json.txt"
        exit 1
    fi
    if grep "WARNING: DATA RACE" "test_logs_json.txt"; then
        echo "Race condition detected in json test, stopping pipeline..."
        cat "test_logs_json.txt"
        exit 1
    fi
    if ! json_scan_reports; then
        cat test_logs_json.txt
        exit 1
    fi
    echo "All tests passed (yaml + json)"
else
    echo "All tests passed (yaml only — json pass skipped for compat-matrix cell)"
fi
exit 0
