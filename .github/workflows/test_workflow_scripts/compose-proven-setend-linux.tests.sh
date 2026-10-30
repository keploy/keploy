#!/usr/bin/env bash
# Tests for compose-proven-setend-linux.sh, the lane "proven set-end stop
# (docker_compose)". Needs bash, python3, coreutils and procps (pgrep -P); no
# Docker, no keploy, no network, no sudo. Run by linux-ci-scripts.yml, or
# locally:
#
#   bash .github/workflows/test_workflow_scripts/compose-proven-setend-linux.tests.sh
#
# Each case runs the lane's record() as the lane defines it (its functions,
# taken from the lane script), in a scratch directory, with fakes on PATH for
# sudo, docker and curl, and a fake RECORD_BIN that says the app listens, and
# runs until it is interrupted, as keploy record does. On the interrupt it
# says how many test cases it had written, and exits.
#
# A hang of record() is the bug these cases are for: timeout(1) bounds each
# case at 60 s, so a case that hangs fails instead.
#
# a request of the recording fails
#   curl fails each request (exit 56, as a connection reset ends it). record()
#   has to say which request failed, stop keploy record, print its log and
#   return, not wait for test cases the failed requests never made.
# every request succeeds, its test case written as it is answered
#   record() has to stop keploy record and find both test cases.
# every request succeeds, its test case written as keploy record stops
#   keploy record's stop writes what it still holds. record() has to count
#   the test cases once keploy record has exited, not before. A record()
#   that waits for them while keploy record runs hangs here.
# keploy record drops a test case of a request that succeeded
#   It writes one of the two. record() has to say it wrote 1 of 2, print its
#   log and return. A record() that waits for the second while keploy record
#   runs on hangs: the lane held until its job was cancelled, with the
#   recording's log never printed.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
lane="$here/compose-proven-setend-linux.sh"

root="$(mktemp -d "${TMPDIR:-/tmp}/compose-setend-tests.XXXXXX")"
trap 'rm -rf "$root"' EXIT

failures=0
fail_case() {
    echo "FAIL: $1"
    failures=$((failures + 1))
}

# The lane's functions, as the lane defines them: every one, so record() finds
# whatever it calls, as in the lane.
funcs="$root/funcs.sh"
# A function is its "name() {" line to its first "}" line; a one-line
# function, "name() { ...; }", is its line alone.
awk -v names='^[A-Za-z_][A-Za-z0-9_]*\\(\\) \\{' '
    !in_fn && $0 ~ names { print; if ($0 !~ /\}[[:space:]]*$/) in_fn = 1; next }
    in_fn { print; if ($0 ~ /^\}/) in_fn = 0 }
' "$lane" >"$funcs"
for f in fail await test_files signal_keploy no_orphans cleanup record; do
    grep -q "^$f() {" "$funcs" || { echo "FAIL: the lane defines no $f()"; exit 1; }
done

mkdir -p "$root/bin" "$root/ws"
# sudo: runs its command as a child, as sudo runs keploy (signal_keploy
# signals sudo's child), without -E.
cat >"$root/bin/sudo" <<'SH'
#!/bin/bash
[ "${1:-}" = -E ] && shift
"$@" &
child=$!
wait "$child"
SH
# docker: a project with no container and no network left.
printf '#!/bin/bash\nexit 0\n' >"$root/bin/docker"
# curl: every request reset (CURL=fail), or answered (CURL=ok), each answer
# noted; with WRITE=run, keploy record's test case of it is written as it is
# answered.
cat >"$root/bin/curl" <<'SH'
#!/bin/bash
if [ "${CURL:-}" = ok ]; then
    echo "$*" >>"$ROOT/answered"
    if [ "${WRITE:-}" = run ]; then
        mkdir -p keploy/test-set-0/tests
        touch "keploy/test-set-0/tests/test-$(wc -l <"$ROOT/answered").yaml"
    fi
    exit 0
