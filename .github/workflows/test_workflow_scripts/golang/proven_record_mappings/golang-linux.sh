#!/bin/bash
source "$(dirname "${BASH_SOURCE[0]}")/../../go-retry.sh"

# E2E for the proven model's row S3 (KEPLOY_PROVEN=1, the proven model's staging
# gate): a recording made WITHOUT --sync writes mappings.yaml at its end,
# derived from its test windows, so `keploy test` replays it with per-test
# ownership. Before S3 only --sync wrote the file; the first replay of a
# default recording then made one from what each test happened to consume, so
# a call a test stopped making could drop out of its mapping for good.
#
#   1. record http-pokeapi under the switch, no --sync: mappings.yaml exists,
#      lists every test, maps each /api/locations* test to exactly its own
#      pokeapi call, the dependency-free /api/greet tests to nothing, and no
#      test to a DNS lookup;
#   2. replay that recording under the switch: every test passes, and every
#      test that owns a call has its dependencies checked (the mapping is in
#      force: per-test ownership);
#   3. replay it against a build of the app whose /api/locations answers the
#      recorded locations without calling pokeapi: each /api/locations test
#      answers as recorded and FAILS, its recorded call named under
#      failure_info.unconsumed_mocks; every other test passes; the run exits
#      non-zero;
#   4. a replay of a copy without mappings.yaml (a recording made before S3,
#      or without the switch): each test owns the calls recorded inside its
#      own window, derived as step 1's record derived them (S12), so every
#      test passes with its dependencies checked, and no mappings.yaml is
#      created (only record writes it).
#
# CI sources this script from a `bash -e` step: a command that is expected to
# fail runs where errexit does not apply (an `if`, a `||` list).
#
# Never `pgrep keploy`/`pkill keploy` here: the recording is stopped by its own
# pid, so nothing else running as the same user is touched.

echo "$RECORD_BIN"
echo "$REPLAY_BIN"

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh
echo "iid.sh executed"

# Both sides must be this build: the published release neither writes
# window-derived mappings (record) nor knows the switch's verdicts (replay).
for bin in "${RECORD_BIN:-}" "${REPLAY_BIN:-}"; do
    case "$bin" in
        */build/keploy|*/build-no-race/keploy) ;;
        *)
            echo "$bin predates the window-derived mappings of a proven recording (S3); skipping suite"
            exit 0
            ;;
    esac
done

rm -f ./keploy.yml
rm -rf ./keploy/

go_retry build -o http-pokeapi
echo "go binary built"

cat > ./keploy.yml <<'KEPLOY_CFG'
test:
    globalNoise:
        global: {"body": {"updated_at":[]}}
KEPLOY_CFG

fail() {
    echo "::error::$1"
    shift
    for f in "$@"; do
        [ -f "$f" ] && tail -60 "$f"
    done
    exit 1
}

check_race() {
    if grep -q "WARNING: DATA RACE" "$1"; then
        fail "Race condition detected in $1" "$1"
    fi
}

# ---------------------------------------------------------------------------
# 1. The recording: KEPLOY_PROVEN=1, no --sync.
# ---------------------------------------------------------------------------
KEPLOY_PROVEN=1 "$RECORD_BIN" record -c "./http-pokeapi" --generateGithubActions=false > record_logs.txt 2>&1 &
record_pid=$!

# The first answered request is a test like the others; until the app is up
# the curl gets no answer and records nothing.
until curl -fsS http://localhost:8080/api/locations > /dev/null; do
    kill -0 "$record_pid" 2>/dev/null || fail "keploy record exited before the app answered" record_logs.txt
    sleep 1
