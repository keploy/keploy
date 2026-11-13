#!/usr/bin/env bash
# Tests for fuzz-targets.sh. Run by linux-ci-scripts.yml, or locally (it
# needs only bash and awk, and leaves nothing behind):
#
#   bash .github/workflows/test_workflow_scripts/fuzz-targets.tests.sh
#
# Each case runs the script in a module of its own (go.keploy.io/server/v3,
# as the real one) with a stub `go` first on PATH that prints the case's
# canned `go test -list` output and exits with the case's code. So the cases
# check how the script reads that output; which targets a real listing finds
# under each OS's build tags is the nightly run's to show.
#
# A package's Fuzz names become entries with its import path and directory;
# Test, Benchmark and Example names, and packages with no test files, add
# nothing; a listing with no Fuzz name fails, and so does a listing that
# fails, with its output passed through.
set -u
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
script="$here/fuzz-targets.sh"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fails=0

mkdir -p "$tmp/bin" "$tmp/mod"
echo 'module go.keploy.io/server/v3' >"$tmp/mod/go.mod"
cat >"$tmp/bin/go" <<'EOF'
#!/usr/bin/env bash
# go test -list ...: prints $STUB_OUT and exits $STUB_RC.
printf '%s\n' "$STUB_OUT"
exit "${STUB_RC:-0}"
EOF
chmod +x "$tmp/bin/go"

# run_case NAME WANT_RC WANT_STDOUT WANT_STDERR_TEXT STUB_RC STUB_OUT: the
# script's stdout must be WANT_STDOUT exactly (when not empty) and its stderr
# must hold WANT_STDERR_TEXT (when not empty).
run_case() {
    local name=$1 want_rc=$2 want_out=$3 want_err=$4 stub_rc=$5 stub_out=$6 out err rc
    out=$(cd "$tmp/mod" && STUB_RC=$stub_rc STUB_OUT=$stub_out PATH="$tmp/bin:$PATH" \
        bash "$script" 2>"$tmp/stderr")
    rc=$?
    err=$(cat "$tmp/stderr")
    [ "$rc" -eq 0 ] || rc=1
    if [ "$rc" -ne "$want_rc" ] ||
        { [ -n "$want_out" ] && [ "$out" != "$want_out" ]; } ||
        { [ -n "$want_err" ] && [[ $err != *"$want_err"* ]]; }; then
        echo "FAIL $name: returned $rc, want $want_rc"
        echo "     stdout: $out"
        echo "     want:   $want_out"
        echo "     stderr: $err"
        echo "     want it to contain: $want_err"
        fails=$((fails + 1))
    else
        echo "ok   $name (rc $rc)"
    fi
}

http=go.keploy.io/server/v3/pkg/agent/proxy/integrations/http

run_case two-targets 0 \
    "[{\"pkg\":\"$http\",\"dir\":\"pkg/agent/proxy/integrations/http\",\"name\":\"FuzzRequestFraming\"},{\"pkg\":\"$http\",\"dir\":\"pkg/agent/proxy/integrations/http\",\"name\":\"FuzzResponseFraming\"}]" \
    '' 0 "FuzzRequestFraming
FuzzResponseFraming
ok  	$http	0.012s"

run_case only-fuzz-names 0 \
    '[{"pkg":"go.keploy.io/server/v3/pkg/models","dir":"pkg/models","name":"FuzzCallShape"}]' \
    '' 0 'TestCallShape
BenchmarkCallShape
ExampleCallShape
FuzzCallShape
ok  	go.keploy.io/server/v3/pkg/models	0.004s'

run_case no-test-files-ignored 0 \
    '[{"pkg":"go.keploy.io/server/v3/utils/log","dir":"utils/log","name":"FuzzANSI"}]' \
    '' 0 '?   	go.keploy.io/server/v3/pkg/secretname	[no test files]
FuzzANSI
ok  	go.keploy.io/server/v3/utils/log	0.003s
?   	go.keploy.io/server/v3/pkg/agent/listeners	[no test files]'

run_case no-targets 1 '' 'found no Fuzz targets' 0 \
    'ok  	go.keploy.io/server/v3/pkg/models	0.004s'

run_case listing-fails 1 '' 'pkg/models/callshape_test.go:12:2: undefined: callShape' 1 \
    '# go.keploy.io/server/v3/pkg/models [go.keploy.io/server/v3/pkg/models.test]
pkg/models/callshape_test.go:12:2: undefined: callShape
FAIL	go.keploy.io/server/v3/pkg/models [build failed]'

if [ "$fails" -gt 0 ]; then
    echo "$fails case(s) failed"
    exit 1
fi
echo "all cases passed"