fi
echo "curl: (56) Recv failure: Connection reset by peer" >&2
exit 56
SH
# keploy record: the app listens; on keploy's interrupt the app publishes its
# shutdown, keploy record writes what it still holds (WRITE=stop: a test case
# for each request answered, but the last where DROP=1), says how many test
# cases it had written, and exits. Python, as a background job's SIGINT,
# ignored on entry, cannot be trapped in bash; the handler is in place before
# the app is said to listen.
cat >"$root/bin/record" <<'PY'
#!/usr/bin/env python3
import os, signal, sys
tests = "keploy/test-set-0/tests"
def written():
    return len([f for f in os.listdir(tests) if f.endswith(".yaml")]) if os.path.isdir(tests) else 0
def bye(*_):
    print("APP SHUTDOWN PUBLISHED bye", flush=True)
    if os.environ.get("WRITE") == "stop":
        answered = os.path.join(os.environ["ROOT"], "answered")
        n = sum(1 for _ in open(answered)) if os.path.exists(answered) else 0
        os.makedirs(tests, exist_ok=True)
        for i in range(n - int(os.environ.get("DROP", "0"))):
            open(os.path.join(tests, "test-%d.yaml" % (i + 1)), "w").close()
    print("RECORDED %d" % written(), flush=True)
    sys.exit(0)
signal.signal(signal.SIGINT, bye)
print("APP LISTENING", flush=True)
while True:
    signal.pause()
PY
chmod +x "$root/bin/"*

# run_record CASE CURL WRITE DROP: record 0 in a fresh workspace, curl
# failing or answering, keploy record writing its test cases as the requests
# are answered or as it stops, all or all but the last; its output is in
# $root/CASE.out, keploy record's log in $root/CASE.log.
run_record() {
    local rc
    rm -rf "$root/ws" "$root/answered"
    mkdir -p "$root/ws"
    (
        cd "$root/ws" &&
            PATH="$root/bin:$PATH" ROOT="$root" CURL="$2" WRITE="$3" DROP="$4" FUNCS="$funcs" RECORD_BIN="$root/bin/record" PORT=1 KNAME=app PROJECT=setend-tests \
                timeout 60 bash -c 'FAIL=0; . "$FUNCS"; record 0; echo "RECORD RETURNED FAIL=$FAIL"'
    ) >"$root/$1.out" 2>&1
    rc=$?
    cp "$root/ws/record0.log" "$root/$1.log" 2>/dev/null
    [ "$rc" -ne 124 ] || fail_case "$1: record() did not return"
}

run_record failed-request fail stop 0
out="$root/failed-request.out"
grep -q "FAIL: record 0: GET /price/r0a failed" "$out" || fail_case "a request of the recording fails: record() did not say which request failed"
grep -q "^RECORD RETURNED FAIL=[1-9]" "$out" || fail_case "a request of the recording fails: record() did not return with the failure counted"
grep -q "^APP LISTENING" "$out" || fail_case "a request of the recording fails: record() did not print keploy record's log"

for write in run stop; do
    run_record "answered-$write" ok "$write" 0
    grep -q "^RECORD RETURNED FAIL=0" "$root/answered-$write.out" || fail_case "every request succeeds, its test cases written at $write: record() failed"
    grep -q "^RECORDED 2" "$root/answered-$write.log" || fail_case "every request succeeds, its test cases written at $write: keploy record had not written both"
done

run_record dropped ok stop 1
out="$root/dropped.out"
grep -q "FAIL: record 0: keploy record wrote 1 test cases, want 2" "$out" || fail_case "keploy record drops a test case: record() did not say it wrote 1 of 2"
grep -q "^RECORD RETURNED FAIL=[1-9]" "$out" || fail_case "keploy record drops a test case: record() did not return with the failure counted"
grep -q "^RECORDED 1" "$out" || fail_case "keploy record drops a test case: record() did not print keploy record's log"

if [ "$failures" -ne 0 ]; then
    for f in "$root"/*.out "$root"/*.log; do
        echo "--- $f:"
        cat "$f" 2>/dev/null
    done
    exit 1
fi
echo "PASS: compose-proven-setend-linux.sh's record() cases"
