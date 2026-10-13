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

# The app answers /api/locations and /api/locations/{area} by calling
# pokeapi.co, a live third-party API behind Cloudflare. When that call gets a
# status above 399 the app answers 500 with {"error":"bad status code : N"}
# (the area handler, pokemon.go's FetchPokemons, has no return after its
# error and appends "null"), and that is what this lane then records. Twice on 2026-10-13 (runs
# 38342044826 and 38342500968) pokeapi.co answered 502: the recorded mock
# carries Cloudflare's Server and Cf-Ray headers. That set held a 500 where
# the location list should be, then a call for the area "null", and no
# response of 8 KiB or more, and the strict mock-window phase below failed
# blaming keploy. send_request writes "<call> <status>" for each call through
# pokeapi.co to $2, and the app's answer to $2.<call>, so record_iterations can
# tell a usable recording from one of pokeapi.co's outages.

# answered NAME OUT STATUS_FILE: notes the app's answer OUT (curl's body, a
# newline, then its status) to call NAME, and leaves its body in $response.
answered() {
    local code=${2##*$'\n'}
    response=${2%$'\n'*}
    echo "$1 $code" >> "$3"
    printf '%s\n' "$response" > "$3.$1"
}

# all_200 STATUS_FILE: whether every call noted so far got a 200.
all_200() {
    ! grep -qv ' 200$' "$1"
}

send_request() {
    local index=$1
    local status_file=$2
    local out location
    # This runs in the background, and the WSL lane sources this script under
    # set -euo pipefail: a call or a jq that fails must not end it before it
    # stops keploy below, or the record never ends.
    set +e +o pipefail

    : > "$status_file"
    sleep 6
    # The first call that connects is the app's readiness, and is recorded as
    # a test like the others.
    until out=$(curl -s -w '\n%{http_code}' -X GET http://localhost:8080/api/locations); do
        sleep 3
    done
    answered locations-probe "$out" "$status_file"
    
    echo "App started"
    
    # After a call through pokeapi.co that did not get a 200, the iteration is
    # not usable whatever follows (an area named "null"), and each further call
    # can wait a minute on an upstream that is failing: they are not made.
    if all_200 "$status_file"; then
        out=$(curl -s -w '\n%{http_code}' -X GET http://localhost:8080/api/locations)
        answered locations "$out" "$status_file"
    fi

    if all_200 "$status_file"; then
        # Extract any location from the reponse
        location=$(echo "$response" | jq -r ".location[$index]")
        out=$(curl -s -w '\n%{http_code}' -X GET "http://localhost:8080/api/locations/$location")
        answered location-area "$out" "$status_file"
    fi
    
    curl -s -X GET http://localhost:8080/api/greet

    curl -s -X GET http://localhost:8080/api/greet?format=html

    curl -s -X GET http://localhost:8080/api/greet?format=xml

    # Wait for 7 seconds for Keploy to record the tcs and mocks.
    sleep 7
    pid=$(pgrep keploy)
    echo "$pid Keploy PID"
    echo "Killing Keploy"
    sudo kill $pid
}

# The most times one iteration is recorded while pokeapi.co keeps answering
# its calls with an error.
upstream_attempts=5

# test_sets: the test sets under ./keploy, by name.
test_sets() {
    if [ -d ./keploy ]; then
        find ./keploy -mindepth 1 -maxdepth 1 -type d -name 'test-set-*' -printf '%f\n' | sort
    fi
}

# upstream_answers SET_DIR: one line per HTTP mock in SET_DIR that is not a
# 200: its name, status, request URL, and the Server, Cf-Ray and Server-Timing
# headers that show where the answer came from.
upstream_answers() {
    if [ -f "$1/mocks.json" ]; then
        jq -r 'select(.spec.resp.status_code != null and .spec.resp.status_code != 200)
            | "\(.name) \(.spec.resp.status_code) \(.spec.req.url // "-") Server=\(.spec.resp.header.Server // "-") Cf-Ray=\(.spec.resp.header["Cf-Ray"] // "-") Server-Timing=\(.spec.resp.header["Server-Timing"] // "-")"' "$1/mocks.json"
    elif [ -f "$1/mocks.yaml" ]; then
        awk '
            function flush() { if (st != "" && st != "200") print nm, st, u, "Server=" sv, "Cf-Ray=" cf, "Server-Timing=" tm }
            /^---/ { flush(); nm = st = ""; u = sv = cf = tm = "-"; next }
            NR == 1 { u = sv = cf = tm = "-" }
            /^name:/ { nm = $2 }
            $1 == "url:" { u = $2 }
            $1 == "status_code:" { st = $2 }
            $1 == "Server:" { sv = $2 }
            $1 == "Cf-Ray:" { cf = $2 }
            $1 == "Server-Timing:" { tm = $2 }
            END { flush() }' "$1/mocks.yaml"
    fi
}

record_iterations() {
    local extra_flags="${1:-}"
    local label="${extra_flags:+_json}"
    local attempt status_file before new_sets set_dir first call body upstream answers not_upstream
    for i in {1..2}; do
        local app_name="http-pokeapi_${i}${label}"
        attempt=1
        while :; do
            status_file="upstream-${app_name}-${attempt}.txt"
            before=$(test_sets)
            send_request "$i" "$status_file" &
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
            sleep 5
            wait
            # The set this recording made: one, new.
            new_sets=$(comm -13 <(echo "$before") <(test_sets))
            if [ "$(grep -c . <<<"$new_sets")" -ne 1 ]; then
                echo "::error::recording iteration ${i}${label:+ (json)} made $(grep -c . <<<"$new_sets") new test sets under ./keploy, want one: ${new_sets:-none}"
                cat "${app_name}.txt"
                exit 1
            fi
            set_dir="./keploy/${new_sets}"
            # Usable: each of the three calls through pokeapi.co got a 200.
            if [ "$(grep -c ' 200$' "$status_file")" -eq 3 ]; then
                break
            fi
            echo "recording iteration ${i}${label:+ (json)} (attempt ${attempt}) into ${set_dir}: the app did not answer every call through pokeapi.co with 200:"
            cat "$status_file"
            echo "the mocks of ${set_dir} that are not a 200:"
            upstream_answers "$set_dir" || true
            # Recorded again only on proof that pokeapi.co itself answered
            # the error: the app said "bad status code : N" with N a 5xx, and
            # the recording holds an answer N to a location-area request from
            # Cloudflare (its Server or Cf-Ray header). Anything else (no
            # answer, a connection keploy dropped, a request keploy changed,
            # a 4xx such as a 429 a client can bring on itself) is not
            # pokeapi.co failing, and fails the lane.
            first=$(grep -m 1 -v ' 200$' "$status_file" || true)
            call=${first%% *}
            body=$(cat "${status_file}.${call}" 2>/dev/null || true)
            upstream=$(sed -nE 's/^\{"error":"bad status code : ([0-9]+)"\}(null)?$/\1/p' <<<"$body")
            # Captured, not piped into grep -q: under pipefail an early exit
            # of grep would fail the pipeline whatever it found.
            answers=$(upstream_answers "$set_dir" || true)
            not_upstream=""
            if [ -z "$upstream" ]; then
                not_upstream="its answer names no status from pokeapi.co"
            elif ! [[ "$upstream" =~ ^5[0-9][0-9]$ ]]; then
                not_upstream="pokeapi.co's ${upstream} is not a server error (a 4xx such as a 429 a client can bring on itself)"
            elif ! grep -qE "^[^ ]+ ${upstream} (https://pokeapi\.co)?/api/v2/location-area[^ ]* (Server=cloudflare |.* Cf-Ray=[^-])" <<<"$answers"; then
                not_upstream="${set_dir} holds no answer ${upstream} to a location-area request from pokeapi.co itself (Cloudflare's Server or Cf-Ray header)"
            fi
            if [ -n "$not_upstream" ]; then
                echo "::error::recording iteration ${i}${label:+ (json)}: the app answered ${call:-a call} with ${first##* } (${body:-no body}), and ${not_upstream}. That is not pokeapi.co failing, so it is not recorded again."
                cat "${app_name}.txt"
                exit 1
            fi
            if [ "$attempt" -ge "$upstream_attempts" ]; then
                echo "::error::pokeapi.co answered ${upstream} in ${upstream_attempts} recordings of iteration ${i}${label:+ (json)}: no usable recording can be made while it is failing. This is the third-party API, not keploy; its answers are above."
                exit 1
            fi
            echo "::warning::pokeapi.co answered ${upstream} while recording iteration ${i}${label:+ (json)} (attempt ${attempt} of ${upstream_attempts}); discarding ${set_dir} and recording the iteration again"
            rm -rf "$set_dir"
            attempt=$((attempt + 1))
        done
        echo "Recorded test case and mocks for iteration ${i}${label:+ (json)}"
    done
}

record_iterations

# shellcheck disable=SC1091
source "${GITHUB_WORKSPACE:-${PWD%/samples-*}}/.github/workflows/test_workflow_scripts/json-pass-helpers.sh"

if json_pass_supported; then
    record_iterations "--storage-format json"
fi

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
# with every response of 8 KiB or more (here the 14.8-16 KB body of the one
# location area each set fetches) stored apart from its mock, to be loaded
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