done
locations=$(curl -fsS http://localhost:8080/api/locations) || fail "GET /api/locations failed while recording" record_logs.txt
location=$(echo "$locations" | jq -r '.location[0]')
for path in "/api/locations/$location" /api/greet "/api/greet?format=xml"; do
    curl -fsS "http://localhost:8080$path" > /dev/null || fail "GET $path failed while recording" record_logs.txt
done

# Stop the recording as Ctrl+C does; it hands over everything captured before
# the stop, and only then exits.
sudo kill -INT "$record_pid"
record_rc=0
wait "$record_pid" || record_rc=$?
check_race record_logs.txt
if grep -qE "mappings.yaml was not written|failed to save the test-mock mappings derived from the test windows|the recording may be incomplete" record_logs.txt; then
    fail "the proven recording did not save all it captured, or its mappings" record_logs.txt
fi
[ "$record_rc" -eq 0 ] || fail "keploy record exited $record_rc" record_logs.txt

shopt -s nullglob
sets=( ./keploy/test-set-* )
shopt -u nullglob
[ ${#sets[@]} -eq 1 ] || fail "want one recorded test set, got ${#sets[@]}" record_logs.txt
set_dir=${sets[0]}
mappings="$set_dir/mappings.yaml"
[ -f "$mappings" ] || fail "a recording made without --sync under KEPLOY_PROVEN=1 wrote no $mappings" record_logs.txt
grep -q "Saved the test-mock mappings derived from the test windows" record_logs.txt ||
    fail "keploy record did not say it saved the mappings derived from the test windows" record_logs.txt

# mock_field NAME EXPR: EXPR of the recorded mock NAME.
mock_field() {
    MOCK="$1" yq ea "select(.name == strenv(MOCK)) | $2" "$set_dir/mocks.yaml"
}

location_tests=()
owned_location_mocks=()
greet_tests=0
for tc in "$set_dir"/tests/*.yaml; do
    id=$(yq '.name' "$tc")
    url=$(yq '.spec.req.url' "$tc")
    if [ "$(ID="$id" yq '[.tests[] | select(.id == strenv(ID))] | length' "$mappings")" != 1 ]; then
        fail "$id has no entry of its own in $mappings" "$mappings"
    fi
    owned=$(ID="$id" yq '.tests[] | select(.id == strenv(ID)) | (.mock_entries // []) | .[].name' "$mappings")
    for m in $owned; do
        [ "$(mock_field "$m" '.kind')" != DNS ] || fail "$id owns the DNS lookup $m: protocol-phase mocks are not a test's" "$mappings"
    done
    case "$url" in
        */api/greet*)
            [ -z "$owned" ] || fail "$id calls nothing, yet owns: $owned" "$mappings"
            greet_tests=$((greet_tests + 1))
            ;;
        */api/locations*)
            [ "$(echo "$owned" | wc -w)" -eq 1 ] || fail "$id should own exactly its one pokeapi call; it owns: ${owned:-none}" "$mappings"
            # A recorded HTTP mock keeps the request's path as its url and the
            # host in its Host header.
            case "$(mock_field "$owned" '.spec.req.header.Host')$(mock_field "$owned" '.spec.req.url')" in
                pokeapi.co/api/v2/location-area*) ;;
                *) fail "$id owns $owned, which is not a pokeapi location-area call" "$mappings" "$set_dir/mocks.yaml" ;;
            esac
            location_tests+=("$id")
            owned_location_mocks+=("$owned")
            ;;
    esac
