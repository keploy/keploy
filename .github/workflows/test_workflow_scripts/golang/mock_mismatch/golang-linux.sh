#!/bin/bash
source "$(dirname "${BASH_SOURCE[0]}")/../../go-retry.sh"

# E2E for the mock-mismatch report. Reuses the http-pokeapi sample (it makes
# mockable outgoing HTTP calls), records it, then MUTATES the recorded mocks so
# the live outgoing request no longer matches any mock on replay. Unlike the
# other suites, success here is INVERTED: we assert that replay SURFACES the
# mismatch (the "MOCK MISMATCH" report with an unmatched outgoing call), not
# that every test passed.
#
# The rest asserts the per-test DEPENDENCY contract (the DepResult writer,
# `keploy report --format json` and --assert-dependencies) over BOTH of its
# branches, using one recording:
#
#   phase 1   (the default  the mutated recording replayed as an ordinary
#   replay)                 user replays it: every test FAILS and the run
#                           exits non-zero, whoever recorded it. The calls got
#                           no recorded answer, so no test is demoted to
#                           OBSOLETE, though a recording that states lifetimes
#                           makes its calls per-test and so eligible;
#   phase 2   (steps 0-8)   every dependency session-tier: as an older
#                           recording's lax promotion leaves them, or, for a
#                           recording that states lifetimes, as keploy.yaml's
#                           sessionCalls declares the sample's pokeapi.co calls.
#                           Nothing is eligible, and the honest verdict is NOT
#                           CHECKED;
#   phase 3   (steps 9-11)  the per-test tier in force, on a copy of the
#                           recording whose tests' answers no longer match and
#                           whose mappings each list a twin of the test's
#                           call, which no call consumes, so that nothing
#                           misses: the assertion RUNS (checked bit, MISSING
#                           row), and the knob's delta: a demoted wrong
#                           answer (OBSOLETE, not judged, its set INTERNAL_ERR)
#                           -> FAILED (its set FAILED). Both runs exit
#                           non-zero: a demoted wrong answer is never a pass.
#
# Neither phase is sufficient alone — a writer stuck at "checked" passes phase 3
# and a writer stuck at "not checked" passes phase 1-2. Read the long comment
# above step 0 and the one above step 9 before changing either.

echo "$RECORD_BIN"
echo "$REPLAY_BIN"

source ./../../.github/workflows/test_workflow_scripts/test-iid.sh
echo "iid.sh executed"

git fetch origin

# Mock-mismatch reporting (the per-test capture window + unmatched_calls in the
# report) is new in this PR. On the cross-version matrix the 'latest' replay
# binary is the published release that predates this feature, so it can never
# emit the "Mock mismatch: [HTTP]" line / unmatched_calls entry this suite
# asserts (success is inverted) — skip cleanly there rather than fail
# deterministically. Local build binaries (*/build/keploy, */build-no-race/
# keploy) have the feature. Mirrors the streaming sibling + risk_profile /
# connect_tunnel capability gates.
case "${REPLAY_BIN:-}" in
    */build/keploy|*/build-no-race/keploy) ;;
    *)
        echo "REPLAY_BIN ($REPLAY_BIN) predates mock-mismatch reporting; skipping suite"
        exit 0
        ;;
esac

if [ -f "./keploy.yml" ]; then
    rm ./keploy.yml
fi
rm -rf keploy/

go_retry build -o http-pokeapi
echo "go binary built"

config_file="./keploy.yml"
# Keploy's config now carries only the settings that DIFFER from its
# defaults, so patching a default value out of the generated file with
# `sed` silently patched nothing: the noise rule vanished and every
# replay diffed on the fields it was meant to mask. Write what this
# test needs instead of editing what the generator happened to print.
# write_config [declared]: declared adds the sessionCalls rule that makes the
# sample's pokeapi.co calls session-tier (phase 2, on a recording that states
# lifetimes; see the comment above step 0).
write_config() {
    cat > "$config_file" <<'KEPLOY_CFG'
test:
    globalNoise:
        global: {"body": {"updated_at":[]}}
KEPLOY_CFG
    if [ "${1:-}" = declared ]; then
        cat >> "$config_file" <<'KEPLOY_CFG'
sessionCalls:
    - host: "pokeapi.co"
KEPLOY_CFG
    fi
}
write_config

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
    # Unquoted on purpose: pgrep can return BOTH the keploy CLI and the agent
    # process, so $pid may be multiple newline-separated PIDs. Quoting it passes
    # one mangled arg to kill ("failed to parse argument"), keploy survives, and
    # `wait` on the record command hangs to the job timeout. Word-split instead.
    sudo kill $pid
}

# Record one iteration of test cases + mocks.
#
# --sync IS LOAD-BEARING, not a style choice. mappings.yaml (the per-test
# test<->mock mapping) is produced ONLY by synchronous record: the agent emits a
# TestMockMapping from SyncMockManager.ResolveRange, and the only call site that
# reaches it is the `if synchronous` branch of the capture hook
# (pkg/agent/hooks/conn/util.go). A plain `keploy record` writes tests + mocks
# and NO keploy/test-set-*/mappings.yaml, and without that file replay falls
# back to timestamp-based mock filtering (useMappingBased=false), the per-test
# dependency assertion is never armed, and every dependency assertion below
# would be asserting a run in which nothing was even attempted.
# --debug: which test a mock is mapped to is decided in the agent's mock
# manager (diag/ResolveRange, diag/FlushOwnedWindows), and said only at DEBUG.
# A recording that leaves a test without its mapping (step 9's THIN-recording
# error) is otherwise left with nothing to tell why. It adds under 200 lines.
send_request &
"$RECORD_BIN" record -c "./http-pokeapi" --sync --debug --generateGithubActions=false 2>&1 | tee record_logs.txt
if grep "WARNING: DATA RACE" record_logs.txt; then
    echo "::error::Race condition detected in recording"
    cat record_logs.txt
    exit 1
fi
wait
echo "Recorded test cases and mocks"

# The recording as recorded, before the mutation below, for the proven-switch
# steps at the end (12b, 12c).
rm -rf ./pristine-run
mkdir ./pristine-run
cp -a ./keploy ./pristine-run/keploy

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
# outgoing call.
#
# THIS RUN IS RED, and is asserted red below. With no mock serving /api/v2/ the
# app's outgoing call fails, so its responses drift from what was recorded and
# every test comes back FAILED, with a non-zero exit, whoever recorded it.
#
# On a recording made before lifetimes were stated every dependency is
# session-tier, so the mock set never diverges and the demotion to OBSOLETE
# never fires. On one that states them each test's call is per-test and its
# mapped mock goes unconsumed, so its mock set diverges; it is still FAILED,
# because a call of it got no recorded answer, which vetoes the demotion (a
# demoted test would leave this red run green).
"$REPLAY_BIN" test -c "./http-pokeapi" --delay 7 --debug --generateGithubActions=false 2>&1 | tee test_logs.txt
default_rc=${PIPESTATUS[0]}

if grep "WARNING: DATA RACE" test_logs.txt; then
    echo "::error::Race condition detected in test"
    cat test_logs.txt
    exit 1
fi

# Assert the SPECIFIC forced mismatch surfaced — an HTTP unmatched outgoing
# call on the mutated path. A generic banner (e.g. a routine DNS miss) must
# NOT satisfy this, otherwise the suite could pass while the HTTP mismatch
# was actually dropped. The per-call heading is "Mock mismatch: [HTTP]
# <METHOD> <path>". The live path is the unmutated /api/v2/... the app
# requested, and its closest recording's is /api/v0-mismatch/...: the miss's
# "path" field diff finds the second segment differs, and with
# KEPLOY_LOG_CONTENT unset the report withholds that diff's values and so
# names the call's path with that segment written {}: /api/{}/..., the
# segments the recording has too kept.
mismatch_reported=false
if grep -qE "Mock mismatch: \[HTTP\][^]]*/api/\{\}/" test_logs.txt; then
    echo "✓ replay reported the forced HTTP /api/{}/ mock mismatch"
    mismatch_reported=true
fi

# Equivalent specific check on the test-set report yaml: a SINGLE unmatched_calls
# item whose protocol is HTTP *and* whose actual_summary references /api/{}/.
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
        /actual_summary:/  { if (httpItem && index($0, "/api/{}/") > 0) { found = 1 } }
        END                { exit found ? 0 : 1 }
    ' "$rf"; then
        echo "✓ $(basename "$rf") has a single HTTP /api/{}/ unmatched_calls entry"
        mismatch_reported=true
    fi
done

if [ "$mismatch_reported" != true ]; then
    echo "::error::replay did NOT report the forced HTTP /api/{}/ mock mismatch"
    echo "--- mismatch/unmatched lines in test_logs.txt ---"
    grep -iE "mismatch|unmatched|no matching" test_logs.txt | head -20
    echo "--- tail of test_logs.txt ---"
    tail -40 test_logs.txt
    exit 1
fi

