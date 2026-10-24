#!/usr/bin/env bash
# Tests for golang-linux.sh, the lane "keploy test holds no test set in
# memory". Needs bash, python3 and coreutils; no Docker, no keploy, no
# network, no sudo. Run by linux-ci-scripts.yml, or locally:
#
#   bash .github/workflows/test_workflow_scripts/golang/client_mock_memory/golang-linux.tests.sh
#
# Each case runs the lane script the way the workflow does (sourced from
# samples-go/http-pokeapi under `bash --noprofile --norc -eo pipefail`, as
# GitHub's `shell: bash` runs a step) in a scratch workspace, with fakes on
# PATH for go, sudo, curl and pgrep, and fake RECORD_BIN and REPLAY_BIN. The
# fake record writes a one-mock test set and waits to be killed, as keploy
# record does; the lane adds INFLATE_MB=4 mocks to it.
#
# keploy test exits 2
#   The fake replay prints a line and exits 2, as keploy test does when it
#   panics. The lane has to print that line (keploy test's log), say in an
#   ::error:: line that keploy test exited 2, and fail. Before, `wait` handed
#   the 2 to `bash -e`, which ended the step with it before the log was
#   printed: the lane said only "Process completed with exit code 2"
#   (job 120765033746).
# keploy test ends while its peak RSS is read
#   The shell reaps keploy test as soon as it exits, so its
#   /proc/<pid>/status can vanish between the loop's `kill -0` and its read
#   of the file, and awk then exits 2. The fake awk fails that way on every
#   read of a /proc/<pid>/status (and runs the real awk otherwise); the fake
#   replay passes every set. The lane has to pass. Before, the failed read
#   ended the step with exit 2, with nothing printed.
# a report with no status line
#   The fake replay exits 0 and writes a test-set report that has no status
#   line. The lane has to fail with an ::error:: line naming that report.
#   Before, the grep pipeline that read the status failed under pipefail and
#   ended the step with exit 1, with nothing said.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
lane="$here/golang-linux.sh"
repo="$(cd "$here/../../../../.." && pwd)"
real_awk="$(command -v awk)"

root="$(mktemp -d "${TMPDIR:-/tmp}/client-mock-memory-tests.XXXXXX")"
trap 'rm -rf "$root"' EXIT

failures=0
fail() {
    echo "FAIL: $1"
    failures=$((failures + 1))
}

