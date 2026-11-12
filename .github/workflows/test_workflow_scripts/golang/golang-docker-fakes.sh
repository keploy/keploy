# shellcheck shell=bash
# The fakes and the case runner for the tests of the golang_docker.yml lane
# scripts: gin_mongo/golang-docker.tests.sh and echo_sql/golang-docker.tests.sh
# source this file after setting
#
#   LANE_SCRIPT  the lane script, relative to .github/workflows/test_workflow_scripts
#   LANE_APP     the samples-go directory its workflow step runs it from
#
# It needs bash, coreutils, awk, grep and jq; no Docker, no keploy, no
# network, no sudo.
#
# run_case runs the lane script as its workflow step does: sourced by
# `bash -e`, from samples-go/$LANE_APP of a scratch workspace, with fakes on
# PATH for docker, curl, sleep, pgrep and sudo, and one fake keploy as both
# RECORD_BIN and REPLAY_BIN. The fake's keploy record exits with the code a
# case gives it, its log the line a case gives it; its keploy test writes the
# test-set reports a case gives it into a new test-run directory, and exits
# with the code a case gives it.

set -u

fakes_here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$fakes_here/../../../.." && pwd)"
[ -f "$repo/.github/workflows/test_workflow_scripts/$LANE_SCRIPT" ] || {
    echo "no lane script at $repo/.github/workflows/test_workflow_scripts/$LANE_SCRIPT"
    exit 1
}

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
# No lane sends docker's output into a file: a docker command whose output
# is one has the lane's own, which a step gives as a pipe (run_case's
# header), and says so, for expect to fail the case on.
if [ -f /dev/stdout ]; then echo "fake docker: the lane's output is a file, not a pipe"; fi
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
# test-9, passed. FAKE_TEST_NORUN_<n>=1: the nth keploy test writes no test
# run. FAKE_TEST_ERROR_<n>=1: its log has an ERROR line;
# FAKE_TEST_RACE_<n>=1: a "WARNING: DATA RACE" line. FAKE_RECORD_SAYS_<n>: a
# line the nth keploy record's log has.
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
    says_var="FAKE_RECORD_SAYS_$n"
    if [ -n "${!says_var:-}" ]; then
        echo "${!says_var}"
    fi
    echo "fake keploy record $n log"
    rc_var="FAKE_RECORD_RC_$n"
    exit "${!rc_var:-0}"
    ;;
test)
    n=$(next test)
    norun_var="FAKE_TEST_NORUN_$n"
    rc_var="FAKE_TEST_RC_$n"
    error_var="FAKE_TEST_ERROR_$n"
    race_var="FAKE_TEST_RACE_$n"
    if [ "${!error_var:-0}" = 1 ]; then
        echo "ERROR fake keploy test $n error"
    fi
    if [ "${!race_var:-0}" = 1 ]; then
        echo "WARNING: DATA RACE in fake keploy test $n"
    fi
    if [ "${!norun_var:-0}" = 1 ]; then
        echo "fake keploy test $n log"
        exit "${!rc_var:-0}"
    fi
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
            if [ -n "$failed" ]; then
                tests="$tests"',{"kind":"Http","status":"FAILED","testCaseID":"'"$failed"'"}'
                tests="$tests"',{"kind":"Http","status":"PASSED","testCaseID":"test-9"}'
            fi
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
    exit "${!rc_var:-0}"
    ;;
esac
exit 0
EOF
    chmod +x "$bin"/*
}

# run_case NAME [VAR=VALUE...]: runs the lane script with the fakes, the
# case's settings in its environment; leaves its output in $out, its exit in
# $rc. The case's LANE_SHELL_OPTS (default "-e", the step's own) are the
# options of the bash that sources the lane script.
#
# The lane's output reaches $out through a pipe, as a workflow step's reaches
# the runner, never as the file itself. A lane prints a keploy log with cat
# while its background send_request still prints, and cat (GNU coreutils 9,
# uutils too) copies a file into a regular file without write(2), moving the
# file offset the two share without the lock write(2) takes: each overwrote
# the other's line (the record-fails cases lost the log's line). A pipe takes
# each write whole, and $out has one writer, the cat below, which ends only
# when the last writer, a background send_request too, has closed the pipe:
# expect reads all the lane printed.
run_case() {
    local name="$1" dir
    shift
    dir="$root/$name"
    mkdir -p "$dir/ws/samples-go/$LANE_APP" "$dir/home" "$dir/counters"
    ln -s "$repo/.github" "$dir/ws/.github"
    make_fakes "$dir"
    out="$dir/out.txt"
    (
        cd "$dir/ws/samples-go/$LANE_APP" &&
            env "$@" PATH="$dir/bin:$PATH" HOME="$dir/home" GITHUB_WORKSPACE="$dir/ws" \
                FAKE_DIR="$dir/counters" RECORD_BIN="$dir/bin/keploy" REPLAY_BIN="$dir/bin/keploy" \
                LANE_SCRIPT="$LANE_SCRIPT" \
                bash "$fakes_here/../hang_guard.sh" 60 bash ${LANE_SHELL_OPTS:--e} -c 'source "$GITHUB_WORKSPACE/.github/workflows/test_workflow_scripts/$LANE_SCRIPT"'
    ) 2>&1 | cat >"$out"
    rc=${PIPESTATUS[0]}
}

# expect CASE WANT_RC LINE... [-- NOT_LINE...]: the case's exit is WANT_RC,
# its output has each LINE and no NOT_LINE (fixed strings), no error bash
# raised in the lane script or a script it sources ("<script>: line N: ..."),
# and no fake docker's line that the lane's output was a file.
expect() {
    local name="$1" want_rc="$2" line bad=0 refute=0
    shift 2
    if [ "$rc" -ne "$want_rc" ]; then
        echo "FAIL $name: exit $rc, want $want_rc"
        bad=1
    fi
    if grep -E -- "\.sh: line [0-9]+: " "$out"; then
        echo "FAIL $name: bash raised an error in the lane script"
        bad=1
    fi
    if grep -qF -- "the lane's output is a file, not a pipe" "$out"; then
        echo "FAIL $name: the lane's output is a file, not a pipe as a step's"
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

# finish: the suite's verdict.
finish() {
    if [ "$fails" -ne 0 ]; then
        echo "$fails case(s) failed"
        exit 1
    fi
    echo "all cases passed"
}