# The default run is red: a non-zero exit, and every test FAILED, none
# OBSOLETE or PASSED.
statuses=$(yq -N '.tests[].status' "${reports[@]}" 2>/dev/null | sort | uniq -c | tr '\n' ' ')
if [ "$default_rc" -eq 0 ] || [ -z "$statuses" ] || [ -n "$(yq -N '.tests[] | select(.status != "FAILED") | .test_case_id' "${reports[@]}")" ]; then
    echo "::error::the default replay of the mutated recording must FAIL every test and exit non-zero, whoever recorded it: its calls got no recorded answer. It exited $default_rc with statuses: $statuses"
    exit 1
fi
echo "✓ the default replay failed every test and exited $default_rc"

# The miss's report names the call and the field that differed, and none of
# the values it was found in, unless KEPLOY_LOG_CONTENT opts in (it does not
# here). The mutated recorded path (/api/v0-mismatch/) reaches a miss's report
# only as content: the closest mock's rendered request (closest_mock_req) and
# the recorded value of its "path" field diff. Only the misses are read: the
# test's dependency rows name the recorded call by its path by design.
for rf in "${reports[@]}"; do
    calls=$(yq -o=json '[.tests[].failure_info.unmatched_calls[]?]' "$rf")
    if grep -nE 'v0-mismatch|closest_mock_req|received_req' <<<"$calls"; then
        echo "::error::$(basename "$rf")'s unmatched calls carry the recorded request's content with KEPLOY_LOG_CONTENT unset"
        exit 1
    fi
    if [ "$(yq '[.tests[].failure_info.unmatched_calls[]? | select(.content_withheld != true)] | length' "$rf")" != 0 ]; then
        echo "::error::$(basename "$rf") has an unmatched call that does not say its content was withheld"
        echo "$calls" | head -60
        exit 1
    fi
    if ! yq -e '[.tests[].failure_info.unmatched_calls[]?.field_diffs[]? | select(.path == "path" and .kind == "value_changed" and (has("expected") | not) and (has("actual") | not))] | length > 0' "$rf" >/dev/null; then
        echo "::error::$(basename "$rf") does not name the path that differed, without its values"
        echo "$calls" | head -60
        exit 1
    fi
done
# The same for what the replay printed: its MOCK MISMATCH table (the log
# lines around it are not the report's) names the field and withholds both
# values, and says once how to see them.
table=$(sed -n '/=== MOCK MISMATCH ===/,$p' test_logs.txt | grep -v 'Keploy')
if grep -n 'v0-mismatch' <<<"$table"; then
    echo "::error::the MOCK MISMATCH table printed the recorded request's content with KEPLOY_LOG_CONTENT unset"
    exit 1
fi
if ! grep -qE '^[^a-zA-Z]*path[^a-zA-Z(]*\(withheld\)[^a-zA-Z(]*\(withheld\)' <<<"$table" || ! grep -q 'values are withheld; set KEPLOY_LOG_CONTENT=1 to include them' <<<"$table"; then
    echo "::error::the MOCK MISMATCH table does not name the path that differed with its values withheld"
    echo "$table" | head -60
    exit 1
fi
echo "✓ the default report and table name the miss without the recorded request's content"

# Phase 2 asserts the NOT CHECKED branch, which needs every dependency
# session-tier. A recording made before lifetimes were stated is, as
# recorded: the default run above is that run. A recording that states them
# is replayed again with keploy.yaml declaring the sample's pokeapi.co calls
# shared (sessionCalls), which makes them session-tier.
if grep -q 'lifetime: perTest' ./keploy/test-set-*/mocks.yaml; then
    rm -rf ./keploy/reports
    write_config declared
    "$REPLAY_BIN" test -c "./http-pokeapi" --delay 7 --debug --generateGithubActions=false 2>&1 | tee test_logs_declared.txt
    default_rc=${PIPESTATUS[0]}
    if grep "WARNING: DATA RACE" test_logs_declared.txt; then
        echo "::error::Race condition detected in the declared replay"
        exit 1
    fi
    echo "phase 2 runs on the replay with the pokeapi.co calls declared shared (exit $default_rc)"
fi

# ---------------------------------------------------------------------------
# Dependency assertions on the sync path (keploy-consumer-design-v2.md §7
# slice 4): the DepResult writer, the `keploy report --format json` verdict
# contract, and the --assert-dependencies knob. Everything below asserts on the
# REPORT, the NDJSON and the EXIT CODE; only the ONE assertion that has no
# machine-readable surface (the inert-knob warning) reads a log.
#
# WHAT THIS SAMPLE CAN PROVE, AND WHAT IT CANNOT — read this before "fixing" a
# failure here by relaxing an assertion.
#
# The per-test dependency assertion covers PER-TEST TIER mocks only. Two classes
# are excluded on purpose:
#   * DNS — resolution order is not deterministic;
#   * session/connection tier — recorded once at app boot and shared by every
#     test, so "this test's window did not consume it" is not evidence of
#     anything and asserting it would turn healthy tests RED at random.
#
# This build's HTTP recorder states each mock's lifetime: per-test, the call
# of the test that made it (Spec.Metadata["lifetime"]). A call every test may
# be served is declared in keploy.yaml (sessionCalls), and DeriveLifetime
# classifies the mocks it covers as SESSION tier; such a recording is replayed
# for this phase with the sample's pokeapi.co calls declared (above). A
# recording made by an older recorder states nothing, and DeriveLifetime
# classifies its mocks (tagged "HTTP_CLIENT", non-canonical) as session tier by
# its lax promotion (rule 5). Either way every mock this sample records is
# session tier here:
#
#   name=mock-2 kind=Http lifetime="session" metaType="HTTP_CLIENT" reusable=true
#   name=mock-3 kind=Http lifetime="session" metaType="HTTP_CLIENT" reusable=true
#
# So for that tier, the honest per-test dependency verdict is NOT CHECKED:
# mappings.yaml exists and lists those mocks, the assertion is armed, and every
# entry is filtered out before it can be asserted. In THIS phase there is no
# missing-row path to exercise and --assert-dependencies cannot fail
# anything.
#
# An earlier version of this suite asserted the opposite (a MISSING dep row, a
# FAILED test set under the knob) in THIS phase, and could never pass. Do NOT
# restore those assertions by making the writer count reusable-tier mocks: that
# is the false RED the exclusion exists to prevent. The missing-row path is
# earned instead in phase 3, by replaying the same recording with the lax
# tier-promotion rule switched off so the mocks are genuinely per-test — see the
# comment above step 9.
#
# The assertions below are therefore built so that a run which checked nothing
# CANNOT satisfy them silently:
#   * the mapping file must exist and list mock entries (otherwise "not checked"
#     would be true for the boring reason that nothing was armed);
#   * the inert warning must name the SESSION-TIER reason specifically — the
#     reason ordering in dependencyAssertionInertReason puts "no usable
#     mappings.yaml" ahead of it, so this string can only appear when the
#     mapping precondition really held;
#   * the false-green invariant (step 4) is universal, not sample-specific.
# ---------------------------------------------------------------------------