done
[ ${#location_tests[@]} -ge 2 ] && [ "$greet_tests" -ge 2 ] ||
    fail "want two or more /api/locations* and /api/greet tests, got ${#location_tests[@]} and $greet_tests" record_logs.txt
if [ "$(printf '%s\n' "${owned_location_mocks[@]}" | sort | uniq -d)" != "" ]; then
    fail "two tests own the same call: ${owned_location_mocks[*]}; each test owns the call recorded in its own window" "$mappings"
fi
echo "✓ the recording mapped each test to the calls recorded in its window: ${location_tests[*]}"

# ---------------------------------------------------------------------------
# 2. The recording replayed as recorded, under the switch.
# ---------------------------------------------------------------------------
# A pristine copy for step 4, before any replay touches the recording.
rm -rf ./unmapped-run
mkdir ./unmapped-run
cp -a ./keploy ./unmapped-run/keploy
rm -f ./unmapped-run/keploy/test-set-*/mappings.yaml ./unmapped-run/keploy.yml
cp ./keploy.yml ./unmapped-run/keploy.yml

rm -rf ./keploy/reports
test_rc=0
KEPLOY_PROVEN=1 "$REPLAY_BIN" test -c "./http-pokeapi" --delay 7 --generateGithubActions=false > test_logs.txt 2>&1 || test_rc=$?
check_race test_logs.txt
"$REPLAY_BIN" report --format json > ndjson.txt 2> ndjson_err.txt || true
if [ "$test_rc" -ne 0 ] || ! jq -es 'length > 0 and all(.status == "PASSED")' ndjson.txt > /dev/null; then
    jq -s 'map({test_case_id, status, dependencies_checked})' ndjson.txt
    fail "under KEPLOY_PROVEN=1 the recording must replay green (exit $test_rc)" test_logs.txt
fi
for id in "${location_tests[@]}"; do
    if ! ID="$id" jq -es 'map(select(.test_case_id == env.ID)) | length == 1 and all(.dependencies_checked == true)' ndjson.txt > /dev/null; then
        jq -s 'map({test_case_id, status, dependencies_checked})' ndjson.txt
        fail "$id owns its recorded call, so its dependencies must be checked: the recorded mapping is not in force" test_logs.txt
    fi
done
echo "✓ under KEPLOY_PROVEN=1 the recording replays green, each test against the calls it owns"

# ---------------------------------------------------------------------------
# 3. A removed call: /api/locations answers the recorded locations without
#    calling pokeapi.
# ---------------------------------------------------------------------------
first_location_test=$(grep -l "url: http://localhost:8080/api/locations$" "$set_dir"/tests/*.yaml | head -n 1)
[ -n "$first_location_test" ] || fail "no recorded /api/locations test to take the locations from"
yq '.spec.resp.body' "$first_location_test" | jq '{results: [.location[] | {name: .}]}' > "$PWD/recorded-locations.json"
[ "$(jq '.results | length' recorded-locations.json)" -gt 0 ] || fail "the recorded /api/locations answer lists no location" "$first_location_test"

req=internal/pokeapi/poke_req.go
anchor='func (client \*Client) LocationArearesponse() (models.Location, error) {'
grep -q "^${anchor}\$" "$req" || fail "$req no longer declares LocationArearesponse as this lane patches it"
sed -i "/^${anchor}\$/a\\
\\tif data, err := os.ReadFile(\"$PWD/recorded-locations.json\"); err == nil {\\
\\t\\tv := models.Location{}\\
\\t\\terr = json.Unmarshal(data, \&v)\\
\\t\\treturn v, err\\
\\t}" "$req"
grep -q "recorded-locations.json" "$req" || fail "could not patch $req"
go_retry build -o http-pokeapi-nocall
git checkout -- "$req"

rm -rf ./keploy/reports
removed_rc=0
KEPLOY_PROVEN=1 "$REPLAY_BIN" test -c "./http-pokeapi-nocall" --delay 7 --generateGithubActions=false > removed_logs.txt 2>&1 || removed_rc=$?
check_race removed_logs.txt
"$REPLAY_BIN" report --format json > ndjson_removed.txt 2> ndjson_removed_err.txt || true
[ "$removed_rc" -ne 0 ] || fail "a test that no longer makes its recorded call must fail the run under KEPLOY_PROVEN=1" removed_logs.txt
report=$(ls -1t ./keploy/reports/test-run-*/test-set-*-report.yaml | head -n 1)
for tc in "$set_dir"/tests/*.yaml; do
    id=$(yq '.name' "$tc")
    url=$(yq '.spec.req.url' "$tc")
    status=$(ID="$id" jq -rs 'map(select(.test_case_id == env.ID)) | .[0].status' ndjson_removed.txt)
    if [ "$url" = "http://localhost:8080/api/locations" ]; then
        owned=$(ID="$id" yq '.tests[] | select(.id == strenv(ID)) | .mock_entries[].name' "$mappings")
        [ "$status" = FAILED ] || fail "$id no longer makes its recorded call and is $status; want FAILED" removed_logs.txt
        ID="$id" yq '.tests[] | select(.test_case_id == strenv(ID)) | .failure_info.unconsumed_mocks[].name' "$report" | grep -qx "$owned" ||
            fail "$id's report row must name its recorded call $owned under failure_info.unconsumed_mocks" "$report"
        ID="$id" yq '.tests[] | select(.test_case_id == strenv(ID)) | .result.body_result[].normal' "$report" | grep -qx false &&
            fail "$id's answer should match the recording (only the call is gone)" "$report"
    else
        [ "$status" = PASSED ] || fail "$id still makes its calls and is $status; want PASSED" removed_logs.txt
    fi
done
echo "✓ under KEPLOY_PROVEN=1 a test that no longer makes its recorded call fails with it named unconsumed, and nothing else does"

# ---------------------------------------------------------------------------
# 4. A recording without mappings.yaml (made before S3, or without the
#    switch), under the switch.
# ---------------------------------------------------------------------------
# A set with no mappings.yaml owns what its test windows held (S12): each test
# the calls recorded inside its own window, derived as step 1's record derived
# them. So the replay is step 2's: every test passes, each /api/locations test
# with its dependencies checked against the call it owns; and the replay
# creates no mappings.yaml: only record writes it.
unmapped_rc=0
KEPLOY_PROVEN=1 "$REPLAY_BIN" test -c "./http-pokeapi" --path ./unmapped-run --delay 7 --generateGithubActions=false > unmapped_logs.txt 2>&1 || unmapped_rc=$?
check_race unmapped_logs.txt
"$REPLAY_BIN" report --format json --path ./unmapped-run > ndjson_unmapped.txt 2> ndjson_unmapped_err.txt || true
if [ "$unmapped_rc" -ne 0 ] || ! jq -es 'length > 0 and all(.status == "PASSED")' ndjson_unmapped.txt > /dev/null; then
    jq -s 'map({test_case_id, status, dependencies_checked})' ndjson_unmapped.txt
    fail "under KEPLOY_PROVEN=1 a set without mappings.yaml must replay green, each test owning the calls of its own window (exit $unmapped_rc)" unmapped_logs.txt
fi
for id in "${location_tests[@]}"; do
    if ! ID="$id" jq -es 'map(select(.test_case_id == env.ID)) | length == 1 and all(.dependencies_checked == true)' ndjson_unmapped.txt > /dev/null; then
        jq -s 'map({test_case_id, status, dependencies_checked})' ndjson_unmapped.txt
        fail "$id owns the call recorded in its window, so its dependencies must be checked: ownership was not derived from the test windows" unmapped_logs.txt
    fi
done
grep -q "owns the calls recorded inside its own window" unmapped_logs.txt ||
    fail "the replay did not say it derived each test's ownership from its window" unmapped_logs.txt
shopt -s nullglob
created=( ./unmapped-run/keploy/test-set-*/mappings.yaml )
shopt -u nullglob
[ ${#created[@]} -eq 0 ] || fail "under KEPLOY_PROVEN=1 the replay created ${created[*]}; only record writes mappings.yaml" "${created[@]}"
echo "✓ under KEPLOY_PROVEN=1 a set without mappings.yaml owns what its test windows held, replays green, and the replay writes no mappings.yaml"
