#!/usr/bin/env bash
# Tests for gin_mongo/golang-docker.sh, the lane "gin-mongo" of
# golang_docker.yml. Needs bash, coreutils, awk, grep and jq; no Docker, no
# keploy, no network, no sudo. Run by linux-ci-scripts.yml, or locally:
#
#   bash .github/workflows/test_workflow_scripts/golang/gin_mongo/golang-docker.tests.sh
#
# Each case runs the lane script as its workflow step does: sourced by
# `bash -e`, from samples-go/gin-mongo of a scratch workspace, with fakes on
# PATH for docker, curl, sleep, pgrep and sudo, and one fake keploy as both
# RECORD_BIN and REPLAY_BIN. The fake's keploy record exits with the code a
# case gives it; its keploy test writes the test-set reports a case gives it
# into a new test-run directory, and exits with the code a case gives it.
#
# Under `bash -e` a keploy command that exits non-zero ended the step at that
# line, before the script's checks could print its log: the step failed with
# no line saying what had failed (a keploy test that failed a test set, in a
# run whose record had answered one request with an error). Every keploy
# command's exit has to be named, with the reports and the command's log.
#
# every command succeeds, every report passes
#   The script has to end 0, past its last check.
# keploy test (the first replay) fails test-set-1's test-5 and exits 1
#   The script has to exit 1, name keploy test and its exit, the report that
#   failed and its failed test (not the tests that passed, before it and
#   after it), and print keploy test's log; and name no report the run did
#   not write.
# keploy record of iteration 2 exits 2
#   The script has to exit 2, naming that record, and print its log.
# keploy test --storage-format json fails test-set-3's test-2 and exits 1
#   The script has to exit 1, naming that replay, its failed test from the
#   json report, and print its log.
# the --keep-app-alive replay exits 3
#   The script has to exit 3 with its own message for that replay, which a
#   `replay_status=$?` after the command never reached under `bash -e`.

set -u

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/../../../../.." && pwd)"
script="$repo/.github/workflows/test_workflow_scripts/golang/gin_mongo/golang-docker.sh"
[ -f "$script" ] || { echo "no lane script at $script"; exit 1; }

root="$(mktemp -d)"
trap 'rm -rf "$root"' EXIT

fails=0

# make_fakes DIR: the PATH fakes and the fake keploy, in DIR/bin.
make_fakes() {
    local bin="$1/bin"
    mkdir -p "$bin"
    cat >"$bin/docker" <<'EOF'
#!/usr/bin/env bash
# `docker exec NAME mongosh ... --eval 'db.adminCommand({ ping: 1 }).ok'` is
# start_mongo's ping: it answers 1. Every other docker command succeeds.
case "${1:-}" in
exec) echo 1 ;;
esac
exit 0
EOF
    printf '#!/usr/bin/env bash\nexit 0\n' >"$bin/curl"
    printf '#!/usr/bin/env bash\nexit 0\n' >"$bin/sleep"
    printf '#!/usr/bin/env bash\nexit 1\n' >"$bin/pgrep"
    printf '#!/usr/bin/env bash\nexec "$@"\n' >"$bin/sudo"
    cat >"$bin/keploy" <<'EOF'
#!/usr/bin/env bash
# FAKE_DIR holds the case's counters. FAKE_JSON=1 says --storage-format in
# --help, so the script runs its json pass. FAKE_RECORD_RC_<n> is the exit of
# the nth keploy record; FAKE_TEST_RC_<n> that of the nth keploy test, and
# FAKE_TEST_<n> the reports it writes: "SET:STATUS[:FAILED_TEST]" words. A
# set's tests are test-1, passed, and with FAILED_TEST that test, failed, then
# test-9, passed.
next() {
    local f="$FAKE_DIR/$1" n
    n=$(( $(cat "$f" 2>/dev/null || echo 0) + 1 ))
    echo "$n" >"$f"
    echo "$n"
}
case "${1:-}" in
--help)
    echo "Usage: keploy [command]"
    if [ "${FAKE_JSON:-0}" = 1 ]; then echo "      --storage-format string"; fi
    exit 0
    ;;
record)
    n=$(next record)
    echo "fake keploy record $n log"
    rc_var="FAKE_RECORD_RC_$n"
    exit "${!rc_var:-0}"
    ;;