# 0. THE PRECONDITION, ASSERTED RATHER THAN ASSUMED. --sync must have produced a
#    mappings.yaml that actually lists mocks. Without this, every "not checked"
#    assertion below would also hold for a run where mapping was never armed,
#    and the suite would be green while proving nothing.
shopt -s nullglob
mapping_files=( ./keploy/test-set-*/mappings.yaml )
shopt -u nullglob
if [ ${#mapping_files[@]} -eq 0 ]; then
    echo "::error::no keploy/test-set-*/mappings.yaml was recorded. The per-test mock mapping is produced ONLY by synchronous record — check that '--sync' is still on the record command above and that the agent forwarded it."
    ls -R ./keploy | head -40
    exit 1
fi
mapping_has_entries=false
for mp in "${mapping_files[@]}"; do
    if grep -qE "^[[:space:]]*-[[:space:]]+name:" "$mp"; then
        echo "✓ $(basename "$(dirname "$mp")")/mappings.yaml lists per-test mock entries"
        mapping_has_entries=true
    fi
done
if [ "$mapping_has_entries" != true ]; then
    echo "::error::mappings.yaml exists but lists no mock entries; the dependency assertion has no expectation to work from and every check below would be vacuous"
    head -30 "${mapping_files[0]}"
    exit 1
fi

# 1. `keploy report --format json` must be machine-readable. The regression
#    this catches: the ANSI logo, the "version:" line and zap INFO records went
#    to STDOUT ahead of the NDJSON, so `| jq` failed to parse.
"$REPLAY_BIN" report --format json > ndjson_out.txt 2> ndjson_err.txt
report_rc=$?
if [ "$report_rc" -ne 0 ]; then
    echo "::error::keploy report --format json exited $report_rc"
    cat ndjson_err.txt
    exit 1
fi
if [ ! -s ndjson_out.txt ]; then
    echo "::error::keploy report --format json produced no NDJSON on stdout"
    cat ndjson_err.txt
    exit 1
fi
# Every line must be a standalone JSON object carrying the schema version.
# `jq -e` fails on a parse error AND on a false/null result, so this is one
# assertion for "parseable" and "has the contract fields". dependencies_checked
# is in the list because a consumer is REQUIRED to read it before treating an
# empty `effects` as a clean dependency verdict — a projection that stops
# emitting the key removes the only thing standing between an agent and a false
# green.
line_no=0
while IFS= read -r line; do
    line_no=$((line_no + 1))
    if ! printf '%s' "$line" | jq -e 'has("schema_version") and has("test_case_id") and has("status") and has("effects") and has("dependencies_checked")' > /dev/null; then
        echo "::error::NDJSON line $line_no is not a parseable verdict object"
        echo "--- line $line_no ---"
        printf '%s\n' "$line"
        echo "--- full stdout (first 20 lines) ---"
        head -20 ndjson_out.txt
        exit 1
    fi
done < ndjson_out.txt
echo "✓ keploy report --format json emitted $line_no parseable NDJSON verdict(s)"

# --format junit has the same stdout contract.
"$REPLAY_BIN" report --format junit > junit_out.xml 2> junit_err.txt
if ! head -1 junit_out.xml | grep -q '^<?xml'; then
    echo "::error::keploy report --format junit did not start with the XML declaration"
    head -20 junit_out.xml
    exit 1
fi
echo "✓ keploy report --format junit emitted a clean XML document"

# 2. THE HONEST VERDICT FOR THIS SAMPLE. Every mapped dependency is session
#    tier, so nothing is eligible and every verdict must say NOT CHECKED.
#
#    `dependencies_checked: true` here would mean the writer had claimed the
#    assertion ran over a test whose every dependency it had just filtered away
#    — which is what this sample used to produce (`dependencies_checked: true,
#    dependencies_consumed: 0, effects: []`) and exactly the false green the
#    whole projection exists to close.
if ! jq -es 'length > 0 and all(.dependencies_checked == false)' ndjson_out.txt > /dev/null; then
    echo "::error::a verdict reported dependencies_checked=true, but every mock this sample records is session-tier in this phase (a recording made before lifetimes were stated, or one replayed with its pokeapi.co calls declared in sessionCalls) and therefore ineligible for the per-test assertion. Either the tier classification changed (check the sessionCalls rule, and the recorder's metadata type and lifetime) or the writer is claiming an assertion it did not run."
    jq -s 'map({test_case_id, status, dependencies_checked, dependencies_consumed, effects: (.effects // [] | length)})' ndjson_out.txt
    exit 1
fi
echo "✓ every verdict reports the dependency assertion as NOT CHECKED"

# 3. ...and NOT CHECKED must be reported as nothing else. A verdict that says
#    "not checked" while carrying dependency effects or a consumed count is
#    self-contradictory: the consumer rule is `checked && any(matched==false)`,
#    so rows under an unchecked verdict are invisible to every conforming
#    reader.
if ! jq -es 'map(select(.dependencies_checked == false and (((.effects // []) | length) > 0 or (.dependencies_consumed // 0) > 0))) | length == 0' ndjson_out.txt > /dev/null; then
    echo "::error::a verdict reports dependency data under dependencies_checked=false; a conforming consumer never reads it, so it is data that cannot be acted on"
    jq -s 'map(select(.dependencies_checked == false and (((.effects // []) | length) > 0 or (.dependencies_consumed // 0) > 0)))' ndjson_out.txt
    exit 1
fi

# 4. THE FALSE-GREEN INVARIANT. Universal, not sample-specific: if the assertion
#    really ran, at least one dependency was eligible, so consuming NONE of them
#    means every one of them is missing and `effects` cannot be empty.
#
#    `checked=true, consumed=0, effects=[]` is therefore unreachable by
#    construction — unless the writer starts setting the checked bit for a test
#    it never had a dependency to check, which is the defect this step guards.
#    It stays true (and keeps biting) for any sample, including one that later
#    grows genuinely per-test mocks.
if ! jq -es 'map(select(.dependencies_checked == true and (.dependencies_consumed // 0) == 0 and ((.effects // []) | length) == 0)) | length == 0' ndjson_out.txt > /dev/null; then
    echo "::error::a verdict claims the dependency assertion ran and is clean while reporting zero consumed dependencies and zero effects. That combination cannot happen when something was actually eligible: it means the checked bit was set for a test with nothing to check."
    jq -s 'map(select(.dependencies_checked == true and (.dependencies_consumed // 0) == 0 and ((.effects // []) | length) == 0))' ndjson_out.txt
    exit 1
fi
echo "✓ no verdict claims a checked-and-clean dependency result with nothing eligible"

# 5. The same statements on the PERSISTED report, which is the artifact users
#    keep and the fleet report store ingests.
shopt -s nullglob
reports=( ./keploy/reports/test-run-*/test-set-*-report.yaml )
shopt -u nullglob
if [ ${#reports[@]} -eq 0 ]; then
    echo "::error::the default replay wrote no test-set report"
    exit 1
fi
for rf in "${reports[@]}"; do
    # 5a. deps_checked is omitempty, so an honest not-checked test writes NO
    #     key at all — byte-identical to every report recorded before this
    #     field had a writer.
    if grep -q 'deps_checked: true' "$rf"; then
        echo "::error::$(basename "$rf") persisted deps_checked: true for a test set whose every mapped dependency is session-tier"
        grep -n -B6 'deps_checked: true' "$rf" | head -40
        exit 1
    fi
    # 5b. No missing rows either: nothing was asserted, so nothing can be
    #     reported missing. A row here would be the false RED the tier
    #     exclusion exists to prevent.
    if grep -q 'actual: not consumed' "$rf"; then
        echo "::error::$(basename "$rf") persisted a MISSING dependency row for a mock that is not attributable to a single test (session/connection tier). This is the false RED the tier filter exists to prevent."
        grep -n -B6 'actual: not consumed' "$rf" | head -40
        exit 1
    fi
    # 5c. THE REPORT-SIZE HALF OF THE CONTRACT. Only MISSING dependencies are
    #     ever persisted as rows; the ones that WERE exercised are a count
    #     (`deps_consumed`) plus the `deps_checked` bit. One row per consumed
    #     dependency costs ~190 bytes of YAML, and a Postgres-chatty suite
    #     consumes 50-200 mocks per test — 3-11 MB per test-set report, written
    #     to disk, re-read by `keploy report` and uploaded to the fleet report
    #     store, of `consumed/consumed` boilerplate no consumer reads.
    if grep -q 'actual: consumed$' "$rf"; then
        echo "::error::$(basename "$rf") persisted a MATCHED dependency row. The consumed side must be the deps_consumed count, never rows."
        grep -n -B4 'actual: consumed$' "$rf" | head -30
        exit 1
    fi
done
echo "✓ the persisted reports carry the same not-checked verdict, and no matched rows"

# 6. The exit code of the default run. This suite deliberately mutates mocks and
#    its header says success is INVERTED, so the default replay may redden for
#    RESPONSE reasons that have nothing to do with dependencies. It is captured
#    as the baseline for step 7 and printed as context, never asserted on its
#    own.
echo "--- default replay exit code (diagnostic only): $default_rc ---"

# 7. THE KNOB, over a test set where it cannot assert anything.
#    --assert-dependencies must then be a NO-OP: same exit code as the default
#    run, no test failed for a dependency, and ONE warning per test set saying
#    why. A knob that reddens a run it cannot assert is worse than one that is
#    silently inert.
rm -rf ./keploy/reports
"$REPLAY_BIN" test -c "./http-pokeapi" --delay 7 --assert-dependencies --generateGithubActions=false 2>&1 | tee assert_logs.txt
assert_rc=${PIPESTATUS[0]}

if grep "WARNING: DATA RACE" assert_logs.txt; then
    echo "::error::Race condition detected under --assert-dependencies"
    exit 1
fi

# 7a. THE WARNING, and the ONE assertion here that has to read a log because it
#     has no machine-readable surface. It is also what makes every "not checked"
#     assertion above non-vacuous: dependencyAssertionInertReason checks the
#     set-wide preconditions FIRST, so a run with no usable mappings.yaml emits
#     the "--update-test-mapping" reason instead. Matching the session-tier
#     wording therefore proves the mapping was armed and the entries really were
#     dropped on tier.
#
#     The other half of this assertion — that the string is not simply always
#     printed — is step 11pre, which greps the SAME string out of a run with the
#     knob on whose dependencies were eligible and were asserted. It has to be
#     that run: warnDependencyAssertionInert returns at its first line when
#     --assert-dependencies is off, so a knob-off log is silent whatever the
#     reason logic does and proves nothing.
if ! grep -q 'no eligible dependency to assert' assert_logs.txt; then
    echo "::error::--assert-dependencies ran over a test set where nothing was eligible and never said so. The user is left with a report full of dependencies_checked=false and no explanation."
    echo "--- inert / dependency lines in assert_logs.txt ---"
    grep -inE 'assert-dependencies|inert|eligible|dependency' assert_logs.txt | head -20
    exit 1
fi
if ! grep -q 'session/connection-tier' assert_logs.txt; then
    echo "::error::the inert warning fired but named a different reason than the session/connection tier one. Reason ordering puts the set-wide preconditions first, so this means the mapping precondition ITSELF failed and every not-checked assertion above passed for the wrong reason."
    grep -n 'inert\|eligible\|reason' assert_logs.txt | head -20
    exit 1
fi
echo "✓ the run explains WHY nothing was asserted (session/connection-tier dependencies only)"

# 7b. The knob changed nothing. Asserted as a DELTA against the default run
#     rather than as a literal `exit 0`, because this suite mutates mocks on
#     purpose and the response side may legitimately redden the run; the
#     dependency contract is that turning the knob on adds no failure of its
#     own.
if [ "$assert_rc" -ne "$default_rc" ]; then
    echo "::error::--assert-dependencies changed the run's exit code ($default_rc -> $assert_rc) on a test set where it has nothing to assert. Every mapped dependency here is session-tier and is excluded from the assertion, so the knob must not be able to fail anything."
    tail -40 assert_logs.txt
    exit 1
fi
echo "✓ --assert-dependencies left the exit code unchanged ($assert_rc): it cannot assert, so it fails nothing"

# 7c. ...and nothing was labelled DEPENDENCY_MISSING or promoted through the
#     dependency path. The exit code alone would not catch a test that the knob
#     failed while another test happened to go green.
shopt -s nullglob
assert_reports=( ./keploy/reports/test-run-*/test-set-*-report.yaml )
shopt -u nullglob
if [ ${#assert_reports[@]} -eq 0 ]; then
    echo "::error::--assert-dependencies wrote no report"
    exit 1
fi
for rf in "${assert_reports[@]}"; do
    if grep -q 'DEPENDENCY_MISSING' "$rf"; then
        echo "::error::$(basename "$rf") labelled a test DEPENDENCY_MISSING under --assert-dependencies, but no dependency of this test set was ever eligible to be asserted"
        grep -n -B10 'DEPENDENCY_MISSING' "$rf" | head -40
        exit 1
    fi
    if grep -q 'deps_checked: true' "$rf"; then
        echo "::error::$(basename "$rf") persisted deps_checked: true under --assert-dependencies. The knob changes the VERDICT a missing dependency produces, never whether the assertion ran."
        exit 1
    fi
done
echo "✓ --assert-dependencies failed nothing and claimed nothing"

# 8. THE KNOB DOES NOT CHANGE WHAT IS PERSISTED. Re-project the knob run and
#    assert the same verdict contract holds: the flag is documented as changing
#    only the STATUS a lost dependency produces, so a consumer's parsing must be
#    mode-independent.
"$REPLAY_BIN" report --format json > ndjson_assert.txt 2> ndjson_assert_err.txt
if [ ! -s ndjson_assert.txt ]; then
    echo "::error::keploy report --format json produced no NDJSON after the --assert-dependencies run"
    cat ndjson_assert_err.txt
    exit 1
fi
if ! jq -es 'length > 0 and all(.dependencies_checked == false)' ndjson_assert.txt > /dev/null; then
    echo "::error::--assert-dependencies changed the dependency verdict from NOT CHECKED to checked. The flag decides what a MISSING dependency does to the status; it can never make an ineligible dependency eligible."
    jq -s 'map({test_case_id, status, dependencies_checked, dependencies_consumed})' ndjson_assert.txt
    exit 1
fi
if ! jq -es 'map(select(.dependencies_checked == true and (.dependencies_consumed // 0) == 0 and ((.effects // []) | length) == 0)) | length == 0' ndjson_assert.txt > /dev/null; then
    echo "::error::the knob run produced a checked-and-clean verdict with nothing eligible"
    jq -s 'map(select(.dependencies_checked == true))' ndjson_assert.txt
    exit 1
fi
echo "✓ the verdict contract is identical with the knob on"

# ---------------------------------------------------------------------------
# PHASE 3 — THE OTHER BRANCH, EARNED RATHER THAN FAKED.
#
# Everything above asserts the NOT-CHECKED branch. On its own that is only half
# a test: a writer that returned "not checked" UNCONDITIONALLY would satisfy
# every assertion up to here. Steps 9-11 replay the SAME recording with the
# per-test tier actually in force, so the suite also pins the checked branch —
# the `deps_checked` bit, the MISSING row and the knob's RED — the last of these
# as a two-sided delta against the identical knob-off run, never as a grep for
# the DEPENDENCY_MISSING label, which attachDepResults writes with or without
# the flag. Between them the two phases bracket the writer: neither a
# permanently-true nor a permanently-false `dependencies_checked` can pass.
#
# HOW THE TIER CHANGES, AND WHY THIS IS NOT A FAKE. Nothing is re-recorded.
# keploy.yml declares no sessionCalls, and KEPLOY_STRICT_MOCK_WINDOW=1 is in
# the environment; both are documented, product-supported settings. A
# recording whose recorder stated lifetimes is per-test without the
# declaration; an older one, once the lax promotion goes too:
#
# WHY A COPY OF THE RECORDING, NOT THE MUTATED ONE. The mutated recording's
# calls get no recorded answer, and a miss vetoes the demotion to OBSOLETE on
# its own (phase 1): there would be no demotion for the knob to veto. This
# phase replays a copy of the recording as recorded (pristine-run) in which
# each test's recorded answer no longer matches (its status code is changed)
# and each test's mapping lists a twin of its recorded call. The call is
# answered, by the recording or its twin, so nothing misses; the other is
# never consumed, so the mock set diverges, and the failed answer is demoted
# to OBSOLETE unless a veto flag says otherwise.
#
# models.Mock.DeriveLifetime applies its precedence list top-to-bottom. The HTTP
# recorder tags its mocks `type: HTTP_CLIENT`, which is neither "config" nor
# "connection", so rules 2-3 do not fire, and rule 4 (the untagged kind
# fallback) does not fire either because the tag is NOT empty. What classifies
# these mocks as session in phases 1-2 is rule 5 — the LAX-MODE promotion that
# reproduces pre-tag behaviour for a non-canonical tag on an implicit-session
# kind. laxKindFallbackDisabled() gates rule 5 off when
# KEPLOY_STRICT_MOCK_WINDOW is set to an enabling value, and with rule 5 gone
# the recorder's own tag is honoured and the mocks land on rule 6:
# LifetimePerTest. They are then eligible, and the assertion has something real
# to run over.
#
# So the two phases are the two sides of one documented rule, not a test and a
# workaround. Phase 1-2 is what an ordinary user gets today; phase 3 is what the
# same recording means once the lax compat promotion is off — which is also the
# shape every recording takes when a recorder starts tagging per-test tier
# properly, so these assertions are the ones that will outlive the compat rule.
#
# WHY THE MISSING ROW IS DETERMINISTIC, not a timing race: each test makes one
# call and its mapping lists two identical recordings of it, so exactly one is
# never consumed however the windows fall. And the EXPECTATION is read from
# mappings.yaml, not from the window-filtered set, so even a mock dropped by
# strict windowing is still expected and still reported unconsumed. "Not
# consumed" here is forced by construction.
# ---------------------------------------------------------------------------

# 9. THE CHECKED BRANCH, DEFAULT MODE. Same recording, per-test tier in force:
#    the assertion now runs, finds the mutated mock was never consumed, and says
#    so. `dependencies_checked` must be true, and — this is the invariant step 4
#    asserts from the other direction — consuming none of an eligible set means
#    every one of them is MISSING, so `effects` cannot be empty.
write_config
rm -rf ./knob-run
mkdir ./knob-run
cp -a ./pristine-run/keploy ./knob-run/keploy
# One file at a time: yq -i edits the first of several files only.
for tc_file in ./knob-run/keploy/test-set-*/tests/*.yaml; do
    yq -i '.spec.resp.status_code = 299' "$tc_file"
done
if grep -L 'status_code: 299' ./knob-run/keploy/test-set-*/tests/*.yaml | grep -q .; then
    echo "::error::phase 3's copy of the recording kept a test's recorded status code; its answer would not fail"
    exit 1
fi
for set_dir in ./knob-run/keploy/test-set-*; do
    [ -f "$set_dir/mappings.yaml" ] || continue
    twins=$(yq ea 'select(.kind == "Http") | .name += "-twin"' "$set_dir/mocks.yaml")
    [ -n "$twins" ] && printf -- '---\n%s\n' "$twins" >> "$set_dir/mocks.yaml"
    yq -i '.tests[].mock_entries |= (. + (map(select(.kind == "Http")) | map(.name = .name + "-twin")))' "$set_dir/mappings.yaml"
done
if ! grep -q -- '-twin' ./knob-run/keploy/test-set-*/mappings.yaml; then
    echo "::error::no test's mapping lists a twin of its call; phase 3's copy of the recording was not built"
    exit 1
fi
KEPLOY_STRICT_MOCK_WINDOW=1 "$REPLAY_BIN" test -c "./http-pokeapi" --path ./knob-run --delay 7 --generateGithubActions=false 2>&1 | tee strict_logs.txt
strict_rc=${PIPESTATUS[0]}

if grep "WARNING: DATA RACE" strict_logs.txt; then
    echo "::error::Race condition detected in the per-test-tier run"
    exit 1
fi

"$REPLAY_BIN" report --path ./knob-run --format json > ndjson_strict.txt 2> ndjson_strict_err.txt
if [ ! -s ndjson_strict.txt ]; then
    echo "::error::keploy report --format json produced no NDJSON after the per-test-tier run"
    cat ndjson_strict_err.txt
    exit 1
fi
# The baseline's set verdicts, read now: step 10 removes its reports.
shopt -s nullglob
strict_reports=( ./knob-run/keploy/reports/test-run-*/test-set-*-report.yaml )
shopt -u nullglob
if [ ${#strict_reports[@]} -eq 0 ]; then
    echo "::error::the per-test-tier run wrote no report"
    exit 1
fi
strict_set_statuses=$(yq -N '.status' "${strict_reports[@]}" | sort -u | tr '\n' ' ')
strict_set_reasons=$(yq -N '.failure_reason // ""' "${strict_reports[@]}")

# 9pre. THE PRECONDITION FOR `all(...)`, diagnosed separately so 9a's failure
#       can only mean one thing.
#
#       Steps 9a/9b quantify over EVERY verdict, so they require every recorded
#       test to have at least one eligible mapped dependency. That holds for
#       this sample today (the recording maps one mock per test), but the
#       mapping is produced from LIVE pokeapi.co calls inside a readiness loop:
#       a test recorded without an outgoing call — an upstream hiccup, a
#       short-circuited handler — gives hasExpectedMocks=false and hence
#       dependencies_checked=false for that one test. 9a would then fail with a
#       message blaming the DeriveLifetime rule-5 gate or the recorder's tag,
#       neither of which moved, and the natural "fix" a maintainer reaches for
#       is to weaken `all` to `any`. Assert the thin-recording cause HERE
#       instead, with its own wording.
missing_mapping=false
for tcid in $(jq -r '.test_case_id' ndjson_strict.txt); do
    found=false
    for mp in ./knob-run/keploy/test-set-*/mappings.yaml; do
        [ -e "$mp" ] || continue
        # Each mapping entry is `- id: <test>` followed by its mock_entry list;
        # awk keeps the two bound to the same entry (independent greps could
        # match a name under a different test).
        # Exact id comparison, not a regex match: "test-1" must not be
        # satisfied by "test-10". Written without gawk's \< \> word
        # boundaries because CI runners default to mawk, where those are not
        # supported and the match would silently never fire.
        if awk -v want="$tcid" '
            /^[[:space:]]*-[[:space:]]*id:/ {
                v = $0
                sub(/^[^:]*:[[:space:]]*/, "", v)
                gsub(/["\r ]/, "", v)
                inTest = (v == want)
            }
            /^[[:space:]]*-[[:space:]]+name:/ { if (inTest) { found = 1 } }
            END { exit found ? 0 : 1 }
        ' "$mp"; then
            found=true
            break
        fi
    done
    if [ "$found" != true ]; then
        echo "::error::recorded test '$tcid' has no mock entry in any mappings.yaml. The recording is THIN — that test made no outgoing call when it was recorded (upstream hiccup, or a short-circuited handler), so hasExpectedMocks is false and its dependency verdict is legitimately not-checked. This is NOT a tier-gate regression; re-record the sample. Steps 9a/9b quantify over every verdict and cannot run against a thin recording."
        missing_mapping=true
    fi
done
if [ "$missing_mapping" = true ]; then
    for mp in ./knob-run/keploy/test-set-*/mappings.yaml; do echo "--- $mp ---"; cat "$mp"; done
    exit 1
fi
echo "✓ every recorded test maps at least one dependency, so 'all(...)' below is a real quantifier"

# 9a. The tier flip must actually have happened. If this fails, the rule-5 gate
#     or the recorder's tag changed and the WHOLE phase is asserting nothing —
#     so it is checked before anything downstream of it. The thin-recording
#     cause is ruled out by 9pre above, so this message names the real one.
if ! jq -es 'length > 0 and all(.dependencies_checked == true)' ndjson_strict.txt > /dev/null; then
    echo "::error::with KEPLOY_STRICT_MOCK_WINDOW=1 the recorded HTTP mocks should resolve to PER-TEST tier (DeriveLifetime rule 5 is gated off, so the recorder's 'HTTP_CLIENT' tag is honoured) and the assertion should run. It reported not-checked instead: either the recorder's metadata tag changed, or the lax-fallback gate no longer keys off KEPLOY_STRICT_MOCK_WINDOW."
    jq -s 'map({test_case_id, status, dependencies_checked, dependencies_consumed, effects:(.effects//[]|length)})' ndjson_strict.txt
    exit 1
fi

# 9b. ...and the assertion found the mutated dependency missing. This is the
#     one path the not-checked phase can never reach: a real MISSING row, with
#     matched=false, which is what a consumer's
#     `checked && any(matched == false)` rule keys off.
if ! jq -es 'length > 0 and all((.effects // []) | map(select(.matched == false)) | length > 0)' ndjson_strict.txt > /dev/null; then
    echo "::error::the dependency assertion ran and reported no unexercised dependency. Each test's mapping lists two identical recordings of its one call, so one of them MUST come back unconsumed."
    jq -s 'map({test_case_id, dependencies_checked, dependencies_consumed, effects})' ndjson_strict.txt
    exit 1
fi
echo "✓ with per-test-tier dependencies the assertion RAN and reported the vanished dependency"

# 10. The persisted report carries the same thing: the checked bit, and the
#     MISSING row as a row. `expected: consumed / actual: not consumed` is the
#     literal shape the writer emits for a dependency that vanished.
shopt -s nullglob
strict_reports=( ./knob-run/keploy/reports/test-run-*/test-set-*-report.yaml )
shopt -u nullglob
if [ ${#strict_reports[@]} -eq 0 ]; then
    echo "::error::the per-test-tier run wrote no test-set report"
    exit 1
fi
strict_rows=false
for rf in "${strict_reports[@]}"; do
    if grep -q 'deps_checked: true' "$rf" && grep -q 'actual: not consumed' "$rf"; then
        strict_rows=true
    fi
    # The report-size contract from step 5c holds on THIS branch too, and here
    # it is a much stronger statement: the assertion really ran, so a writer
    # that persisted the consumed side as rows would have had the chance to.
    if grep -q 'actual: consumed$' "$rf"; then
        echo "::error::$(basename "$rf") persisted a MATCHED dependency row. The consumed side must be the deps_consumed count, never rows."
        grep -n -B4 'actual: consumed$' "$rf" | head -30
        exit 1
    fi
done
if [ "$strict_rows" != true ]; then
    echo "::error::no report carried deps_checked: true together with a MISSING dependency row, although the NDJSON projection of the same run did. The persisted report and the projection are supposed to be two views of one result."
    for rf in "${strict_reports[@]}"; do echo "--- $rf ---"; grep -n -A12 'dep_result:' "$rf" | head -30; done
    exit 1
fi
echo "✓ the persisted report carries deps_checked: true and the MISSING dependency row"

# 11. THE KNOB'S ONE JOB, on the branch where it can do it. Phase 2 proved
#     --assert-dependencies cannot redden a run it cannot assert. The complement
#     — that it DOES redden a run whose assertion found a lost dependency — has
#     no other coverage in this suite, and without it the flag could be a no-op
#     in every mode and still pass.
#
#     ASSERTED AS A REAL DELTA against step 9's identical run: same binary, same
#     environment, same mocks, only the flag differs.
#
#     What is NOT used as that delta, because it is knob-INDEPENDENT: the
#     DEPENDENCY_MISSING label. attachDepResults appends it to the failure
#     categories of every FAILED or OBSOLETE test that has missing rows, without
#     consulting assertDependencies at all, so the knob-OFF run at step 9
#     produces exactly the same label. Measured on this sample's knob-off strict
#     run: all three verdicts carry "failure_categories":["DEPENDENCY_MISSING"]
#     with status OBSOLETE. Grepping for it therefore passes even if the flag is
#     a total no-op, which is the kind of assertion this PR exists to delete.
#
#     The two things that ARE knob-specific, and are what this step asserts:
#       * THE STATUS. resolveTestOutcome sends the same (mockSetMismatch,
#         response-failed) state to mismatchLogObsolete when the knob is off
#         (OBSOLETE, failsTestSet=false) and to mismatchLogVetoedFailure when it
#         is on (FAILED, failsTestSet=true). That is a deterministic branch on
#         the flag, not a timing-dependent outcome.
#       * THE SET VERDICT, as a two-sided delta. A demoted wrong answer is not
#         judged, so the baseline's sets are INTERNAL_ERR for the stale mapping
#         alone (their reason says so) and the run exits non-zero; the knob
#         run's sets are FAILED. The exit code no longer tells the two apart
#         (neither run is a pass), so the delta is read from the set reports:
#         a baseline that is red for any other reason (a pokeapi drift; this
#         suite mutates mocks on purpose) has another status or reason, and
#         fails here.
rm -rf ./knob-run/keploy/reports
KEPLOY_STRICT_MOCK_WINDOW=1 "$REPLAY_BIN" test -c "./http-pokeapi" --path ./knob-run --delay 7 --assert-dependencies --generateGithubActions=false 2>&1 | tee strict_assert_logs.txt
strict_assert_rc=${PIPESTATUS[0]}

# 11pre. THE NEGATIVE CONTROL FOR STEP 7a, on the only run where it can fire.
#        7a asserts the session-tier inert warning appears in phase 2; that is
#        only meaningful if the warning is not printed unconditionally. This is
#        the run that proves it: --assert-dependencies IS on (so
#        warnDependencyAssertionInert does not return at its knob guard) and the
#        dependencies ARE eligible and WERE asserted, so the reason must be "".
#
#        It deliberately reads strict_assert_logs.txt and NOT the step-9 log: the
#        step-9 run passes no --assert-dependencies, and the warner returns on
#        its first line when the knob is off, so the string is absent there
#        whatever the reason logic does. An earlier version of this suite grepped
#        that log and asserted nothing at all — mutating
#        dependencyAssertionInertReason to return the tier reason unconditionally
#        left it green.
if grep -q 'no eligible dependency to assert' strict_assert_logs.txt; then
    echo "::error::the no-eligible-dependency warning fired for a run with --assert-dependencies ON whose dependencies WERE eligible and WERE asserted (step 9 just proved the assertion ran and found a missing dependency on this exact recording). The reason is supposed to describe a state, not to be printed unconditionally — and step 7a's matching assertion is only meaningful if this one holds."
    grep -n 'eligible' strict_assert_logs.txt | head -10
    exit 1
fi
echo "✓ the inert warning stays silent when the assertion can actually run"

shopt -s nullglob
strict_assert_reports=( ./knob-run/keploy/reports/test-run-*/test-set-*-report.yaml )
shopt -u nullglob
if [ ${#strict_assert_reports[@]} -eq 0 ]; then
    echo "::error::the per-test-tier --assert-dependencies run wrote no report"
    exit 1
fi

"$REPLAY_BIN" report --path ./knob-run --format json > ndjson_strict_assert.txt 2> ndjson_strict_assert_err.txt
if [ ! -s ndjson_strict_assert.txt ]; then
    echo "::error::keploy report --format json produced no NDJSON after the per-test-tier --assert-dependencies run"
    cat ndjson_strict_assert_err.txt
    exit 1
fi

# 11a. THE STATUS DELTA. Knob off -> OBSOLETE and not judged (a demoted wrong
#      answer: verdict_unknown, never a pass); knob on -> FAILED. Same
#      recording, same mocks, same environment.
if ! jq -es 'length > 0 and all(.status == "OBSOLETE" and .verdict_unknown == true)' ndjson_strict.txt > /dev/null; then
    echo "::error::the BASELINE run (step 9, per-test tier, NO --assert-dependencies) did not demote its tests to OBSOLETE and leave them not judged (verdict_unknown), so there is no delta for --assert-dependencies to be measured against. A mock-set mismatch with a failing response is demoted, not failed, until a veto flag says otherwise, and a demoted wrong answer has no verdict."
    jq -s 'map({test_case_id, status, verdict_unknown, verdict_unknown_reason, dependencies_checked, failure_categories})' ndjson_strict.txt
    exit 1
fi
if ! jq -es 'length > 0 and all(.status == "FAILED")' ndjson_strict_assert.txt > /dev/null; then
    echo "::error::--assert-dependencies did not turn the demoted tests into FAILED ones. Step 9 proved the assertion RAN on this recording and reported a missing dependency, and the baseline demoted those same tests to OBSOLETE; the flag's entire job is to veto that demotion. A DEPENDENCY_MISSING label alone is NOT evidence the flag worked — attachDepResults writes it on the knob-off run too."
    jq -s 'map({test_case_id, status, dependencies_checked, failure_categories})' ndjson_strict_assert.txt
    exit 1
fi

# 11b. THE SET-VERDICT DELTA, two-sided. The baseline's sets are not judged,
#      for the stale mapping alone, and its run is red (a demoted wrong answer
#      is never a pass); the knob run's sets are FAILED, and it is red.
strict_assert_set_statuses=$(yq -N '.status' "${strict_assert_reports[@]}" | sort -u | tr '\n' ' ')
if [ "$strict_set_statuses" != "INTERNAL_ERR " ] || ! grep -q 'stale mapping' <<<"$strict_set_reasons" || [ "$strict_rc" -eq 0 ]; then
    echo "::error::the baseline (step 9, same recording, same env, no knob) must leave every set not judged for the stale mapping alone (INTERNAL_ERR, its reason naming the stale mapping and the remedy) and exit non-zero: its sets are '$strict_set_statuses', it exited $strict_rc. A baseline that is red for another reason means this step could pass with the flag doing nothing."
    printf '%s\n' "$strict_set_reasons"
    echo "--- tail of baseline (strict_logs.txt) ---"
    tail -20 strict_logs.txt
    exit 1
fi
if [ "$strict_assert_set_statuses" != "FAILED " ] || [ "$strict_assert_rc" -eq 0 ]; then
    echo "::error::--assert-dependencies must turn the not-judged sets into FAILED ones: its sets are '$strict_assert_set_statuses' and it exited $strict_assert_rc (must be non-zero)."
    echo "--- tail of knob run (strict_assert_logs.txt) ---"
    tail -40 strict_assert_logs.txt
    exit 1
fi
echo "✓ --assert-dependencies vetoed the OBSOLETE demotion: not judged (INTERNAL_ERR, exit $strict_rc) -> FAILED (exit $strict_assert_rc)"

# 11c. And the flag still did not invent the assertion: it changed the STATUS a
#      missing dependency produces, not whether the check ran. Step 8 asserts
#      this on the not-checked branch; here it is asserted where the bit is
#      true, so "mode-independent parsing" is pinned from both sides.
#      ndjson_strict_assert.txt was already generated for the status delta above.
if ! jq -es 'length > 0 and all(.dependencies_checked == true and ((.effects // []) | map(select(.matched == false)) | length > 0))' ndjson_strict_assert.txt > /dev/null; then
    echo "::error::the knob run's projection disagrees with the default run's on the same recording. --assert-dependencies decides what a missing dependency does to the status; the dependency data itself must be identical."
    jq -s 'map({test_case_id, status, dependencies_checked, dependencies_consumed, effects:(.effects//[]|length)})' ndjson_strict_assert.txt
    exit 1
fi
echo "✓ the knob changed the status, not the dependency data"

# ---------------------------------------------------------------------------
# PHASE 4 — THE PROVEN SWITCH (KEPLOY_PROVEN=1, the proven model's staging
# gate). A test fails when one of its calls got no recorded answer or when it
# did not make a call it recorded, and which calls a test owns is what its
# mapping lists (every data-plane mock), not DeriveLifetime's tier. So the
# recording's HTTP_CLIENT mocks, session-tier and never checked in phases 1-2,
# are checked here WITHOUT KEPLOY_STRICT_MOCK_WINDOW.
# ---------------------------------------------------------------------------

# 12a. The mutated recording under the switch. Step 2 proved every verdict is
#      NOT CHECKED without it; under it every test owns its pokeapi mock, did
#      not consume it (the path was rewritten), reports the miss, and fails.
rm -rf ./keploy/reports
KEPLOY_PROVEN=1 "$REPLAY_BIN" test -c "./http-pokeapi" --delay 7 --generateGithubActions=false 2>&1 | tee proven_logs.txt
proven_rc=${PIPESTATUS[0]}
if grep "WARNING: DATA RACE" proven_logs.txt; then
    echo "::error::Race condition detected in the proven-switch run"
    exit 1
fi
"$REPLAY_BIN" report --format json > ndjson_proven.txt 2> ndjson_proven_err.txt
if ! jq -es 'length > 0 and all(.status == "FAILED" and .dependencies_checked == true and ((.effects // []) | map(select(.matched == false)) | length > 0))' ndjson_proven.txt > /dev/null; then
    echo "::error::under KEPLOY_PROVEN=1 every test of the mutated recording must own its recorded pokeapi call (membership, not tier: no KEPLOY_STRICT_MOCK_WINDOW here), report it unconsumed, and fail. Step 2 showed the same recording NOT CHECKED without the switch."
    jq -s 'map({test_case_id, status, dependencies_checked, effects: (.effects // [] | length)})' ndjson_proven.txt
    exit 1
fi
shopt -s nullglob
proven_reports=( ./keploy/reports/test-run-*/test-set-*-report.yaml )
shopt -u nullglob
proven_misses=false
for rf in "${proven_reports[@]}"; do
    if grep -q 'unmatched_calls:' "$rf"; then
        proven_misses=true
    fi
done
if [ "$proven_misses" != true ] || [ "$proven_rc" -eq 0 ]; then
    echo "::error::under KEPLOY_PROVEN=1 the run must report the calls that got no recorded answer (unmatched_calls) and exit non-zero; reported=$proven_misses exit=$proven_rc"
    tail -40 proven_logs.txt
    exit 1
fi
echo "✓ KEPLOY_PROVEN=1: every test owned its recorded call, the miss was reported, and the run failed (exit $proven_rc)"

# 12b. THE CONTROL: the recording as recorded, under the switch. Every test
#      makes every call it recorded and gets every answer, so every test passes
#      and the run exits 0. This is what keeps 12a and 12c honest: DNS and
#      protocol-phase mocks are not owned, and a consumed session-tier mock
#      reads as consumed.
KEPLOY_PROVEN=1 "$REPLAY_BIN" test -c "./http-pokeapi" --path ./pristine-run --delay 7 --generateGithubActions=false 2>&1 | tee proven_pristine_logs.txt
proven_pristine_rc=${PIPESTATUS[0]}
if grep "WARNING: DATA RACE" proven_pristine_logs.txt; then
    echo "::error::Race condition detected in the proven-switch control run"
    exit 1
fi
"$REPLAY_BIN" report --format json --path ./pristine-run > ndjson_proven_pristine.txt 2> ndjson_proven_pristine_err.txt
if [ "$proven_pristine_rc" -ne 0 ] || ! jq -es 'length > 0 and all(.status == "PASSED")' ndjson_proven_pristine.txt > /dev/null; then
    echo "::error::under KEPLOY_PROVEN=1 the unmutated recording must replay green (exit $proven_pristine_rc). A red test here is a false red: a mock its test did consume read as unconsumed, or a protocol-phase/DNS mock was required of a test."
    jq -s 'map({test_case_id, status, dependencies_checked, effects})' ndjson_proven_pristine.txt
    tail -40 proven_pristine_logs.txt
    exit 1
fi
echo "✓ KEPLOY_PROVEN=1: the unmutated recording replays green"

# 12c. A RECORDED CALL THE TEST NO LONGER MAKES, AND NOTHING ELSE. In a copy of
#      the unmutated recording, the first /api/locations test is made to own
#      the pokeapi call of the /api/locations/<location> test too: a call it
#      never makes. Its response still matches. Without the switch it passes
#      (that mock is session-tier and filtered out); with it, it fails, and
#      every other test still passes.
rm -rf ./unconsumed-run
mkdir ./unconsumed-run
cp -a ./pristine-run/keploy ./unconsumed-run/keploy
rm -rf ./unconsumed-run/keploy/reports
uc_mappings=$(ls ./unconsumed-run/keploy/test-set-*/mappings.yaml | head -n 1)
uc_tests_dir="$(dirname "$uc_mappings")/tests"
owner=""
lender=""
for tc in "$uc_tests_dir"/*.yaml; do
    url=$(yq '.spec.req.url' "$tc")
    name=$(yq '.name' "$tc")
    case "$url" in
        */api/locations) [ -z "$owner" ] && owner="$name" ;;
        */api/locations/*) [ -z "$lender" ] && lender="$name" ;;
    esac
done
lent_mock=$(LENDER="$lender" yq '.tests[] | select(.id == strenv(LENDER)) | .mock_entries[] | select(.kind != "DNS") | .name' "$uc_mappings" | head -n 1)
if [ -z "$owner" ] || [ -z "$lender" ] || [ -z "$lent_mock" ]; then
    echo "::error::could not find a /api/locations test ($owner), a /api/locations/<location> test ($lender) and the latter's mapped pokeapi mock ($lent_mock) in $uc_mappings"
    cat "$uc_mappings"
    exit 1
fi
OWNER="$owner" LENDER="$lender" yq -i '(.tests[] | select(.id == strenv(OWNER)) | .mock_entries) += [(.tests[] | select(.id == strenv(LENDER)) | .mock_entries[] | select(.kind != "DNS"))]' "$uc_mappings"
echo "made $owner own $lender's recorded call $lent_mock"

for phase in off on; do
    rm -rf ./unconsumed-run/keploy/reports
    if [ "$phase" = on ]; then
        KEPLOY_PROVEN=1 "$REPLAY_BIN" test -c "./http-pokeapi" --path ./unconsumed-run --delay 7 --generateGithubActions=false 2>&1 | tee "unconsumed_${phase}_logs.txt"
    else
        "$REPLAY_BIN" test -c "./http-pokeapi" --path ./unconsumed-run --delay 7 --generateGithubActions=false 2>&1 | tee "unconsumed_${phase}_logs.txt"
    fi
    uc_rc=${PIPESTATUS[0]}
    if grep "WARNING: DATA RACE" "unconsumed_${phase}_logs.txt"; then
        echo "::error::Race condition detected in the unconsumed-mock run (switch $phase)"
        exit 1
    fi
    "$REPLAY_BIN" report --format json --path ./unconsumed-run > "ndjson_unconsumed_${phase}.txt" 2>/dev/null
    if [ "$phase" = off ]; then
        if [ "$uc_rc" -ne 0 ] || ! jq -es 'length > 0 and all(.status == "PASSED")' "ndjson_unconsumed_${phase}.txt" > /dev/null; then
            echo "::error::control: without the switch the extra session-tier mock is not checked, so every test must pass (exit $uc_rc)"
            jq -s 'map({test_case_id, status})' "ndjson_unconsumed_${phase}.txt"
            exit 1
        fi
        continue
    fi
    if ! OWNER="$owner" jq -es 'length > 0 and all(if .test_case_id == env.OWNER then .status == "FAILED" and ((.effects // []) | map(select(.matched == false)) | length > 0) else .status == "PASSED" end)' "ndjson_unconsumed_${phase}.txt" > /dev/null; then
        echo "::error::under KEPLOY_PROVEN=1, $owner, which owns a recorded call it never makes, must fail with that call reported unconsumed, and every other test must pass"
        jq -s 'map({test_case_id, status, effects})' "ndjson_unconsumed_${phase}.txt"
        exit 1
    fi
    if [ "$uc_rc" -eq 0 ]; then
        echo "::error::under KEPLOY_PROVEN=1 a failed test must make the run exit non-zero"
        exit 1
    fi
    # The evidence names the mock (failure_info.unconsumed_mocks), not only a
    # display row: what a judge builds unconsumed_mock evidence from.
    uc_report=$(ls -1t ./unconsumed-run/keploy/reports/test-run-*/test-set-*-report.yaml | head -n 1)
    if ! OWNER="$owner" yq '.tests[] | select(.test_case_id == strenv(OWNER)) | .failure_info.unconsumed_mocks[].name' "$uc_report" | grep -qx "$lent_mock"; then
        echo "::error::under KEPLOY_PROVEN=1 $owner's report row must name the recorded call it did not make ($lent_mock) under failure_info.unconsumed_mocks"
        OWNER="$owner" yq '.tests[] | select(.test_case_id == strenv(OWNER)) | .failure_info' "$uc_report"
        exit 1
    fi
done
echo "✓ KEPLOY_PROVEN=1: a test that no longer makes a recorded call fails, with the call named, and nothing else does"

# 13. STRICT FAILURE UNDER THE PROVEN SWITCH (KEPLOY_PROVEN=1, the proven
#     model's staging gate). Step 9's run, unchanged except for the switch: no
#     --assert-dependencies and no --strict-failure. Step 11a/11b proved that
#     baseline demotes every test to OBSOLETE, not judged. Under the switch
#     strict failure is on for everyone, so a test whose response differs is
#     FAILED whatever its mocks did, and the run is red: the switch is the only
#     difference, so it is the only reason.
rm -rf ./keploy/reports
KEPLOY_STRICT_MOCK_WINDOW=1 KEPLOY_PROVEN=1 "$REPLAY_BIN" test -c "./http-pokeapi" --delay 7 --generateGithubActions=false 2>&1 | tee proven_strict_logs.txt
proven_strict_rc=${PIPESTATUS[0]}
if grep "WARNING: DATA RACE" proven_strict_logs.txt; then
    echo "::error::Race condition detected in the proven-switch run"
    exit 1
fi
"$REPLAY_BIN" report --format json > ndjson_proven_strict.txt 2> ndjson_proven_strict_err.txt
if ! jq -es 'length > 0 and all(.status == "FAILED")' ndjson_proven_strict.txt > /dev/null; then
    echo "::error::under KEPLOY_PROVEN=1 a test whose response differs while its mock set diverged must be FAILED (strict failure is always on), not demoted to OBSOLETE as step 9's identical run without the switch demoted it."
    jq -s 'map({test_case_id, status})' ndjson_proven_strict.txt
    exit 1
fi
if [ "$proven_strict_rc" -eq 0 ]; then
    echo "::error::under KEPLOY_PROVEN=1 the run exited 0 with every test FAILED; step 9's identical run without the switch exited $strict_rc"
    tail -40 proven_strict_logs.txt
    exit 1
fi
echo "✓ KEPLOY_PROVEN=1 vetoed the OBSOLETE demotion: every test FAILED and the run exited $proven_strict_rc (step 9 without the switch: OBSOLETE and not judged, exit $strict_rc)"

# 14. STRICT HTTP MATCHING UNDER THE PROVEN SWITCH (design v14 row S1). Step
#     12b is the control: the recording as recorded replays green under the
#     switch, so nothing below is a false red of the strict rules. Each case
#     changes ONE compared field of the recorded pokeapi calls, in a copy of
#     the unmutated recording, and checks both sides of the switch: without
#     it the field is not compared and every test passes, with it the calls
#     get no recorded answer, the report names the field, and the run is red.
#
# s1_case <name> <yq update of the recorded HTTP request> <a line the update
# writes, grep -F> <field the report must name>
s1_case() {
    local name="$1" update="$2" written="$3" field="$4"
    local dir="./s1-${name}-run"
    rm -rf "$dir"
    mkdir "$dir"
    cp -a ./pristine-run/keploy "$dir/keploy"
    rm -rf "$dir/keploy/reports"
    local http changed
    for mf in "$dir"/keploy/test-set-*/mocks.yaml; do
        yq -i "(select(.kind == \"Http\") | .spec.req) |= ($update)" "$mf"
    done
    # yq rewrites a file whether or not its select matched: count what the
    # update wrote, one per recorded HTTP call.
    http=$(cat "$dir"/keploy/test-set-*/mocks.yaml | grep -c '^kind: Http$')
    changed=$(cat "$dir"/keploy/test-set-*/mocks.yaml | grep -cF -- "$written")
    if [ "$http" -eq 0 ] || [ "$changed" -ne "$http" ]; then
        echo "::error::S1 case $name wrote '$written' on $changed of $http recorded HTTP calls"
        exit 1
    fi
    for phase in off on; do
        rm -rf "$dir/keploy/reports"
        if [ "$phase" = on ]; then
            KEPLOY_PROVEN=1 "$REPLAY_BIN" test -c "./http-pokeapi" --path "$dir" --delay 7 --generateGithubActions=false 2>&1 | tee "s1_${name}_${phase}_logs.txt"
        else
            "$REPLAY_BIN" test -c "./http-pokeapi" --path "$dir" --delay 7 --generateGithubActions=false 2>&1 | tee "s1_${name}_${phase}_logs.txt"
        fi
        rc=${PIPESTATUS[0]}
        if grep "WARNING: DATA RACE" "s1_${name}_${phase}_logs.txt"; then
            echo "::error::Race condition detected in S1 case $name (switch $phase)"
            exit 1
        fi
        "$REPLAY_BIN" report --format json --path "$dir" > "ndjson_s1_${name}_${phase}.txt" 2>/dev/null
        if [ "$phase" = off ]; then
            if [ "$rc" -ne 0 ] || ! jq -es 'length > 0 and all(.status == "PASSED")' "ndjson_s1_${name}_${phase}.txt" > /dev/null; then
                echo "::error::S1 case $name, control: without the switch the changed field is not compared, so every test must pass (exit $rc)"
                jq -s 'map({test_case_id, status})' "ndjson_s1_${name}_${phase}.txt"
                exit 1
            fi
            continue
        fi
        # Every test owns a pokeapi call (step 12a), so every test misses.
        if [ "$rc" -eq 0 ] || ! jq -es 'length > 0 and all(.status == "FAILED")' "ndjson_s1_${name}_${phase}.txt" > /dev/null; then
            echo "::error::S1 case $name: under KEPLOY_PROVEN=1 a call whose recorded $field differs must get no recorded answer and fail its test (exit $rc)"
            jq -s 'map({test_case_id, status})' "ndjson_s1_${name}_${phase}.txt"
            exit 1
        fi
        report=$(ls -1t "$dir"/keploy/reports/test-run-*/test-set-*-report.yaml | head -n 1)
        if ! FIELD="$field" yq -e '[.tests[].failure_info.unmatched_calls[]?.field_diffs[]?.path] | any_c(. == strenv(FIELD))' "$report" > /dev/null; then
            echo "::error::S1 case $name: under KEPLOY_PROVEN=1 the report must name $field as what differs in the call that got no recorded answer"
            yq '.tests[].failure_info.unmatched_calls' "$report"
            exit 1
        fi
    done
    echo "✓ S1 $name: compared under KEPLOY_PROVEN=1 (the call missed, the report names $field), not compared without it"
}

# 14a. The host: the same request to another authority is another dependency.
s1_case host '.header.Host = "pokeapi.invalid"' 'Host: pokeapi.invalid' host

# 14b. A header value: another client (the User-Agent's product) is another
#      caller.
s1_case user-agent '.header["User-Agent"] = "curl/8.4.0"' 'User-Agent: curl/8.4.0' header.user-agent

# 14c. A credential (design v14 §7.2 item 8): without the switch Authorization
#      is on the static noise list, so a recording made with a token the app
#      does not send still answers; with it, the credential is compared.
s1_case credential '.header.Authorization = "Bearer recorded-token"' 'Authorization: Bearer recorded-token' header.authorization

# 14d. NOT a difference: the same client in another version. The User-Agent
#      is compared by its first product token's name, so this recording still
#      answers under the switch (a false red here would fail every upgrade).
rm -rf ./s1-ua-version-run
mkdir ./s1-ua-version-run
cp -a ./pristine-run/keploy ./s1-ua-version-run/keploy
rm -rf ./s1-ua-version-run/keploy/reports
for mf in ./s1-ua-version-run/keploy/test-set-*/mocks.yaml; do
    yq -i '(select(.kind == "Http") | .spec.req.header["User-Agent"]) |= sub("/.*$"; "/99.0")' "$mf"
done
if ! grep -q 'User-Agent: .*/99.0' ./s1-ua-version-run/keploy/test-set-*/mocks.yaml; then
    echo "::error::S1 case ua-version changed no recorded User-Agent"
    exit 1
fi
KEPLOY_PROVEN=1 "$REPLAY_BIN" test -c "./http-pokeapi" --path ./s1-ua-version-run --delay 7 --generateGithubActions=false 2>&1 | tee s1_ua_version_logs.txt
ua_rc=${PIPESTATUS[0]}
if grep "WARNING: DATA RACE" s1_ua_version_logs.txt; then
    echo "::error::Race condition detected in S1 case ua-version"
    exit 1
fi
"$REPLAY_BIN" report --format json --path ./s1-ua-version-run > ndjson_s1_ua_version.txt 2>/dev/null
if [ "$ua_rc" -ne 0 ] || ! jq -es 'length > 0 and all(.status == "PASSED")' ndjson_s1_ua_version.txt > /dev/null; then
    echo "::error::S1 case ua-version: under KEPLOY_PROVEN=1 a recording made by another version of the same client must still answer (exit $ua_rc)"
    jq -s 'map({test_case_id, status})' ndjson_s1_ua_version.txt
    exit 1
fi
echo "✓ S1 ua-version: another version of the same client still matches under KEPLOY_PROVEN=1"

echo "mock-mismatch e2e passed:"
echo "  - the forced HTTP unmatched call was reported, its differing path segment withheld;"
echo "  - with the tier as an ordinary recording leaves it, every dependency is"
echo "    session-tier, so the per-test assertion reported NOT CHECKED, said WHY,"
echo "    and --assert-dependencies failed nothing it could not assert;"
echo "  - with the per-test tier in force (KEPLOY_STRICT_MOCK_WINDOW=1) the same"
echo "    recording drove the other branch: the assertion RAN and reported the"
echo "    vanished dependency as MISSING, the inert warning stayed silent, and"
echo "    the demoted wrong answers were not judged (OBSOLETE, verdict_unknown, set"
echo "    INTERNAL_ERR for the stale mapping, run red), and --assert-dependencies"
echo "    moved them to FAILED (set FAILED), which no knob-independent label can"
echo "    fake;"
echo "  - under KEPLOY_PROVEN=1 a test owns every data-plane mock its mapping"
echo "    lists: the unmutated recording replays green, and a test fails when it"
echo "    does not make a call it recorded or a call got no recorded answer."
echo "  - under KEPLOY_PROVEN=1 strict failure is on for everyone: the same run"
echo "    FAILED its tests instead of demoting them to OBSOLETE, and went red."
echo "  - under KEPLOY_PROVEN=1 HTTP matching is strict (S1): a recorded call to"
echo "    another host, from another client or with a credential the app does not"
echo "    send answers nothing, and the report names the field; another version"
echo "    of the same client still answers."
exit 0