# A scratch workspace for one case: $1/ws is GITHUB_WORKSPACE (its .github
# is this repository's), $1/bin the fakes, $1/home HOME.
setup() {
    local w="$1"
    mkdir -p "$w/ws/samples-go/http-pokeapi" "$w/bin" "$w/home" "$w/tmp"
    ln -s "$repo/.github" "$w/ws/.github"

    cat >"$w/bin/go" <<'EOF'
#!/bin/bash
# go build -o NAME: an executable NAME.
while [ $# -gt 0 ]; do
    if [ "$1" = "-o" ]; then printf '#!/bin/true\n' >"$2"; chmod +x "$2"; fi
    shift
done
EOF
    cat >"$w/bin/sudo" <<'EOF'
#!/bin/bash
exec "$@"
EOF
    # The app answers once the fake record has started it.
    cat >"$w/bin/curl" <<EOF
#!/bin/bash
[ -s "$w/record.pid" ] || exit 7
echo '{"location":["canalave-city-area","eterna-city-area"]}'
EOF
    cat >"$w/bin/pgrep" <<EOF
#!/bin/bash
cat "$w/record.pid"
EOF
    cat >"$w/record" <<EOF
#!/bin/bash
# keploy record: one test set of one mock, then wait to be killed.
mkdir -p keploy/test-set-0/tests
cat >keploy/test-set-0/mocks.yaml <<'YAML'
version: api.keploy.io/v1beta1
kind: Http
name: mock-0
spec:
    metadata:
        name: Http
YAML
echo "recorded a test set"
trap 'kill "\$sleeper" 2>/dev/null; exit 0' TERM
sleep 600 &
sleeper=\$!
echo \$\$ >"$w/record.pid"
wait "\$sleeper"
EOF
    chmod +x "$w/bin/"* "$w/record"
}

# run_lane DIR: the lane against DIR's fakes; its output in DIR/out, its exit
# code in DIR/rc. It runs in a session of its own, which is killed after it,
# so a case that fails part-way leaves no process behind.
run_lane() {
    local w="$1" rc=0
    (
        cd "$w/ws/samples-go/http-pokeapi" &&
            exec setsid -w env HOME="$w/home" TMPDIR="$w/tmp" PATH="$w/bin:$PATH" \
                GITHUB_WORKSPACE="$w/ws" RECORD_BIN="$w/record" REPLAY_BIN="$w/replay" \
                INFLATE_MB=4 \
                bash --noprofile --norc -eo pipefail -c 'source "$1"' lane "$lane"
    ) >"$w/out" 2>&1 &
    local pid=$!
    wait "$pid" || rc=$?
    kill -KILL -- "-$pid" 2>/dev/null || true
    echo "$rc" >"$w/rc"
}

show() {
    echo "--- lane output ($1) ---"
    cat "$1/out"
    echo "--- exit code $(cat "$1/rc") ---"
}

# keploy test exits 2.
w="$root/exit2"
setup "$w"
cat >"$w/replay" <<'EOF'
#!/bin/bash
echo "fake keploy test: panic: the replay broke"
exit 2
EOF
chmod +x "$w/replay"
run_lane "$w"
ok=1
[ "$(cat "$w/rc")" != 0 ] || { fail "exit2: the lane passed although keploy test exited 2"; ok=0; }
grep -q '^fake keploy test: panic: the replay broke$' "$w/out" ||
    { fail "exit2: the lane did not print keploy test's log"; ok=0; }
grep -q '^::error::keploy test exited 2 ' "$w/out" ||
    { fail "exit2: the lane did not say that keploy test exited 2"; ok=0; }
[ "$ok" = 1 ] && echo "ok: keploy test exits 2" || show "$w"

# keploy test ends while its peak RSS is read.
w="$root/reaped"
setup "$w"
cat >"$w/bin/awk" <<EOF
#!/bin/bash
for a in "\$@"; do
    case "\$a" in
    /proc/*/status)
        echo "awk: fatal: cannot open file '\$a' for reading: No such file or directory" >&2
        exit 2
        ;;
    esac
done
exec "$real_awk" "\$@"
EOF
cat >"$w/replay" <<'EOF'
#!/bin/bash
# keploy test: every set passes, and every mock (the 4 added and the one
# recorded) was sent to the agent, each read back from the file.
mkdir -p keploy/reports/test-run-0
printf 'name: test-set-0-report\nstatus: PASSED\n' >keploy/reports/test-run-0/test-set-0-report.yaml
echo 'stored the mocks on the agent {"testSet":"test-set-0","mocks":5,"payloadsReadBack":5}'
# Long enough for the lane to read the status file at least once.
sleep 2
exit 0
EOF
chmod +x "$w/bin/awk" "$w/replay"
run_lane "$w"
ok=1
[ "$(cat "$w/rc")" = 0 ] || { fail "reaped: the lane failed (exit $(cat "$w/rc")) on a replay that passed"; ok=0; }
grep -q '^keploy test exited 0; ' "$w/out" ||
    { fail "reaped: the lane did not reach keploy test's exit code"; ok=0; }
grep -q '^✓ keploy test peaked at ' "$w/out" ||
    { fail "reaped: the lane did not end on its pass line"; ok=0; }
[ "$ok" = 1 ] && echo "ok: keploy test ends while its peak RSS is read" || show "$w"

# A report with no status line.
w="$root/nostatus"
setup "$w"
cat >"$w/replay" <<'EOF'
#!/bin/bash
mkdir -p keploy/reports/test-run-0
printf 'name: test-set-0-report\n' >keploy/reports/test-run-0/test-set-0-report.yaml
echo 'stored the mocks on the agent {"testSet":"test-set-0","mocks":5,"payloadsReadBack":5}'
exit 0
EOF
chmod +x "$w/replay"
run_lane "$w"
ok=1
[ "$(cat "$w/rc")" != 0 ] || { fail "nostatus: the lane passed a report with no status"; ok=0; }
grep -q '^::error::test-set-0-report.yaml is without a status ' "$w/out" ||
    { fail "nostatus: the lane did not name the report that has no status"; ok=0; }
[ "$ok" = 1 ] && echo "ok: a report with no status line" || show "$w"

if [ "$failures" -ne 0 ]; then
    echo "$failures check(s) failed"
    exit 1
fi
echo "all cases passed"