test)
    n=$(next test)
    format=yaml
    case " $* " in *" --storage-format json "*) format=json ;; esac
    run=0
    while [ -d "./keploy/reports/test-run-$run" ]; do run=$((run + 1)); done
    dir="./keploy/reports/test-run-$run"
    mkdir -p "$dir"
    sets_var="FAKE_TEST_$n"
    for word in ${!sets_var:-0:PASSED 1:PASSED}; do
        IFS=: read -r set status failed <<<"$word"
        if [ "$format" = json ]; then
            tests='[{"kind":"Http","status":"PASSED","testCaseID":"test-1"}'
            if [ -n "$failed" ]; then tests="$tests"',{"kind":"Http","status":"FAILED","testCaseID":"'"$failed"'"}'; fi
            printf '{"name":"test-set-%s","status":"%s","tests":%s]}\n' "$set" "$status" "$tests" \
                >"$dir/test-set-$set-report.json"
        else
            {
                echo "name: test-set-$set"
                echo "status: $status"
                echo "tests:"
                echo "    - kind: Http"
                echo "      name: test-set-$set"
                echo "      status: PASSED"
                echo "      test_case_id: test-1"
                if [ -n "$failed" ]; then
                    echo "    - kind: Http"
                    echo "      name: test-set-$set"
                    echo "      status: FAILED"
                    echo "      test_case_id: $failed"
                    echo "    - kind: Http"
                    echo "      name: test-set-$set"
                    echo "      status: PASSED"
                    echo "      test_case_id: test-9"
                fi
            } >"$dir/test-set-$set-report.yaml"
        fi
    done
    echo "fake keploy test $n log"
    rc_var="FAKE_TEST_RC_$n"
    exit "${!rc_var:-0}"
    ;;
esac
exit 0
EOF
    chmod +x "$bin"/*
}

# run_case NAME [VAR=VALUE...]: runs the lane script with the fakes, the
# case's settings in its environment; leaves its output in $out, its exit in
# $rc.
run_case() {
    local name="$1" dir
    shift
    dir="$root/$name"
    mkdir -p "$dir/ws/samples-go/gin-mongo" "$dir/home" "$dir/counters"
    ln -s "$repo/.github" "$dir/ws/.github"
    make_fakes "$dir"
    out="$dir/out.txt"
    rc=0
    (
        cd "$dir/ws/samples-go/gin-mongo" &&
            env "$@" PATH="$dir/bin:$PATH" HOME="$dir/home" GITHUB_WORKSPACE="$dir/ws" \
                FAKE_DIR="$dir/counters" RECORD_BIN="$dir/bin/keploy" REPLAY_BIN="$dir/bin/keploy" \
                timeout 60 bash -e -c 'source "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/golang/gin_mongo/golang-docker.sh"'
    ) >"$out" 2>&1 || rc=$?
}

# expect CASE WANT_RC LINE... [-- NOT_LINE...]: the case's exit is WANT_RC,
# its output has each LINE and no NOT_LINE (fixed strings).
expect() {
    local name="$1" want_rc="$2" line bad=0 refute=0
    shift 2
    if [ "$rc" -ne "$want_rc" ]; then
        echo "FAIL $name: exit $rc, want $want_rc"
        bad=1
    fi
    for line in "$@"; do
        if [ "$line" = -- ]; then
            refute=1
            continue
        fi
        if [ "$refute" -eq 0 ] && ! grep -qF -- "$line" "$out"; then
            echo "FAIL $name: no line with: $line"
            bad=1
        elif [ "$refute" -eq 1 ] && grep -qF -- "$line" "$out"; then
            echo "FAIL $name: a line with: $line"
            bad=1
        fi
    done
    if [ "$bad" -ne 0 ]; then
        echo "--- output of $name:"
        cat "$out"
        echo "---"
        fails=$((fails + 1))
    else
        echo "ok   $name"
    fi
}

run_case pass FAKE_JSON=1 FAKE_TEST_2="0:PASSED 1:PASSED 2:PASSED 3:PASSED"
expect pass 0 "All tests passed (yaml + json)" "All tests passed (--keep-app-alive run)"

run_case replay-fails FAKE_JSON=1 FAKE_TEST_RC_1=1 \
    FAKE_TEST_1="0:PASSED 1:PASSED 2:FAILED:test-5 3:PASSED"
expect replay-fails 1 \
    "::error::keploy test exited 1" \
    "test-run-0/test-set-2-report.yaml: FAILED" \
    "  failed: test-5" \
    "fake keploy test 1 log" \
    -- "failed: test-1" "failed: test-9" "report.json"

run_case record-fails FAKE_RECORD_RC_2=2
expect record-fails 2 \
    "::error::keploy record (iteration 2) exited 2" \
    "fake keploy record 2 log"

run_case json-replay-fails FAKE_JSON=1 FAKE_TEST_RC_2=1 \
    FAKE_TEST_2="0:PASSED 1:PASSED 2:PASSED 3:FAILED:test-2"
expect json-replay-fails 1 \
    "::error::keploy test --storage-format json exited 1" \
    "test-run-1/test-set-3-report.json: FAILED" \
    "  failed: test-2" \
    "fake keploy test 2 log"

run_case keep-app-alive-fails FAKE_TEST_RC_2=3
expect keep-app-alive-fails 3 \
    "::error::--keep-app-alive replay failed with exit code 3" \
    "fake keploy test 2 log"

if [ "$fails" -ne 0 ]; then
    echo "$fails case(s) failed"
    exit 1
fi
echo "all cases passed"
